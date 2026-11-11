package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func TestCompactionPreservesTargetAndLatestToolBatchWithoutSecrets(t *testing.T) {
	fl := &fakeLLM{}
	for i := 0; i < 8; i++ {
		fl.responses = append(fl.responses, &llm.ChatResponse{Content: "prod/pod-0 evidence=e1; denied delete; unresolved cause", Usage: llm.TokenUsage{TotalTokens: 2}})
	}
	a := New(Options{LLM: fl, ContextWindow: 16384, OutputTokens: 1024})
	a.recordSensitiveValues([]string{"sensitive-diagnostic-value"})
	messages := []llm.Message{
		{Role: "system", Content: "Cluster B; authorized namespace prod; never bypass approval"},
		{Role: "user", Content: "old investigation"},
		{Role: "user", Content: strings.Repeat("old evidence ", 1400) + "<model_only_sensitive_secret_values>sensitive-diagnostic-value</model_only_sensitive_secret_values>"},
		{Role: "user", Content: "diagnose prod/pod-0"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("latest", "get_pod", `{"namespace":"prod","name":"pod-0"}`)}},
		{Role: "tool", ToolCallID: "latest", Content: "latest evidence=e1"},
	}
	before := messages[2].Content
	remaining := 8
	got, usage, err := a.prepareContext(context.Background(), messages, nil, "diagnose prod/pod-0", &remaining)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Content != messages[0].Content || got[1].Content != "diagnose prod/pod-0" {
		t.Fatal("lost system scope or current target")
	}
	if got[len(got)-2].ToolCalls[0].ID != "latest" || got[len(got)-1].ToolCallID != "latest" {
		t.Fatal("split latest tool batch")
	}
	if messages[2].Content != before {
		t.Fatal("mutated original history")
	}
	if usage.TotalTokens == 0 || remaining >= 8 {
		t.Fatal("compaction costs not measured")
	}
	for _, request := range fl.captured {
		if containsMessage(request, "sensitive-diagnostic-value") || containsMessage(request, "model_only_sensitive_secret_values") {
			t.Fatal("compaction received Secret channel")
		}
	}
	if EstimateRequestTokens(got, nil)+a.OutputTokens > a.ContextWindow {
		t.Fatal("compacted request exceeds window")
	}
}

func TestCompactionFailureIsBoundedAndDoesNotDiscardHistory(t *testing.T) {
	for _, mode := range []string{"empty", "tool-call", "error", "cancel", "request-limit"} {
		t.Run(mode, func(t *testing.T) {
			fl := &fakeLLM{responses: []*llm.ChatResponse{{}}}
			if mode == "tool-call" {
				fl.responses[0] = toolCallResp(call("bad", "delete_pod", `{}`))
			}
			if mode == "error" {
				fl.errors = []error{errors.New("provider failed")}
			}
			a := New(Options{LLM: fl, ContextWindow: 16384, OutputTokens: 1024})
			messages := []llm.Message{{Role: "system", Content: "scope"}, {Role: "user", Content: strings.Repeat("e", 18000)}, {Role: "user", Content: "current"}}
			remaining := 8
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if mode == "request-limit" {
				remaining = 0
			}
			_, _, err := a.prepareContext(ctx, messages, nil, "current", &remaining)
			if err == nil {
				t.Fatal("expected controlled compaction failure")
			}
			if len(fl.captured) > 1 {
				t.Fatal("unexpected repeated failure requests")
			}
			if len(messages[1].Content) != 18000 {
				t.Fatal("lost original history")
			}
		})
	}
}

func TestRequestBudgetIncludesSchemaAndOutputReserve(t *testing.T) {
	a := New(Options{LLM: &fakeLLM{}, ContextWindow: 8192, OutputTokens: 1024})
	schemas := []llm.ToolSpec{{Name: "huge", Description: strings.Repeat("schema", 1600)}}
	remaining := 8
	_, _, err := a.prepareContext(context.Background(), []llm.Message{{Role: "system", Content: "scope"}, {Role: "user", Content: "q"}}, schemas, "q", &remaining)
	if err == nil || remaining != 8 {
		t.Fatal("schema overflow should fail without a model request")
	}
}

func TestBatchFailureCompletesProtocolWithoutExecutingRemainder(t *testing.T) {
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("c1", "read", `{}`), call("c2", "read", `{}`)), {Content: "done"}}}
	a, _ := testAgent(t, fl, []tools.Tool{&stubTool{name: "read", readonly: true, executed: &executed}}, nil)
	a.TurnMaxBytes = 1
	if _, _, err := a.Chat(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if executed != 1 {
		t.Fatal("executed batch after budget failure")
	}
	last := fl.captured[1]
	if last[len(last)-1].ToolCallID != "c2" || !strings.Contains(last[len(last)-1].Content, "NOT EXECUTED") {
		t.Fatal("orphaned tool call or fabricated execution")
	}
}

func TestHistoryObservationsDoNotCreateOrphanedToolMessages(t *testing.T) {
	got := historyToMessages([]session.Message{{Role: "user", Content: "q"}, {Role: "tool", Type: "tool-call-response", ToolCallID: "old", Content: "evidence"}}, 20)
	if got[1].Role != "user" || !strings.Contains(got[1].Content, "untrusted") {
		t.Fatal("orphaned historical tool result")
	}
}
