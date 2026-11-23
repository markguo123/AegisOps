package tools

import (
	"github.com/k8s-ai/k8s-ai/internal/authz"
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
		{"kubectl get pods -n team && kubectl get clusterrolebindings", false},
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
