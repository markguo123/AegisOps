package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type secretReadStubTool struct {
	plain string
	auto  bool
}

type captureTracer struct {
	turns []session.Turn
	steps []session.Step
}

func (c *captureTracer) TurnStarted(_ context.Context, turn *session.Turn) {
	c.turns = append(c.turns, *turn)
}
func (c *captureTracer) TurnUpdated(_ context.Context, turn *session.Turn) {
	c.turns = append(c.turns, *turn)
}
func (c *captureTracer) StepStarted(_ context.Context, step *session.Step) {
	c.steps = append(c.steps, *step)
}
func (c *captureTracer) StepUpdated(_ context.Context, step *session.Step) {
	c.steps = append(c.steps, *step)
}

func (s *secretReadStubTool) Name() string { return "get_secret_value" }
func (s *secretReadStubTool) Description() string {
	return "secret read stub"
}
func (s *secretReadStubTool) InputSchema() any { return map[string]any{"type": "object"} }
func (s *secretReadStubTool) Execute(context.Context, json.RawMessage) (tools.ToolResult, error) {
	return tools.ToolResult{
		Data: map[string]any{
			"kind":  "SecretValueResult",
			"items": []map[string]any{{"key": "password", "value": "[REDACTED]"}},
		},
		Text:            "secret value loaded",
		ModelOnly:       map[string]any{"password": s.plain},
		SensitiveValues: []string{s.plain},
	}, nil
}
func (s *secretReadStubTool) IsReadOnly(json.RawMessage) bool { return false }
func (s *secretReadStubTool) Risk(json.RawMessage) model.RiskLevel {
	return model.RiskHigh
}
func (s *secretReadStubTool) RequiresHumanApproval(json.RawMessage) bool { return !s.auto }

// SensitiveReadAuto 模拟 agent.secret.read_approval=auto 的显式敏感读取授权。
func (s *secretReadStubTool) SensitiveReadAuto(json.RawMessage) bool { return s.auto }

func TestSecretReadOnlyReachesCurrentModelAndIsRedactedEverywhereElse(t *testing.T) {
	const plain = "db-password-42"
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"verify login failure"}`)),
		{Content: "数据库连接失败，当前口令是 " + plain + "，建议更新。"},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "secret-read"}
	rec := &auditRecorder{}
	approver := &stubApprover{choices: []string{ChoiceYes}}
	a := New(Options{
		LLM:            fl,
		Tools:          reg,
		Session:        sess,
		Approver:       approver,
		Recorder:       rec,
		MaxIterations:  5,
		MaxToolCalls:   10,
		ApprovalPolicy: &approval.Policy{Mode: approval.ModeStrict, RequireApproval: true, SkipPermissions: true},
	})
	answer, _, err := a.Chat(context.Background(), "检查数据库 Secret")
	if err != nil {
		t.Fatal(err)
	}

	if len(fl.captured) < 2 || !containsMessage(fl.captured[1], plain) {
		t.Fatal("当前轮模型上下文应收到审批后的 Secret 原值")
	}
	if approver.calls != 1 {
		t.Fatalf("skip_permissions 不得绕过 Secret 审批，approver calls=%d", approver.calls)
	}
	if strings.Contains(answer, plain) {
		t.Fatalf("最终回答泄露 Secret: %q", answer)
	}
	if !strings.Contains(answer, "[REDACTED]") {
		t.Fatalf("最终回答应保留脱敏标记: %q", answer)
	}
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, plain) {
			t.Fatalf("会话消息泄露 Secret: %+v", m)
		}
	}
	if rec.leaks(plain) {
		t.Fatal("审计泄露 Secret")
	}
	for {
		select {
		case msg := <-a.Output:
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), plain) {
				t.Fatalf("Agent 输出事件泄露 Secret: %s", raw)
			}
		default:
			return
		}
	}
}

func TestSensitiveOutputSuppressesStreamAndRedactsReasoning(t *testing.T) {
	const plain = "stream-secret-99"
	a := New(Options{LLM: &typedStreamingLLM{
		events: []llm.StreamEvent{
			{Type: llm.StreamReasoningDelta, Text: "比较 " + plain},
			{Type: llm.StreamContentDelta, Text: "结论 " + plain},
		},
		resp: &llm.ChatResponse{Content: "结论 " + plain},
	}})
	a.recordSensitiveValues([]string{plain})
	if _, _, err := a.chatOnce(context.Background(), "native", llm.ChatRequest{}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case msg := <-a.Output:
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), plain) {
				t.Fatalf("敏感模式下流式事件仍泄露: %s", raw)
			}
		default:
			goto reasoning
		}
	}

reasoning:
	a.Session = &session.Session{ID: "reasoning"}
	a.persistRoundReasoning("分析 " + plain + " 的格式")
	if strings.Contains(a.Session.Messages[0].Content, plain) {
		t.Fatalf("持久化 reasoning 泄露 Secret: %q", a.Session.Messages[0].Content)
	}
}

func TestSensitiveValuesAreRedactedFromSubsequentToolArguments(t *testing.T) {
	const plain = "model-reused-secret-77"
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"diagnose"}`)),
		toolCallResp(call("c2", "echo_secret", `{"value":"`+plain+`"}`)),
		{Content: "诊断完成"},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&stubTool{name: "echo_secret", readonly: true, executed: &executed}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "secret-args"}
	rec := &auditRecorder{}
	a := New(Options{
		LLM: fl, Tools: reg, Session: sess,
		Approver: &stubApprover{choices: []string{ChoiceYes}},
		Recorder: rec, MaxIterations: 5, MaxToolCalls: 10,
	})
	if _, _, err := a.Chat(context.Background(), "检查 Secret"); err != nil {
		t.Fatal(err)
	}
	if executed != 1 {
		t.Fatalf("echo_secret executed=%d", executed)
	}
	if rec.leaks(plain) {
		t.Fatal("后续工具参数把 Secret 写进审计")
	}
	for _, m := range sess.Messages {
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), plain) {
			t.Fatalf("后续工具参数把 Secret 写进会话: %s", raw)
		}
	}
	for {
		select {
		case msg := <-a.Output:
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), plain) {
				t.Fatalf("后续工具参数把 Secret 送进输出事件: %s", raw)
			}
		default:
			return
		}
	}
}

