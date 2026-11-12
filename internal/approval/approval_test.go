package approval

import (
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/model"
)

// TestIsReadOnlyCommandShellConstructs 对应方案 24 §3.2：可证明只读的命令组合
// （管道、&&/||、顺序执行）自动执行；其余 shell 构造永不归类为只读。
func TestIsReadOnlyCommandShellConstructs(t *testing.T) {
	readOnlyCombos := []string{
		"kubectl get pods | grep crash",
		"kubectl get po -A | grep mysql",
		"kubectl get pods -n team-a | grep -E 'mysql|redis' | head -n 20",
		"kubectl get pods -n team-a -o json | jq -r '.items[].metadata.name'",
		"kubectl get pods && kubectl get nodes",
		"kubectl get pods; kubectl get nodes",
		"kubectl get pods -n a || kubectl get pods -n b",
	}
	for _, cmd := range readOnlyCombos {
		if !IsReadOnlyCommand(cmd) {
			t.Errorf("可证明只读的组合应自动执行: %q", cmd)
		}
		if got := ClassifyCommand(cmd); got != model.RiskSafe {
			t.Errorf("可证明只读的组合风险应为 SAFE: %q got %s", cmd, got)
		}
		if NeedsShellApproval(cmd) {
			t.Errorf("可证明只读的组合不应要求 shell 审批: %q", cmd)
		}
	}
	// 无法证明只读的 shell 构造永不自动执行。
	unproven := []string{
		"kubectl get pods > out.txt",
		"kubectl get pods $FOO",
		"cat <<'EOF' | kubectl apply -f -",
		"kubectl get pods 2>/dev/null || true",
		"kubectl get pods | awk '{print $1}'",
		"kubectl get pods -n a | grep x | sed -n '1p'",
	}
	for _, cmd := range unproven {
		if IsReadOnlyCommand(cmd) {
			t.Errorf("无法证明只读的 shell 构造不应为只读: %q", cmd)
		}
		if !NeedsShellApproval(cmd) {
			t.Errorf("无法证明只读的 shell 构造必须要求人工审批: %q", cmd)
		}
	}
	// 引号内的管道符号不是新增 shell 命令。
	if !IsReadOnlyCommand(`kubectl get pods -n "default"`) {
		t.Fatal("引号内不含 shell 语法，应为只读")
	}
	if !IsReadOnlyCommand("kubectl get pods -n team-a | grep -E 'mysql|redis'") {
		t.Fatal("引号内的管道符号不应让只读组合升级为审批")
	}
}

func TestIsForbiddenCommand(t *testing.T) {
	forbidden := []string{
		"kubectl get secret -n default",                          // get secret
		"kubectl get secrets",                                    // 复数
		"kubectl get secret/db-password -o yaml",                 // secret/name
		"kubectl describe secret db-password -n production",      // describe
		"kubectl delete secret db-password",                      // 写也拦（输出/审计不落敏感数据）
		"kubectl get --raw /api/v1/namespaces/default/secrets/x", // --raw 绕过
		"kubectl get --raw=/api/v1/pods",                         // --raw= 形式
		"kubectl get pods -n SECRET",                             // 大小写不敏感（命名空间极端例，宁严勿漏）
		`kubectl get sec"ret" -o yaml`,                           // 引号混淆 secret
		"kubectl get $(echo secret) -o yaml",                     // 命令替换混淆 secret
		`kubectl get '--raw' /api/v1/pods`,                       // 引号混淆 --raw
		// 三期 R4.8：创建 Secret 只能走 create_resource 的占位符通道；
		// kubectl create secret 会把明文写进命令行、审计与会话。
		"kubectl create secret generic db -n web --from-literal=password=x",
	}
	for _, cmd := range forbidden {
		if !IsForbiddenCommand(cmd) {
			t.Errorf("IsForbiddenCommand(%q) = false, want true", cmd)
		}
	}
	allowed := []string{
		"kubectl get pods -n default",
		"kubectl get deployment web -o yaml",
		"kubectl logs pod-1 -n default --tail=50",
		"kubectl describe pod web-0",
		"kubectl get configmap my-secret-config -n default", // 名字含 secret 但资源是 configmap
		"kubectl get pods -l app=secret-app",                // label 值含 secret
		"kubectl get pod my-secret-pod -n default",          // 资源名含 secret 但不是 secret 资源
		"kubectl top pods -n production",
		"kubectl delete pod web-0 -n default",
	}
	for _, cmd := range allowed {
		if IsForbiddenCommand(cmd) {
			t.Errorf("IsForbiddenCommand(%q) = true, want false", cmd)
		}
	}
}

