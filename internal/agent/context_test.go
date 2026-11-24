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
		{Role: "user", Content: strings.Repeat("old evidence ", 8000) + "<model_only_sensitive_secret_values>sensitive-diagnostic-value</model_only_sensitive_secret_values>"},
		{Role: "user", Content: "diagnose prod/pod-0"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{call("latest", "get_pod", `{"namespace":"prod","name":"pod-0"}`)}},
		{Role: "tool", ToolCallID: "latest", Content: "latest evidence=e1"},
	}
	before := messages[2].Content
	remaining := 8
	got, usage, err := a.prepareContext(context.Background(), messages, nil, "diagnose prod/pod-0", &remaining, contextMeter{})
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
			messages := []llm.Message{{Role: "system", Content: "scope"}, {Role: "user", Content: strings.Repeat("historical evidence ", 10000)}, {Role: "user", Content: "current"}}
			remaining := 8
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if mode == "request-limit" {
				remaining = 0
			}
			_, _, err := a.prepareContext(ctx, messages, nil, "current", &remaining, contextMeter{})
			if err == nil {
				t.Fatal("expected controlled compaction failure")
			}
			if len(fl.captured) > 1 {
				t.Fatal("unexpected repeated failure requests")
			}
			if messages[1].Content != strings.Repeat("historical evidence ", 10000) {
				t.Fatal("lost original history")
			}
		})
	}
}

func TestRequestBudgetIncludesSchemaAndOutputReserve(t *testing.T) {
	a := New(Options{LLM: &fakeLLM{}, ContextWindow: 8192, OutputTokens: 1024})
	schemas := []llm.ToolSpec{{Name: "huge", Description: strings.Repeat("schema ", 8000)}}
	remaining := 8
	_, _, err := a.prepareContext(context.Background(), []llm.Message{{Role: "system", Content: "scope"}, {Role: "user", Content: "q"}}, schemas, "q", &remaining, contextMeter{})
	if err == nil || remaining != 8 {
		t.Fatal("schema overflow should fail without a model request")
	}
}

func TestMissingToolCompletesProtocolWithoutExecutingRemainder(t *testing.T) {
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{toolCallResp(call("c1", "missing", `{}`), call("c2", "read", `{}`)), {Content: "done"}}}
	a, _ := testAgent(t, fl, []tools.Tool{&stubTool{name: "read", readonly: true, executed: &executed}}, nil)
	if _, _, err := a.Chat(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if executed != 0 {
		t.Fatal("executed batch after missing tool failure")
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

func TestChat128KContinuesAcrossToolBatches(t *testing.T) {
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_logs", `{"pod":"pod-1"}`), call("c2", "get_logs", `{"pod":"pod-2"}`)),
		toolCallResp(call("c3", "get_logs", `{"pod":"pod-3"}`), call("c4", "get_logs", `{"pod":"pod-4"}`)),
		toolCallResp(call("c5", "get_logs", `{"pod":"pod-5"}`)),
		{Content: "已汇总五个 Pod 的日志", Usage: llm.TokenUsage{PromptTokens: 20000}},
	}}
	logText := strings.Repeat("容器启动失败 connection timeout\n", 200)
	a, _ := testAgent(t, fl, []tools.Tool{&stubTool{name: "get_logs", readonly: true, executed: &executed, result: &tools.ToolResult{Text: logText}}}, nil)
	a.ContextWindow, a.OutputTokens = 128000, 4096
	answer, _, err := a.Chat(context.Background(), "排查这些 Pod 的启动故障")
	if err != nil || answer != "已汇总五个 Pod 的日志" || executed != 5 || len(fl.captured) != 4 {
		t.Fatalf("answer=%q executed=%d requests=%d err=%v", answer, executed, len(fl.captured), err)
	}
	last := fl.captured[3]
	for _, m := range last {
		if m.Role == "tool" && m.Content != tools.WrapUntrusted(logText) {
			t.Fatalf("tool result was lost or truncated: %s", m.ToolCallID)
		}
	}
	if a.flowContextBudget.InputActual != 20000 || a.flowContextBudget.Window != 128000 {
		t.Fatalf("wrong gateway usage sample: %+v", a.flowContextBudget)
	}
	raw := EstimateRequestTokens(last, a.Tools.Schemas())
	if raw >= len(logText)*executed/2 {
		t.Fatalf("local estimate still resembles byte accounting: %d", raw)
	}
}

func TestCompactionFailureBelowHardLimitContinues(t *testing.T) {
	fl := &fakeLLM{errors: []error{errors.New("summary unavailable")}}
	messages := []llm.Message{{Role: "system", Content: "scope"}, {Role: "user", Content: strings.Repeat("historical observation ", 3600)}, {Role: "user", Content: "current"}}
	meter := contextMeter{}
	input := meter.estimate(messages, nil)
	a := New(Options{LLM: fl, ContextWindow: input*10/9 + 1024, OutputTokens: 1024})
	remaining := 8
	got, _, err := a.prepareContext(context.Background(), messages, nil, "current", &remaining, meter)
	if err != nil || len(fl.captured) != 1 || got[1].Content != messages[1].Content {
		t.Fatalf("failed optional compaction stopped a request that still fits: %v", err)
	}
}

type contextTestLLM func(llm.ChatRequest) (*llm.ChatResponse, error)

func (f contextTestLLM) Chat(_ context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	return f(req)
}

func TestChatCompactsAndRetriesOnGatewayContextRejection(t *testing.T) {
	executed, rejected, summaries := 0, 0, 0
	tool := &stubTool{name: "get_logs", readonly: true, executed: &executed,
		result: &tools.ToolResult{Text: strings.Repeat("日志发现容器连接超时\n", 250)}}
	a, _ := testAgent(t, &fakeLLM{}, []tools.Tool{tool}, nil)
	a.ContextWindow, a.OutputTokens = 128000, 4096
	a.LLM = contextTestLLM(func(req llm.ChatRequest) (*llm.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, compactionInstruction) {
			summaries++
			if len(req.Tools) != 0 {
				t.Fatal("compaction request exposed tools")
			}
			// The summary endpoint can also use a stricter tokenizer.
			if summaries == 1 {
				return nil, &llm.APIError{StatusCode: 400, Body: `{"code":"context_length_exceeded"}`}
			}
			return &llm.ChatResponse{Content: "容器连接超时，尚未确认根因"}, nil
		}
		if executed == 0 {
			return toolCallResp(call("c1", "get_logs", `{}`)), nil
		}
		if containsMessage(req.Messages, "日志发现容器连接超时") {
			rejected++
			return nil, &llm.APIError{StatusCode: 400, Body: `{"code":"context_length_exceeded"}`}
		}
		if !containsMessage(req.Messages, "尚未确认根因") || !containsMessage(req.Messages, "检查日志") {
			t.Fatal("gateway retry lost the compacted evidence or current question")
		}
		return &llm.ChatResponse{Content: "连接超时，需复核上下游状态"}, nil
	})
	answer, _, err := a.Chat(context.Background(), "检查日志")
	if err != nil || answer != "连接超时，需复核上下游状态" || executed != 1 || rejected != 1 || summaries < 2 {
		t.Fatalf("answer=%q executed=%d rejected=%d summaries=%d err=%v", answer, executed, rejected, summaries, err)
	}
}
