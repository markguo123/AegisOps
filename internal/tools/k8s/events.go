package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/k8s-ai/k8s-ai/internal/scanner"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type eventsTool struct{ base }

func (t *eventsTool) Name() string { return "get_events" }
func (t *eventsTool) Description() string {
	return "List recent Kubernetes events by namespace, object kind, name, UID, type and time window. Use UID to restrict evidence to the current object instance."
}
func (t *eventsTool) InputSchema() any {
	return objSchema(map[string]any{
		"namespace":    strProp("Namespace"),
		"kind":         strProp("Involved object kind"),
		"name":         strProp("Involved object name"),
		"uid":          strProp("Involved object UID"),
		"type":         strProp("Event type, such as Warning or Normal"),
		"sinceSeconds": intProp("Maximum event age in seconds"),
		"limit":        intProp("Maximum results, default 20, maximum 200"),
	})
}
func (t *eventsTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input)
	if err != nil {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, err.Error()), nil
	}
	limit := 20
	if v := args["limit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "limit must be between 1 and 200"), nil
		}
		limit = n
	}
	now := time.Now().UTC()
	filter := eventFilter{kind: args["kind"], name: args["name"], uid: args["uid"], eventType: args["type"]}
	if v := args["sinceSeconds"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 || n > (1<<63-1)/int64(time.Second) {
			return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "invalid sinceSeconds"), nil
		}
		filter.since = now.Add(-time.Duration(n) * time.Second)
	}
	events, coverageLimited, err := eventSample(ctx, t.reader, args["namespace"], filter)
	if err != nil {
		return readError(err), nil
	}
	items, limited := recentEvents(events, filter, limit)
	data := map[string]any{
		"kind": "EventList", "namespace": args["namespace"], "count": len(items), "items": items,
		"limit": limit, "limited": limited, "collectedAt": now, "objectUID": filter.uid,
		"maySpanObjectInstances": filter.uid == "", "coverageLimited": coverageLimited, "partial": coverageLimited,
	}
	if !filter.since.IsZero() {
		data["since"] = filter.since
	}
	return tools.ToolResult{Data: data, Text: fmt.Sprintf("events=%d", len(items))}, nil
}

type eventFilter struct {
	kind, name, uid, eventType string
	since                      time.Time
}

// Limit event requests while keeping a deterministic recent subset of the pages observed.
func eventSample(ctx context.Context, reader Reader, namespace string, filter eventFilter) ([]corev1.Event, bool, error) {
	source, ok := reader.(resourcePageReader)
	if !ok {
		events, err := reader.ListEvents(ctx, namespace)
		return events, false, err
	}
	selector := fields.Set{}
	if filter.uid != "" {
		selector["involvedObject.uid"] = filter.uid
	}
	if filter.name != "" {
		selector["involvedObject.name"] = filter.name
	}
	opts := metav1.ListOptions{Limit: 200, FieldSelector: selector.AsSelector().String()}
	var events []corev1.Event
	for pageNumber := 0; pageNumber < 5; pageNumber++ {
		object, err := source.ListResourcePage(ctx, "/events", namespace, opts)
		if err != nil {
			return nil, false, err
		}
		page, ok := object.(*corev1.EventList)
		if !ok {
			return nil, false, fmt.Errorf("unexpected event list type %T", object)
		}
		events = append(events, page.Items...)
		if page.Continue == "" {
			return events, false, nil
		}
		opts.Continue = page.Continue
	}
	return events, true, nil
}

func recentEvents(events []corev1.Event, filter eventFilter, limit int) ([]map[string]any, bool) {
	matching := make([]corev1.Event, 0)
	for _, e := range events {
		obj := e.InvolvedObject
		if filter.kind != "" && !strings.EqualFold(obj.Kind, filter.kind) ||
			filter.name != "" && obj.Name != filter.name ||
			filter.uid != "" && string(obj.UID) != filter.uid ||
			filter.eventType != "" && !strings.EqualFold(e.Type, filter.eventType) ||
			scanner.EventLastObserved(e).Before(filter.since) {
			continue
		}
		matching = append(matching, e)
	}
	sort.SliceStable(matching, func(i, j int) bool {
		a, b := matching[i], matching[j]
		if at, bt := scanner.EventLastObserved(a), scanner.EventLastObserved(b); !at.Equal(bt) {
			return at.After(bt)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.UID < b.UID
	})
	limited := len(matching) > limit
	if limited {
		matching = matching[:limit]
	}
	items := make([]map[string]any, 0, len(matching))
	for _, e := range matching {
		items = append(items, summarizeEvent(e))
	}
	return items, limited
}

func readError(err error) tools.ToolResult {
	code := ""
	switch {
	case apierrors.IsForbidden(err):
		code = tools.ErrorRBACDenied
	case apierrors.IsUnauthorized(err):
		code = tools.ErrorAuthentication
	case apierrors.IsNotFound(err):
		code = tools.ErrorNotFound
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		code = tools.ErrorTimeout
	}
	if code == "" {
		return tools.ExecutionError(err)
	}
	return tools.ResultErrorWithCode(code, err.Error())
}
