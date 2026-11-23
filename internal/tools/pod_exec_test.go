package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	"github.com/k8s-ai/k8s-ai/internal/executor"
)

type noPermitRunner struct{}

func (noPermitRunner) PreparePodExec(ctx context.Context, _ cmdplan.PodExecRequest, _, _ string) (context.Context, error) {
	return ctx, nil
}
func (noPermitRunner) ApprovePodExec(ctx context.Context) (context.Context, error) { return ctx, nil }
func (noPermitRunner) ExecPod(context.Context, cmdplan.PodExecRequest) (executor.Result, error) {
	return executor.Result{}, fmt.Errorf("POLICY_DENIED: no human approval")
}
func TestExecPodScopeMatchesKubectl(t *testing.T) {
	for _, exec := range []bool{false, true} {
		actions := []string{authz.ActionRead, authz.ActionOperate}
		if exec {
			actions = append(actions, authz.ActionExec)
		}
		scope := AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: actions}}}}
		for _, ns := range []string{"team", "other"} {
			raw, _ := json.Marshal(cmdplan.PodExecRequest{Namespace: ns, Pod: "api", Argv: []string{"ls", "/app"}})
			command, _ := json.Marshal(map[string]string{"command": "kubectl exec api -n " + ns + " -- ls /app"})
			a, b := scope.Check("exec_pod", raw), scope.Check("kubectl", command)
			want := exec && ns == "team"
			if (a == nil) != want || (b == nil) != want {
				t.Fatalf("exec=%v ns=%s structured=%v kubectl=%v", exec, ns, a, b)
			}
		}
	}
}
func TestExecPodMetadataAndDirectExecute(t *testing.T) {
	tool := NewPodExecTool(noPermitRunner{})
	raw := json.RawMessage(`{"namespace":"team","pod":"api","argv":["id"]}`)
	if tool.IsReadOnly(raw) || !tool.(MandatoryHumanApproval).RequiresHumanApproval(raw) {
		t.Fatal("exec approval metadata is unsafe")
	}
	res, e := tool.Execute(context.Background(), raw)
	if e != nil || res.ErrorCode != ErrorPolicyDenied {
		t.Fatalf("%+v %v", res, e)
	}
}

func TestKubectlGlobalNamespaceExecAlwaysRequiresHuman(t *testing.T) {
	tool := &kubectlTool{runner: &fakeWriteRunner{}}
	for _, command := range []string{"kubectl -n team exec api -- id", "kubectl --namespace=team exec api -- ls /app"} {
		raw, _ := json.Marshal(map[string]string{"command": command})
		if !tool.RequiresHumanApproval(raw) {
			t.Fatalf("missing mandatory approval: %s", command)
		}
	}
}
func TestClusterMetadataScopeAndAuthCanI(t *testing.T) {
	scope := AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead}}}}}
	for _, cmd := range []string{"kubectl version", "kubectl api-resources", "kubectl auth can-i get pods -n team"} {
		raw, _ := json.Marshal(map[string]string{"command": cmd})
		if err := scope.Check("kubectl", raw); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
	}
}

func TestDiscoveryNeverGrantsUnsupportedResources(t *testing.T) {
	scope := AccessScope{RequestContext: context.Background(), Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead}}}}}
	for _, res := range []string{"pods", "widgets", "secrets"} {
		scope.ResolveResource = func(context.Context, string) (string, string, bool, error) { return "", res, false, nil }
		raw := json.RawMessage(`{"command":"kubectl get customalias -n team"}`)
		err := scope.Check("kubectl", raw)
		if (err == nil) != (res == "pods") {
			t.Fatalf("resource=%s: %v", res, err)
		}
	}
}