func TestSensitiveValuesAreRedactedFromSubsequentToolOutput(t *testing.T) {
	const plain = "tool-output-secret-88"
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"diagnose"}`)),
		toolCallResp(call("c2", "echo_secret", `{}`)),
		{Content: "诊断完成"},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&stubTool{
		name: "echo_secret", readonly: true, executed: &executed,
		result: &tools.ToolResult{
			Text: "输出 " + plain,
			Data: map[string]any{"stdout": plain},
		},
	}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "secret-output"}
	rec := &auditRecorder{}
	a := New(Options{
		LLM: fl, Tools: reg, Session: sess,
		Approver: &stubApprover{choices: []string{ChoiceYes}},
		Recorder: rec, MaxIterations: 5, MaxToolCalls: 10,
	})
	if _, _, err := a.Chat(context.Background(), "检查 Secret"); err != nil {
		t.Fatal(err)
	}
	if len(fl.captured) < 3 || !containsMessage(fl.captured[2], plain) {
		t.Fatal("当前轮模型应能看到后续工具原始输出")
	}
	if rec.leaks(plain) {
		t.Fatal("后续工具输出把 Secret 写进审计")
	}
	for _, m := range sess.Messages {
		raw, _ := json.Marshal(m)
		if strings.Contains(string(raw), plain) {
			t.Fatalf("后续工具输出把 Secret 写进会话: %s", raw)
		}
	}
	for {
		select {
		case msg := <-a.Output:
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), plain) {
				t.Fatalf("后续工具输出把 Secret 送进输出事件: %s", raw)
			}
		default:
			return
		}
	}
}

func TestSensitiveValuesAreRedactedFromTracerSteps(t *testing.T) {
	const plain = "tracer-secret-55"
	executed := 0
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"diagnose"}`)),
		toolCallResp(call("c2", "echo_secret", `{"value":"`+plain+`"}`)),
		{Content: "结论 " + plain},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(&stubTool{
		name: "echo_secret", readonly: true, executed: &executed,
		result: &tools.ToolResult{Text: plain, Data: map[string]any{"stdout": plain}},
	}); err != nil {
		t.Fatal(err)
	}
	tracer := &captureTracer{}
	a := New(Options{
		LLM: fl, Tools: reg, Session: &session.Session{ID: "secret-tracer"},
		Approver: &stubApprover{choices: []string{ChoiceYes}},
		Tracer:   tracer, MaxIterations: 5, MaxToolCalls: 10,
	})
	if _, _, err := a.Chat(context.Background(), "检查 Secret"); err != nil {
		t.Fatal(err)
	}
	for _, step := range tracer.steps {
		raw, _ := json.Marshal(step)
		if strings.Contains(string(raw), plain) {
			t.Fatalf("Tracer Step 泄露 Secret: %s", raw)
		}
	}
	for _, turn := range tracer.turns {
		raw, _ := json.Marshal(turn)
		if strings.Contains(string(raw), plain) {
			t.Fatalf("Tracer Turn 泄露 Secret: %s", raw)
		}
	}
}

