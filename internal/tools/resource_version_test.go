package tools

import (
	"context"
	"encoding/json"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"testing"
)

type versionTestRunner struct {
	fakeWriteRunner
	state executor.ObjectStatus
}

func (r *versionTestRunner) GetObjectStatus(context.Context, executor.ObjectRef) (executor.ObjectStatus, error) {
	return r.state, nil
}
func TestHighRiskConfirmationRejectsChangedResource(t *testing.T) {
	runner := &versionTestRunner{state: executor.ObjectStatus{Found: true, UID: "original", ResourceVersion: "1"}}
	tool := &kubectlTool{runner: runner}
	input := json.RawMessage(`{"command":"kubectl delete pvc data -n team"}`)
	if tool.Risk(input) != model.RiskHigh && tool.Risk(input) != model.RiskCritical {
		t.Fatal("expected high risk")
	}
	scope := &AccessScope{Policy: &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{{ScopeType: "namespace", ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{"read", "operate", "destroy"}}}}}
	validate, err := tool.PrepareExecution(context.Background(), input, scope)
	if err != nil {
		t.Fatal(err)
	}
	if err := validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	runner.state.ResourceVersion = "2"
	if validate(context.Background()) == nil {
		t.Fatal("changed object accepted")
	}
	runner.state.ResourceVersion = "1"
	runner.state.UID = "replacement"
	if validate(context.Background()) == nil {
		t.Fatal("replacement accepted")
	}
}
