package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func TestRequiredApprovalWithoutApproverNeverExecutes(t *testing.T) {
	for _, batch := range []bool{false, true} {
		count := 0
		reg := tools.NewRegistry()
		if err := reg.Register(&stubTool{name: "write_probe", executed: &count}); err != nil {
			t.Fatal(err)
		}
		a := New(Options{Tools: reg, Session: &session.Session{ID: "approval-unavailable"}, ApprovalPolicy: &approval.Policy{RequireApproval: true}})
		result := a.executeTool(context.Background(), call("probe", "write_probe", `{}`), batch)
		if count != 0 {
			t.Fatalf("batch=%v: unapproved tool executed %d times", batch, count)
		}
		if result.ErrorCode != tools.ErrorToolUnavailable {
			t.Fatalf("batch=%v: result=%+v", batch, result)
		}
	}
}

type execApprovalKey struct{}
type execApprovalRunner struct{ prepared, approved, executed int }

func (r *execApprovalRunner) PreparePodExec(ctx context.Context, _ cmdplan.PodExecRequest, user, session string) (context.Context, error) {
	if user != "user" || session != "exec-integration" {
		return ctx, fmt.Errorf("wrong actor binding")
	}
	r.prepared++
	return ctx, nil
}
func (r *execApprovalRunner) ApprovePodExec(ctx context.Context) (context.Context, error) {
	r.approved++
	return context.WithValue(ctx, execApprovalKey{}, true), nil
}
func (r *execApprovalRunner) ExecPod(ctx context.Context, _ cmdplan.PodExecRequest) (executor.Result, error) {
	if ctx.Value(execApprovalKey{}) != true {
		return executor.Result{}, fmt.Errorf("missing approval context")
	}
	r.executed++
	return executor.Result{Command: "exec_pod", Stdout: "uid=1000"}, nil
}
func TestExecPodAgentApprovalIntegration(t *testing.T) {
	for _, choice := range []string{ChoiceYes, ChoiceNo, "unavailable", ChoiceYesNoAskAgain} {
		t.Run(choice, func(t *testing.T) {
			runner := &execApprovalRunner{}
			reg := tools.NewRegistry()
			if e := reg.Register(tools.NewPodExecTool(runner)); e != nil {
				t.Fatal(e)
			}
			fl := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("exec-1", "exec_pod", `{"namespace":"team","pod":"api","argv":["ls","-R","/app"]}`)), {Content: "done"}}}
			sess := &session.Session{ID: "exec-integration", UserID: "session-owner"}
			opts := Options{LLM: fl, Tools: reg, Session: sess, TurnUserID: "user", ApprovalPolicy: &approval.Policy{SkipPermissions: true}}
			if choice != "unavailable" {
				opts.Approver = &stubApprover{choices: []string{choice}}
			}
			a := New(opts)
			if _, _, e := a.Chat(context.Background(), "查看容器身份"); e != nil {
				t.Fatal(e)
			}
			want := 0
			if choice == ChoiceYes {
				want = 1
			}
			if runner.executed != want || runner.approved != want {
				t.Fatalf("prepared=%d approved=%d executed=%d want=%d", runner.prepared, runner.approved, runner.executed, want)
			}
		})
	}
}

type mandatoryProbe struct{ *stubTool }

func (*mandatoryProbe) RequiresHumanApproval(json.RawMessage) bool { return true }
func TestMandatoryApprovalIgnoresSessionAndBatch(t *testing.T) {
	count := 0
	reg := tools.NewRegistry()
	tool := &mandatoryProbe{&stubTool{name: "exec_probe", executed: &count}}
	if err := reg.Register(tool); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "mandatory"}
	args := json.RawMessage(`{}`)
	sess.AddApproved(declineKey("exec_probe", args))
	approver := &stubApprover{choices: []string{ChoiceYes, ChoiceYes}}
	a := New(Options{Tools: reg, Session: sess, Approver: approver, ApprovalPolicy: &approval.Policy{SkipPermissions: true, RequireApproval: false}})
	a.turn = &session.Turn{ID: "mandatory-turn"}
	for i := 0; i < 2; i++ {
		result := a.executeTool(context.Background(), call("probe", "exec_probe", `{}`), true)
		if !tools.Present(result).Ok {
			t.Fatalf("%+v", result)
		}
	}
	if approver.calls != 2 || count != 2 {
		t.Fatalf("approval calls=%d executions=%d", approver.calls, count)
	}
}

func TestExecPodLowRiskDoesNotRequireApprover(t *testing.T) {
	runner := &execApprovalRunner{}
	registry := tools.NewRegistry()
	if err := registry.Register(tools.NewPodExecTool(runner)); err != nil {
		t.Fatal(err)
	}
	model := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("read", "exec_pod", `{"namespace":"team","pod":"api","argv":["id"]}`)), {Content: "done"}}}
	agent := New(Options{LLM: model, Tools: registry, Session: &session.Session{ID: "exec-integration"}, TurnUserID: "user"})
	if _, _, err := agent.Chat(context.Background(), "inspect identity"); err != nil {
		t.Fatal(err)
	}
	if runner.prepared != 1 || runner.executed != 1 {
		t.Fatalf("prepared=%d executed=%d", runner.prepared, runner.executed)
	}
}
