package agent

import (
	"context"
	"encoding/json"
	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
	"testing"
)

type consentSecretTool struct{ stubTool }

func (*consentSecretTool) RequiresHumanApproval(json.RawMessage) bool { return true }
func (*consentSecretTool) Risk(json.RawMessage) model.RiskLevel       { return model.RiskHigh }
func TestSecretScopeAndPerCallConsent(t *testing.T) {
	for _, tc := range []struct {
		name, ns, choice          string
		executions, confirmations int
	}{
		{"allowed", "team", ChoiceYes, 2, 2}, {"denied", "other", ChoiceYes, 0, 0}, {"rejected", "team", ChoiceNo, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executed := 0
			reg := tools.NewRegistry()
			if err := reg.Register(&consentSecretTool{stubTool{name: "get_secret_value", executed: &executed}}); err != nil {
				t.Fatal(err)
			}
			args := `{"namespace":"` + tc.ns + `","name":"db","keys":["password"],"reason":"diagnose"}`
			fl := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("one", "get_secret_value", args)), toolCallResp(call("two", "get_secret_value", args)), {Content: "结束"}}}
			approver := &stubApprover{choices: []string{tc.choice, tc.choice}}
			sess := &session.Session{ID: "consent", Cluster: "c"}
			sess.AddApproved(declineKey("get_secret_value", json.RawMessage(args)))
			access := &authz.EffectiveAccess{ClusterID: "c", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: []string{authz.ActionRead}}}}
			ag := New(Options{LLM: fl, Tools: reg, Session: sess, Approver: approver, MaxIterations: 4, MaxToolCalls: 4, AccessScope: &tools.AccessScope{Policy: access, ClusterID: "c"}, ApprovalPolicy: &approval.Policy{SkipPermissions: true}})
			if _, _, err := ag.Chat(context.Background(), "检查 Secret"); err != nil {
				t.Fatal(err)
			}
			if executed != tc.executions || approver.calls != tc.confirmations {
				t.Fatalf("executed=%d confirmed=%d", executed, approver.calls)
			}
		})
	}
}
