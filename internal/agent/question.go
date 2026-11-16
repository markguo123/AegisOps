package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// Borrowed from DeepSeek Harness (MIT); see prompts/DSH-LICENSE and docs/CREDITS.md.
//
//go:embed prompts/user_question_guidance.txt
var questionGuidance string

type questionTool struct{ agent *Agent }

func (*questionTool) Name() string { return "ask_user_question" }
func (*questionTool) Description() string {
	return questionGuidance + "关键选择无法从用户输入或只读证据确定时提问。选项必须真实，不强制凑多个方案。用户已明确的信息不重复询问。不得索取 Secret。跳过、超时或不可用不是同意，必须停止依赖选择的操作。选择不代替具体写操作审批。"
}
func (*questionTool) IsReadOnly(json.RawMessage) bool      { return true }
func (*questionTool) Risk(json.RawMessage) model.RiskLevel { return model.RiskSafe }
func (*questionTool) InputSchema() any {
	return map[string]any{
		"type": "object", "required": []string{"question", "options"}, "additionalProperties": false,
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "minLength": 1, "maxLength": 1000},
			"options": map[string]any{"type": "array", "maxItems": 6, "items": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"id", "label", "impact"}, "properties": map[string]any{
				"id": map[string]any{"type": "string", "maxLength": 64}, "label": map[string]any{"type": "string", "maxLength": 120}, "impact": map[string]any{"type": "string", "maxLength": 500}, "recommended": map[string]any{"type": "boolean"},
			}}},
		}}
}
func (t *questionTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	a := t.agent
	a.decisionUnresolved = true
	var q decision.Question
	if err := json.Unmarshal(a.safeArguments(input), &q); err != nil {
		return tools.ResultError("invalid question"), nil
	}
	q.ID = newID()
	q.SessionID = a.Session.ID
	q.TurnID = a.turn.ID
	q.ExpiresAt = time.Now().Add(5 * time.Minute)
	p, err := decision.New(q)
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	defer p.Close()
	// 统一 Interaction 外壳：问题卡与审批卡同源展示，但选择不授予写权限。
	options := make([]sse.ChoiceOption, 0, len(q.Options))
	for _, o := range q.Options {
		options = append(options, sse.ChoiceOption{Label: o.Label, Value: o.ID, Impact: o.Impact, Recommended: o.Recommended})
	}
	interaction := &sse.Interaction{
		InteractionID: q.ID, TurnID: q.TurnID, Kind: sse.InteractionQuestion,
		Status: sse.InteractionPending, Prompt: q.Question, Options: options,
		ExpiresAt: &q.ExpiresAt,
	}
	a.flowInteraction(ctx, interaction)
	if a.QuestionEvent != nil {
		a.QuestionEvent("question.required", a.safeStructured(q))
	}
	a.audit(ctx, "decision.request", map[string]any{"question_id": q.ID, "turn_id": q.TurnID})
	answer := decision.Answer{ID: q.ID, SessionID: q.SessionID, TurnID: q.TurnID, Status: "unavailable"}
	if a.Ask != nil {
		wait, cancel := context.WithDeadline(ctx, q.ExpiresAt)
		defer cancel()
		_ = a.turn.Wait()
		a.flowTurnStatus(ctx, sse.TurnWaiting, sse.WaitQuestion, "", "")
		a.trace().TurnUpdated(ctx, a.turn)
		answer, err = a.Ask(wait, q)
		_ = a.turn.Resume()
		a.flowTurnStatus(ctx, sse.TurnRunning, "", sse.ActivityChecking, "")
		a.trace().TurnUpdated(ctx, a.turn)
		if err != nil {
			answer = decision.Answer{ID: q.ID, SessionID: q.SessionID, TurnID: q.TurnID, Status: "canceled"}
			if errors.Is(err, context.DeadlineExceeded) || wait.Err() == context.DeadlineExceeded {
				answer.Status = "timed_out"
			}
		} else if err = p.Submit(answer, time.Now()); err != nil {
			answer.Status = "invalid"
			answer.Text = ""
			answer.OptionID = ""
		}
	}
	// 交互终态与 Turn 状态分开表达：answered 永远不等于审批 approved。
	interaction.Status = interactionStatusOf(answer.Status)
	interaction.Answer = a.redactSensitive(answer.OptionID)
	if interaction.Answer == "" && answer.Status == "text" {
		interaction.Answer = a.redactSensitive(answer.Text)
	}
	a.flowInteraction(ctx, interaction)
	if a.QuestionEvent != nil {
		a.QuestionEvent("question.resolved", a.safeStructured(map[string]any{"question": q, "answer": answer}))
	}
	a.audit(ctx, "decision.response", map[string]any{"question_id": q.ID, "turn_id": q.TurnID, "status": answer.Status})
	a.decisionUnresolved = answer.Status != "selected" && answer.Status != "text"
	return tools.ToolResult{Data: map[string]any{"question": q, "answer": answer}, Text: "用户决策：" + answer.Status}, nil
}

// interactionStatusOf 把决策答案状态映射为公开交互状态。
func interactionStatusOf(status string) sse.InteractionStatus {
	switch status {
	case "selected", "text":
		return sse.InteractionAnswered
	case "skipped":
		return sse.InteractionSkipped
	case "timed_out":
		return sse.InteractionTimedOut
	case "canceled":
		return sse.InteractionCanceled
	default:
		return sse.InteractionInvalidated
	}
}
