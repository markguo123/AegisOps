package tools

import (
	"encoding/json"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"testing"
)

func TestPlatformKubectlUsesOnlyProvableTargets(t *testing.T) {
	scope := AccessScope{IsAdmin: true, Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{
		{ScopeType: "namespace", Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{"read", "operate"}},
	}}}
	for _, tc := range []struct {
		cmd     string
		allowed bool
	}{
		{"kubectl get pods -n team", true}, {"kubectl scale deployment/web --replicas=2 -n team", true},
		{"kubectl get pods -n other", false}, {"kubectl get pods -A", false}, {"kubectl logs p -n team", false},
		{"kubectl delete pod p -n team", false}, {"kubectl exec p -n team -- id", false}, {"kubectl get secrets -n team", false},
		{"kubectl get pods --kubeconfig=/tmp/other -n team", false}, {"kubectl get pods -n team --server=https://other", false},
		{"kubectl get pods,secrets -n team", false}, {"kubectl get nodes -n team", false}, {"kubectl get pods -n team | head -n 1", false},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			in, _ := json.Marshal(map[string]string{"command": tc.cmd})
			err := scope.Check("kubectl", in)
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}

func TestPlatformAdminExecAndSecretRequireDedicatedTool(t *testing.T) {
	access := &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{
		{ScopeType: authz.ScopeNamespace, Namespaces: []string{"*"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead, authz.ActionLogs, authz.ActionOperate, authz.ActionDestroy, authz.ActionExec, authz.ActionSecretRead}},
		{ScopeType: authz.ScopeCluster, ResourceProfile: "builtin_cluster", Actions: []string{authz.ActionRead, authz.ActionOperate, authz.ActionDestroy, authz.ActionShell}},
	}}
	scope := AccessScope{Policy: access.Coverage().Limit(access)}
	for _, tc := range []struct {
		name, input string
		allowed     bool
	}{
		{"kubectl", `{"command":"kubectl exec pod/p -n team -- id"}`, true},
		{"get_secret_value", `{"namespace":"team","name":"db","keys":["password"],"reason":"diagnose"}`, true},
		{"kubectl", `{"command":"kubectl get secret db -n team"}`, false},
	} {
		err := scope.Check(tc.name, json.RawMessage(tc.input))
		if (err == nil) != tc.allowed {
			t.Fatalf("%s %s allowed=%t err=%v", tc.name, tc.input, tc.allowed, err)
		}
	}
}
