package tools

import (
	"encoding/json"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"testing"
)

// Embedding supplies irrelevant execution methods: this test exercises only
// declaration-based authorization, including an entirely new tool name.
type declaredTestTool struct {
	Tool
	name string
}

func (t declaredTestTool) Name() string { return t.name }
func (t declaredTestTool) Capability() Capability {
	return Capability{Domain: "kubernetes", Resource: "/pods"}
}

type undeclaredTestTool struct{ Tool }

func (undeclaredTestTool) Name() string { return "missing_declaration" }
func TestNewToolUsesCommonAuthorizerWithoutNameMapping(t *testing.T) {
	registry := NewRegistry()
	registry.Register(declaredTestTool{name: "future_pod_reader"})
	registry.Register(undeclaredTestTool{})
	scope := AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "a", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{authz.ActionRead}}}}}
	if err := registry.Authorize(scope, "future_pod_reader", json.RawMessage(`{"namespace":"team"}`)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Authorize(scope, "future_pod_reader", json.RawMessage(`{"namespace":"other"}`)); err == nil {
		t.Fatal("scope bypass")
	}
	if err := registry.Authorize(scope, "missing_declaration", json.RawMessage(`{}`)); err == nil {
		t.Fatal("undeclared tool allowed")
	}
}