func TestClassifyCommand(t *testing.T) {
	cases := []struct {
		cmd  string
		want model.RiskLevel
	}{
		{"kubectl get pods -n default", model.RiskSafe},
		{"kubectl -n default get pods", model.RiskSafe},
		{"kubectl --context prod get pods", model.RiskSafe},
		{"kubectl logs pod-1 -n default", model.RiskSafe},
		{"kubectl scale deployment/payment-api --replicas=5", model.RiskMedium},
		{"kubectl rollout restart deployment/payment-api", model.RiskMedium},
		{"kubectl rollout undo deployment/payment-api", model.RiskHigh},
		{"kubectl patch deployment/payment-api --patch '{}'", model.RiskHigh},
		{"kubectl delete pod web-0 -n default", model.RiskHigh},
		{"kubectl delete deployment/payment-api -n production", model.RiskCritical},
		{"kubectl delete pvc data -n production", model.RiskCritical},
		{"kubectl delete service web -n production", model.RiskCritical},
		{"kubectl delete -n production service web", model.RiskCritical},
		{"kubectl delete namespace production", model.RiskCritical},
		{"kubectl exec pod-1 -- ls", model.RiskHigh},
		{"kubectl edit deployment/payment-api", model.RiskCritical},
		// rollout history/status 只读。
		{"kubectl rollout history deployment/payment-api", model.RiskSafe},
		{"kubectl rollout status deployment/payment-api", model.RiskSafe},
		// --dry-run 的已知写动词降级为只读。
		{"kubectl apply -f x.yaml --dry-run=client", model.RiskSafe},
		{"kubectl scale deployment/api --replicas=3 --dry-run=server", model.RiskSafe},
		// delete --dry-run 同样不实际变更（delete 走特殊分支，需单独覆盖）。
		{"kubectl delete pod x --dry-run=client", model.RiskSafe},
		{"kubectl delete namespace production --dry-run=server", model.RiskSafe},
		// 扩充写动词分类。
		{"kubectl autoscale deployment api --min=2 --max=5", model.RiskMedium},
		{"kubectl expose deployment api --port=80", model.RiskMedium},
		{"kubectl taint node n key=value:NoSchedule", model.RiskMedium},
		{"kubectl certificate approve my-csr", model.RiskHigh},
	}
	for _, c := range cases {
		if got := ClassifyCommand(c.cmd); got != c.want {
			t.Errorf("%s: risk = %s, want %s", c.cmd, got, c.want)
		}
	}
}

func TestIsReadOnlyCommand(t *testing.T) {
	if !IsReadOnlyCommand("kubectl get pods") {
		t.Fatal("get 应为只读")
	}
	if !IsReadOnlyCommand("kubectl -n default get pods") {
		t.Fatal("动词前带 -n 的 get 应为只读")
	}
	if IsReadOnlyCommand("kubectl delete pod x") {
		t.Fatal("delete 不应为只读")
	}
	// rollout history/status 只读，无需审批。
	if !IsReadOnlyCommand("kubectl rollout history deployment/api") {
		t.Fatal("rollout history 应为只读")
	}
	if !IsReadOnlyCommand("kubectl rollout status deployment/api") {
		t.Fatal("rollout status 应为只读")
	}
	if IsReadOnlyCommand("kubectl rollout restart deployment/api") {
		t.Fatal("rollout restart 不应为只读")
	}
	// --dry-run 的写动词降级为只读。
	if !IsReadOnlyCommand("kubectl apply -f x.yaml --dry-run=client") {
		t.Fatal("apply --dry-run 应为只读")
	}
	if !IsReadOnlyCommand("kubectl delete pod x --dry-run=client") {
		t.Fatal("delete --dry-run 应为只读")
	}
}

func TestShellBypassNeverReadOnlyAndRiskNotSafe(t *testing.T) {
	bypass := []string{
		"kubectl get pods | kubectl delete pod x",
		"kubectl get pods | kubectl delete namespace prod",
		"kubectl get pods | xargs kubectl delete pod",
		`sh -c "kubectl delete pod x"`,
		`bash -c "kubectl delete deployment/app"`,
		"$(kubectl delete pod x)",
		"`kubectl delete pod x`",
		"echo PT94bmNsIH0= | base64 -d | sh",
		"kubectl get pods | base64 -d | sh",
		"kubectl get pods; kubectl delete deployment/app",
		"kubectl get pods && kubectl delete namespace prod",
	}
	for _, cmd := range bypass {
		if IsReadOnlyCommand(cmd) {
			t.Errorf("shell 绕过变体不应为只读: %q", cmd)
		}
		if got := ClassifyCommand(cmd); got == model.RiskSafe {
			t.Errorf("shell 绕过变体风险不应为 SAFE: %q", cmd)
		}
	}
}

func TestClassifyShellPipelineUsesMaxRisk(t *testing.T) {
	cases := []struct {
		cmd  string
		want model.RiskLevel
	}{
		{"kubectl get pods | kubectl delete namespace prod", model.RiskCritical},
		{"kubectl get pods | kubectl delete pod x", model.RiskHigh},
		{"kubectl get pods | kubectl delete pvc data", model.RiskCritical},
		{"kubectl get pods && kubectl delete service web", model.RiskCritical},
		{"kubectl get pods --dry-run=client | kubectl delete pod x --dry-run=client", model.RiskMedium},
		{"echo PT94bmNsIH0= | base64 -d | sh", model.RiskCritical},
		// 只读 kubectl + 未放行的过滤器：整体仍是 shell 命令，风险不低于 MEDIUM。
		{"kubectl get pods | awk '{print $1}'", model.RiskMedium},
	}
	for _, c := range cases {
		if got := ClassifyCommand(c.cmd); got != c.want {
			t.Errorf("%s: risk = %s, want %s", c.cmd, got, c.want)
		}
	}
}

func TestPolicyRequiresApproval(t *testing.T) {
	p := Policy{RequireApproval: true}
	if !p.RequiresApproval() {
		t.Fatal("默认应要求审批")
	}
	skip := Policy{RequireApproval: true, SkipPermissions: true}
	if skip.RequiresApproval() {
		t.Fatal("skip-permissions 应免审批")
	}
	off := Policy{}
	if off.RequiresApproval() {
		t.Fatal("require_approval=false 应免审批")
	}
}

func TestIsDenied(t *testing.T) {
	if !IsDenied([]string{"delete namespace"}, "kubectl delete namespace production") {
		t.Fatal("应命中 deny_ops")
	}
	if IsDenied(nil, "kubectl get pods") {
		t.Fatal("空 deny_ops 不应拦截")
	}
	if IsDenied([]string{"delete namespace"}, "kubectl get namespaces") {
		t.Fatal("不应误伤只读命令")
	}
}
