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
		{"kubectl get pods,secrets -n team", false}, {"kubectl get nodes -n team", false},
		// 可证明只读的组合按每条查询分别校验范围（方案 24 §3.2）。
		{"kubectl get pods -n team | head -n 1", true},
		{"kubectl get pods -n other | grep web", false},
		{"kubectl get pods -n team | grep web; kubectl get pods -n other | grep web", false},
		{"kubectl get pods -n team | grep web && kubectl delete pod p -n team", false},
		{"kubectl get pods -n team | grep web | awk '{print $1}'", false},
		// rollout status/history 是只读能力，范围层也必须按 read 放行（方案 24 §7.1）。
		{"kubectl rollout status deployment/web -n team", true},
		{"kubectl rollout history deployment/web -n team", true},
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

// TestPlatformSecretInfoUsesOrdinaryRead 锁方案 24 §4.2：Secret 元数据查询只需要
// 普通 read 权限，不要求 secret.read；没有 read 权限时不能枚举 Secret。
func TestPlatformSecretInfoUsesOrdinaryRead(t *testing.T) {
	access := &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{
		{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead}},
	}}
	scope := AccessScope{Policy: access.Coverage().Limit(access)}
	for _, tc := range []struct {
		input   string
		allowed bool
	}{
		{`{"namespace":"team"}`, true},
		{`{"namespace":"team","name":"db"}`, true},
		{`{"namespace":"other"}`, false},
	} {
		err := scope.Check("get_secret_info", json.RawMessage(tc.input))
		if (err == nil) != tc.allowed {
			t.Fatalf("%s allowed=%t err=%v", tc.input, tc.allowed, err)
		}
	}
	// 同一身份没有 secret.read，明文读取必须被拒绝。
	err := scope.Check("get_secret_value", json.RawMessage(`{"namespace":"team","name":"db","keys":["password"],"reason":"diagnose"}`))
	if err == nil {
		t.Fatal("只有 read 权限时不得读取 Secret 明文")
	}
}
