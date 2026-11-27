package k8s

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type pageStub struct {
	Reader
	options    []metav1.ListOptions
	namespaces []string
}

func (r *pageStub) ListResourcePage(_ context.Context, resource, namespace string, opts metav1.ListOptions) (runtime.Object, error) {
	r.options = append(r.options, opts)
	r.namespaces = append(r.namespaces, namespace)
	page := &corev1.PodList{Items: []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: namespace}}}}
	if opts.Continue == "" {
		page.Continue = "source-token"
	}
	return page, nil
}

func TestResourceCatalogPaginationBindsScopeAndSelectors(t *testing.T) {
	reader := &pageStub{}
	reg := tools.NewRegistry()
	if err := RegisterAllWithOptions(reg, reader, RegisterOptions{Cluster: "cluster-1", Disabled: map[string]bool{"get_deployment": true, "get_previous_logs": true}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"get_pod", "get_resources", "get_previous_logs", "get_deployment", "get_endpointslices"} {
		if _, ok := reg.Lookup(name); ok {
			t.Fatalf("removed tool %s remains callable", name)
		}
	}
	logs, ok := reg.Lookup("get_logs")
	if !ok {
		t.Fatal("current logs should remain available")
	}
	result, err := logs.Execute(context.Background(), json.RawMessage(`{"namespace":"team","pod":"app","instance":"previous"}`))
	if err != nil || result.ErrorCode != tools.ErrorUnsupported {
		t.Fatalf("disabled previous logs: %+v %v", result, err)
	}
	result, err = logs.Execute(context.Background(), json.RawMessage(`{"namespace":"team","pod":"app"}`))
	if err != nil || result.ErrorCode != tools.ErrorInvalidArgument {
		t.Fatalf("missing instance accepted: %+v %v", result, err)
	}

	query := map[string]any{"kind": "Pod", "namespace": "team", "labelSelector": "app=demo", "fieldSelector": "metadata.name=pod", "limit": 1}
	tool, ok := reg.Lookup("list_resources")
	if !ok {
		t.Fatal("list_resources unavailable")
	}
	execute := func() tools.ToolResult {
		t.Helper()
		raw, _ := json.Marshal(query)
		result, err := tool.Execute(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := execute().Data.(map[string]any)
	if len(reader.options) != 1 || reader.namespaces[0] != "team" || reader.options[0].Limit != 1 ||
		reader.options[0].LabelSelector != "app=demo" || reader.options[0].FieldSelector != "metadata.name=pod" {
		t.Fatalf("query did not reach the source: %+v", reader.options)
	}
	query["continue"] = first["continue"]
	query["namespace"] = "other"
	if result := execute(); result.ErrorCode != tools.ErrorInvalidArgument || len(reader.options) != 1 {
		t.Fatalf("cursor used outside its query: %+v", result)
	}
	query["namespace"] = "team"
	next := execute().Data.(map[string]any)
	if reader.options[1].Continue != "source-token" || next["hasMore"] != false {
		t.Fatalf("invalid second page: %+v", next)
	}

	get, ok := reg.Lookup("get_resource")
	if !ok {
		t.Fatal("get_resource unavailable")
	}
	result, err = get.Execute(context.Background(), json.RawMessage(`{"kind":"Deployment","namespace":"team","name":"app"}`))
	if err != nil || result.ErrorCode != tools.ErrorInvalidArgument {
		t.Fatalf("disabled get kind remained accessible: %+v %v", result, err)
	}
}

func TestRecentEventsSelectCurrentUIDBeforeLimit(t *testing.T) {
	now := time.Now().UTC()
	events := []corev1.Event{
		{ObjectMeta: metav1.ObjectMeta{Name: "old"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "pod", UID: "current"}, LastTimestamp: metav1.NewTime(now.Add(-time.Hour))},
		{ObjectMeta: metav1.ObjectMeta{Name: "previous"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "pod", UID: "previous"}, LastTimestamp: metav1.NewTime(now)},
		{ObjectMeta: metav1.ObjectMeta{Name: "new"}, InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "pod", UID: "current"}, Series: &corev1.EventSeries{LastObservedTime: metav1.NewMicroTime(now)}},
	}
	items, limited := recentEvents(events, eventFilter{kind: "Pod", name: "pod", uid: "current"}, 1)
	if len(items) != 1 || items[0]["eventName"] != "new" || !limited {
		t.Fatalf("items=%v limited=%v", items, limited)
	}
}

func TestSecretOperationMustBeExplicit(t *testing.T) {
	reader := &secretValueReaderStub{}
	tool := &secretValueTool{reader: reader}
	input := json.RawMessage(`{"namespace":"team","name":"app","keys":["token"],"reason":"check reference"}`)
	if _, err := tool.OperationFacts(input); err == nil {
		t.Fatal("preflight accepted missing operation")
	}
	result, err := tool.Execute(context.Background(), input)
	if err != nil || result.ErrorCode != tools.ErrorInvalidArgument || reader.name != "" {
		t.Fatalf("invalid call reached reader: %+v, %v", result, err)
	}
}

func TestCanonicalGetsPreserveScopedAuthorization(t *testing.T) {
	reader := &pageStub{}
	reg := tools.NewRegistry()
	if err := RegisterAll(reg, reader); err != nil {
		t.Fatal(err)
	}
	team := tools.AccessScope{Policy: &authz.EffectiveAccess{
		ClusterID: "cluster-1",
		Rules:     []authz.AccessRule{{ScopeType: "namespace", ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{"read"}}},
	}}
	for _, route := range []struct{ name, input string }{
		{"get_resource", `{"kind":"Pod","namespace":"team","name":"pod"}`},

		{"list_resources", `{"kind":"Pod","namespace":"team"}`},
	} {
		if err := reg.Authorize(team, route.name, json.RawMessage(route.input)); err != nil {
			t.Fatalf("%s denied: %v", route.name, err)
		}
		revoked := tools.AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "cluster-1"}}
		if err := reg.Authorize(revoked, route.name, json.RawMessage(route.input)); err == nil {
			t.Fatalf("%s permitted after revocation", route.name)
		}
	}
	if err := reg.Authorize(team, "get_resource", json.RawMessage(`{"kind":"Secret","namespace":"team","name":"secret"}`)); err == nil {
		t.Fatal("generic resource query admitted Secret")
	}
}

func TestResourceCatalogSchemasUseEffectiveAccess(t *testing.T) {
	reg := tools.NewRegistry()
	if err := RegisterAll(reg, &pageStub{}); err != nil {
		t.Fatal(err)
	}
	scope := &tools.AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "cluster",
		Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead}}},
	}}
	found := 0
	for _, spec := range reg.SchemasFor(scope) {
		if spec.Name == "get_logs" {
			t.Fatal("logs exposed without logs grant")
		}
		if spec.Name != "get_resource" && spec.Name != "list_resources" {
			continue
		}
		found++
		props := spec.Parameters.(map[string]any)["properties"].(map[string]any)
		kinds := props["kind"].(map[string]any)["enum"].([]string)
		hasPod := false
		for _, kind := range kinds {
			if kind == "Node" || kind == "Namespace" || kind == "PersistentVolume" {
				t.Fatalf("cluster kind advertised: %s", kind)
			}
			if kind == "Pod" {
				hasPod = true
			}
		}
		if !hasPod {
			t.Fatalf("%s missing authorized Pod kind", spec.Name)
		}
	}
	if found != 2 {
		t.Fatal("canonical read schemas missing")
	}
}
