package agent

import (
	"context"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/decision"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func TestQuestionResolution(t *testing.T) {
	for _, status := range []string{"selected", "text", "skipped", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			f := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("q", "ask_user_question", `{"question":"选择存储","options":[{"id":"a","label":"标准","impact":"一般负载"}]}`)), {Content: "完成"}}}
			a, _ := testAgent(t, f, nil, nil)
			if status != "unavailable" {
				a.Ask = func(_ context.Context, q decision.Question) (decision.Answer, error) {
					r := decision.Answer{ID: q.ID, SessionID: q.SessionID, TurnID: q.TurnID, Status: status}
					if status == "selected" {
						r.OptionID = "a"
					}
					if status == "text" {
						r.Text = "保留现状"
					}
					return r, nil
				}
			}
			if _, _, err := a.Chat(context.Background(), "处理"); err != nil {
				t.Fatal(err)
			}
			if a.decisionUnresolved != (status == "skipped" || status == "unavailable") {
				t.Fatalf("unresolved=%v", a.decisionUnresolved)
			}
		})
	}
}

func TestQuestionDiscardsSameBatchOperations(t *testing.T) {
	f := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("other", "missing_tool", `{}`), call("q", "ask_user_question", `{"question":"如何处理？","options":[]}`)), {Content: "尚未决定"}}}
	a, _ := testAgent(t, f, nil, nil)
	if _, _, err := a.Chat(context.Background(), "处理"); err != nil {
		t.Fatal(err)
	}
	for _, msg := range f.captured[1] {
		for _, c := range msg.ToolCalls {
			if c.Name == "missing_tool" {
				t.Fatal("dependent call retained")
			}
		}
	}
	if !a.decisionUnresolved {
		t.Fatal("unavailable question must block further work")
	}
}

func TestQuestionDoesNotGrantApproval(t *testing.T) {
	for _, status := range []string{"selected", "skipped", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			executed := 0
			ap := &stubApprover{choices: []string{ChoiceNo}}
			f := &fakeLLM{responses: []*llm.ChatResponse{
				toolCallResp(call("q", "ask_user_question", `{"question":"选哪个？","options":[{"id":"a","label":"A","impact":"变更"}]}`)),
				toolCallResp(call("w1", "write", `{}`), call("w2", "write", `{}`)), {Content: "未执行"}}}
			a, _ := testAgent(t, f, []tools.Tool{&stubTool{name: "write", executed: &executed}}, ap)
			if status != "unavailable" {
				a.Ask = func(_ context.Context, q decision.Question) (decision.Answer, error) {
					r := decision.Answer{ID: q.ID, SessionID: q.SessionID, TurnID: q.TurnID, Status: status}
					if status == "selected" {
						r.OptionID = "a"
					}
					return r, nil
				}
			}
			if _, _, err := a.Chat(context.Background(), "处理"); err != nil {
				t.Fatal(err)
			}
			if executed != 0 {
				t.Fatal("decision authorized write")
			}
			if status == "selected" && ap.calls == 0 {
				t.Fatal("write approval was bypassed")
			}
			if status != "selected" && ap.calls != 0 {
				t.Fatal("unresolved decision reached batch approval")
			}
		})
	}
}

func TestQuestionTimeoutAndCancel(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		f := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("q", "ask_user_question", `{"question":"如何处理？","options":[]}`)), {Content: "未决"}}}
		a, _ := testAgent(t, f, nil, nil)
		a.Ask = func(context.Context, decision.Question) (decision.Answer, error) { return decision.Answer{}, cause }
		if _, _, err := a.Chat(context.Background(), "处理"); err != nil {
			t.Fatal(err)
		}
		if !a.decisionUnresolved {
			t.Fatal("timeout/cancel must remain unresolved")
		}
	}
}
