package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type podHistoryTool struct{ stubTool }

func (*podHistoryTool) Capability() tools.Capability {
	return tools.Capability{Domain: "kubernetes", Resource: "/pods", Action: authz.ActionRead}
}

func TestPodHistoryRestoresAuthorizedObservation(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(&podHistoryTool{stubTool: stubTool{name: "get_resource"}}); err != nil {
		t.Fatal(err)
	}
	access := &authz.EffectiveAccess{ClusterID: "cluster", Rules: []authz.AccessRule{{
		ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"},
		ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead},
	}}}
	a := &Agent{AccessScope: &tools.AccessScope{
		Policy: access,
	}, Tools: registry}
	a.turn = &session.Turn{ID: "previous-turn"}
	pod := a.podObservation(map[string]any{
		"kind": "Pod", "namespace": "team", "name": "worker", "uid": "uid",
		"phase": "Running", "ready": true, "restartCount": 2,
	})
	if pod == nil || pod.RestartCount != 2 || !pod.Ready || pod.TurnID != "previous-turn" {
		t.Fatalf("unexpected Pod observation: %+v", pod)
	}
	encoded, err := json.Marshal([]session.Message{
		{Name: "get_pod", Type: "tool-call-response", Content: "worker", PodObservation: pod},
		{Role: "user", Content: "I restarted it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var restored []session.Message
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	a.Session = &session.Session{ID: "session", Messages: restored}
	result, err := (&questionTool{agent: a}).Execute(context.Background(), json.RawMessage(
		`{"question":"请补充重启结果","options":[],"pod_followup":{"namespace":"team","name":"worker","evidence":"restart_result"}}`,
	))
	if err != nil || !strings.Contains(result.Text, "已记录") {
		t.Fatalf("failed to record follow-up: %+v %v", result, err)
	}
	restored = a.Session.Messages
	_, injected := a.podHistory(context.Background(), restored)
	if !strings.Contains(injected, `"restartCount":2`) || !strings.Contains(injected, `"waitingFor":"restart_result"`) {
		t.Fatalf("authorized snapshot missing: %q", injected)
	}
	a.AccessScope.Refresh = func(context.Context) (*tools.AccessScope, error) {
		return nil, errors.New("access revoked")
	}
	history, injected := a.podHistory(context.Background(), restored)
	if strings.Contains(injected, "worker") || len(history) != 1 || history[0].Role != "user" {
		t.Fatalf("revoked observation survived: %v %q", history, injected)
	}
}
