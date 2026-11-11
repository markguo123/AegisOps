package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func TestReadRetryBudgetAndCanonicalArguments(t *testing.T) {
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("1", "get_pod", `{"name":"p","namespace":"n"}`)),
		toolCallResp(call("2", "get_pod", `{ "namespace":"n", "name":"p" }`)),
		toolCallResp(call("3", "get_pod", `{"name":"p","namespace":"n"}`)),
		{Content: "unavailable"},
	}}
	a, _ := testAgent(t, fl, []tools.Tool{&stubTool{name: "get_pod", readonly: true, executed: &executed, fail: errors.New("connection refused")}}, nil)
	if _, _, err := a.Chat(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if executed != 2 {
		t.Fatalf("read retry executions=%d, want 2", executed)
	}
}

func TestUnknownWriteCannotBeReplayedOrFollowedByAnotherWrite(t *testing.T) {
	first, second := 0, 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("1", "patch", `{}`)), toolCallResp(call("2", "scale", `{}`)), {Content: "verify manually"},
	}}
	a, _ := testAgent(t, fl, []tools.Tool{
		&stubTool{name: "patch", readonly: false, executed: &first, fail: context.DeadlineExceeded},
		&stubTool{name: "scale", readonly: false, executed: &second},
	}, &stubApprover{choices: []string{ChoiceYes, ChoiceYes}})
	if _, _, err := a.Chat(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("unknown write was followed by mutation: %d/%d", first, second)
	}
	if !containsMessage(fl.captured[1], "UNKNOWN") {
		t.Fatal("model did not receive reconciliation guidance")
	}
}

func TestPermissionDenialFeedbackNeverSuggestsRetry(t *testing.T) {
	text := failureFeedback(tools.ResultErrorWithCode(tools.ErrorRBACDenied, "forbidden"), true)
	if !strings.Contains(text, "do not retry") {
		t.Fatal("missing permission boundary feedback")
	}
}
