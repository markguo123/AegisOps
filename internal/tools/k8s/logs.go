package k8s

import (
	"context"
	"encoding/json"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"strconv"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type logsTool struct {
	base
	previousDisabled bool
}

func (*logsTool) Name() string { return "get_logs" }
func (*logsTool) Description() string {
	return "Read current or previous container logs. Specify the instance explicitly."
}
func (t *logsTool) InputSchema() any {
	props := map[string]any{
		"namespace":    strProp("命名空间"),
		"pod":          strProp("Pod 名称"),
		"container":    strProp("容器名（可选，单容器 Pod 可省略）"),
		"tailLines":    intProp("日志尾部行数（默认 100）"),
		"sinceSeconds": intProp("仅取最近 N 秒的日志（可选）"),
	}
	instances := []string{"current"}
	if !t.previousDisabled {
		instances = append(instances, "previous")
	}
	props["instance"] = map[string]any{"type": "string", "enum": instances}
	return objSchema(props, "namespace", "pod", "instance")
}
func (t *logsTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "pod", "instance")
	if err != nil {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, err.Error()), nil
	}
	instance := args["instance"]
	if instance != "current" && instance != "previous" {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "instance must be current or previous"), nil
	}
	if instance == "previous" && t.previousDisabled {
		return tools.ResultErrorWithCode(tools.ErrorUnsupported, "log instance is disabled"), nil
	}
	tail := int64(100)
	if v := args["tailLines"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return tools.ResultError("invalid tailLines"), nil
		}
		tail = n
	}
	var since *int64
	if v := args["sinceSeconds"]; v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return tools.ResultError("invalid sinceSeconds"), nil
		}
		since = &n
	}
	observedAt := time.Now().UTC()
	const maxBytes = 64 * 1024
	options := model.LogOptions{
		Container:    args["container"],
		TailLines:    &tail,
		SinceSeconds: since,
		Previous:     instance == "previous",
		MaxBytes:     maxBytes,
		MaxLineBytes: 0,
	}
	var raw []byte
	var clipped bool
	if source, ok := t.reader.(interface {
		GetPodLogSample(context.Context, string, string, model.LogOptions) ([]byte, bool, error)
	}); ok {
		raw, clipped, err = source.GetPodLogSample(ctx, args["namespace"], args["pod"], options)
	} else {
		raw, err = t.reader.GetPodLogs(ctx, args["namespace"], args["pod"], args["container"], options)
		clipped = len(raw) >= maxBytes
	}
	if err != nil {
		if instance == "previous" && apierrors.IsBadRequest(err) &&
			strings.Contains(err.Error(), "previous terminated container") && strings.Contains(err.Error(), "not found") {
			return tools.ResultErrorWithCode("PREVIOUS_INSTANCE_NOT_FOUND", err.Error()), nil
		}
		return readError(err), nil
	}
	content := string(raw)
	lines := strings.Count(content, "\n")
	if content != "" && !strings.HasSuffix(content, "\n") {
		lines++
	}
	data := map[string]any{
		"namespace": args["namespace"], "pod": args["pod"], "container": args["container"],
		"previous": instance == "previous", "instance": instance, "truncated": clipped,
		"collectedAt": observedAt, "tailLines": tail, "sinceSeconds": since, "maxBytes": maxBytes,
		"returnedBytes": len(raw), "returnedLines": lines, "empty": len(raw) == 0,
	}
	if clipped {
		data["limitReached"] = "byte_budget"
	}
	return tools.ToolResult{Data: data, Text: content, Truncated: clipped}, nil
}
