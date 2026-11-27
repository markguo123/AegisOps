package tools

import (
	"encoding/json"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"net/http/httptest"
	"testing"
)

func TestPlatformCompositeCommandTargets(t *testing.T) {
	scope := AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{
		{ScopeType: "cluster", ResourceProfile: "builtin_cluster", Actions: []string{"read", "operate", "destroy", "shell"}},
		{ScopeType: "namespace", ResourceProfile: "builtin_namespaced", Namespaces: []string{"*"}, Actions: []string{"read", "logs", "operate", "destroy"}},
	}}}
	for _, tc := range []struct {
		cmd     string
		allowed bool
	}{
		{"kubectl get pods -n team | head -n 5", true},
		{"kubectl get pods -n team && kubectl rollout restart deployment/web -n team", true},
		{"kubectl delete pods p1 p2 -n team", true},
		{"kubectl get pods,services -n team", true},
		{"kubectl get pods,secrets -n team", false},
		{"kubectl get pods -n team > /tmp/report", false},
		{"kubectl get pods -n $(cat /tmp/ns)", false},
		{"kubectl get pods -n team | sh", false},
		{"kubectl get pods -n team && kubectl get clusterrolebindings", true},
		{"kubectl get pods -n team --context=other", false},
		{"kubectl apply -f - <<'YAML'\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: config\n  namespace: team\ndata:\n  greeting: hello\nYAML", true},
		{"kubectl apply -f - <<'YAML'\napiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRoleBinding\nmetadata:\n  name: escalate\nYAML", false},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			err := scope.commandPolicy(tc.cmd)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}

func TestClusterRoleBindingReadAuthorization(t *testing.T) {
	clusterRule := authz.AccessRule{ScopeType: "cluster", ResourceProfile: "builtin_cluster", Actions: []string{"read"}}
	namespaceRule := authz.AccessRule{ScopeType: "namespace", ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{"read"}}
	for _, tc := range []struct {
		name    string
		rules   []authz.AccessRule
		allowed bool
	}{
		{"cluster read", []authz.AccessRule{clusterRule}, true},
		{"namespace read", []authz.AccessRule{namespaceRule}, false},
		{"revoked", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: tc.rules}}
			resource := Capability{Domain: "kubernetes", Resource: "rbac.authorization.k8s.io/clusterrolebindings", Action: "read"}
			facts, err := resource.Facts(json.RawMessage(`{}`), scope)
			if err != nil {
				t.Fatal(err)
			}
			if err := scope.AuthorizeOperation(facts); (err == nil) != tc.allowed {
				t.Fatalf("structured preflight: %v", err)
			}
			if err := scope.commandPolicy("kubectl get clusterrolebindings"); (err == nil) != tc.allowed {
				t.Fatalf("command preflight: %v", err)
			}
			request := httptest.NewRequest("GET", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", nil)
			if err := scope.Policy.CheckRequest(request); (err == nil) != tc.allowed {
				t.Fatalf("request boundary: %v", err)
			}
		})
	}
}