func containsMessage(msgs []llm.Message, needle string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, needle) {
			return true
		}
	}
	return false
}

// TestSecretReadApprovalAutoSkipsApproverKeepsModelOnly 覆盖方案 24 §7.2 的
// read_on_demand + auto：有权限且配置授权时不弹审批，明文仍只进入当前一次
// 模型请求，其余出口全部脱敏。
func TestSecretReadApprovalAutoSkipsApproverKeepsModelOnly(t *testing.T) {
	const plain = "auto-consent-secret-31"
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"verify login failure"}`)),
		{Content: "当前口令是 " + plain + "，建议更新。"},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain, auto: true}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "secret-auto"}
	rec := &auditRecorder{}
	approver := &stubApprover{choices: []string{ChoiceNo}}
	a := New(Options{
		LLM: fl, Tools: reg, Session: sess, Approver: approver, Recorder: rec,
		MaxIterations: 5, MaxToolCalls: 10,
		ApprovalPolicy: &approval.Policy{Mode: approval.ModeStrict, RequireApproval: true},
	})
	answer, _, err := a.Chat(context.Background(), "检查数据库 Secret")
	if err != nil {
		t.Fatal(err)
	}
	if approver.calls != 0 {
		t.Fatalf("read_approval=auto 不应逐次弹审批，approver calls=%d", approver.calls)
	}
	if len(fl.captured) < 2 || !containsMessage(fl.captured[1], plain) {
		t.Fatal("当前轮模型请求应收到配置授权后的 Secret 原值")
	}
	if strings.Contains(answer, plain) {
		t.Fatalf("最终回答泄露 Secret: %q", answer)
	}
	for _, m := range sess.Messages {
		if strings.Contains(m.Content, plain) {
			t.Fatalf("会话消息泄露 Secret: %+v", m)
		}
	}
	if rec.leaks(plain) {
		t.Fatal("审计泄露 Secret")
	}
	for {
		select {
		case msg := <-a.Output:
			raw, _ := json.Marshal(msg)
			if strings.Contains(string(raw), plain) {
				t.Fatalf("Agent 输出事件泄露 Secret: %s", raw)
			}
		default:
			return
		}
	}
}

// TestSecretReadAlwaysStillNeedsHuman 锁 always（默认）模式：
// 即使开 SkipPermissions，也必须有真人在场；被拒绝时明文不得进入模型。
func TestSecretReadAlwaysStillNeedsHuman(t *testing.T) {
	const plain = "always-mode-secret-12"
	fl := &fakeLLM{responses: []*llm.ChatResponse{
		toolCallResp(call("c1", "get_secret_value", `{"namespace":"prod","name":"db","keys":["password"],"reason":"diagnose"}`)),
		{Content: "无法读取，请人工确认"},
	}}
	reg := tools.NewRegistry()
	if err := reg.Register(&secretReadStubTool{plain: plain, auto: false}); err != nil {
		t.Fatal(err)
	}
	sess := &session.Session{ID: "secret-always"}
	approver := &stubApprover{choices: []string{ChoiceNo}}
	a := New(Options{
		LLM: fl, Tools: reg, Session: sess, Approver: approver,
		MaxIterations: 5, MaxToolCalls: 10,
		ApprovalPolicy: &approval.Policy{Mode: approval.ModeStrict, RequireApproval: true, SkipPermissions: true},
	})
	if _, _, err := a.Chat(context.Background(), "检查 Secret"); err != nil {
		t.Fatal(err)
	}
	if approver.calls != 1 {
		t.Fatalf("always 模式必须逐次审批，approver calls=%d", approver.calls)
	}
	if containsMessage(fl.captured[len(fl.captured)-1], plain) {
		t.Fatal("被拒绝的读取不得把明文送进模型")
	}
}
