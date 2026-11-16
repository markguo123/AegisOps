package agent

import (
	"context"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// Flow 是 Agent 的语义输出出口（方案 25 §4）：Agent 在事件产生时绑定稳定的
// message/part/tool/interaction ID，service 负责安全处理、微批、持久化与提交后
// 广播。实现必须保证单会话有序；Emit 非阻塞失败语义由实现通过取消运行上下文
// 表达（输出不可靠时停止生成）。
type Flow interface {
	Emit(ctx context.Context, e sse.Event)
}

// TurnSnapshot 返回本轮 Turn 的副本（Chat 返回后由 service 提交终态使用）。
func (a *Agent) TurnSnapshot() *session.Turn {
	if a.turn == nil {
		return nil
	}
	cp := *a.turn
	return &cp
}

// FinalMessageID 返回本轮最终回答所属消息 ID（无最终回答时为空）。
func (a *Agent) FinalMessageID() string { return a.flowFinalMessageID }

// newApprovalInteraction 把审批计划映射成统一 Interaction 公开载荷。
func (a *Agent) newApprovalInteraction(plan tools.ApprovalPlan, toolCallID string) *sse.Interaction {
	in := &sse.Interaction{
		InteractionID: "int_" + newID(), TurnID: a.turn.ID, ToolCallID: toolCallID,
		Kind: sse.InteractionApproval, Status: sse.InteractionPending,
		Title: plan.Title, Tool: plan.Tool, Operation: plan.Operation, Risk: plan.Risk,
		Summary: plan.Summary, Command: plan.Command,
		Verification: append([]string(nil), plan.Verification...),
	}
	if plan.Target != (tools.ApprovalTarget{}) {
		in.Target = &sse.Target{
			Cluster: plan.Target.Cluster, Namespace: plan.Target.Namespace,
			Group: plan.Target.Group, Version: plan.Target.Version,
			Kind: plan.Target.Kind, Name: plan.Target.Name,
		}
	}
	for _, change := range plan.Changes {
		in.Changes = append(in.Changes, sse.Change{
			Path: change.Path, Operation: change.Operation, Before: change.Before,
			After: change.After, Description: change.Description,
		})
	}
	for _, check := range plan.Checks {
		in.Checks = append(in.Checks, sse.Check{Name: check.Name, Status: check.Status, Message: check.Message})
	}
	return in
}

// flowEmit 发送一条语义事件；未装配 Flow（CLI 直用/测试）时静默跳过。
func (a *Agent) flowEmit(ctx context.Context, e sse.Event) {
	if a.Flow == nil || a.Session == nil {
		return
	}
	a.Flow.Emit(ctx, e)
}

// flowTurnStatus 发布完整 Turn 状态。状态是 UI 判断运行的唯一依据；
// waiting 与 done/failed 由同一事件类型承载，不再借 turn.completed 表示等待。
func (a *Agent) flowTurnStatus(ctx context.Context, status sse.TurnStatus, reason sse.WaitReason, activity sse.Activity, finalMessageID string) {
	if a.turn == nil {
		return
	}
	data := sse.TurnStatusData{
		TurnID: a.turn.ID, SessionID: a.turn.SessionID,
		Status: status, WaitReason: reason, Activity: activity,
		Input: a.turn.Input, Error: a.turn.Error,
		StartedAt: nilIfZero(a.turn.StartedAt), FinishedAt: a.turn.FinishedAt,
		FinalMessageID: finalMessageID,
		Usage: sse.TokenUsage{
			PromptTokens:     a.turn.Token.PromptTokens,
			CompletionTokens: a.turn.Token.CompletionTokens,
			TotalTokens:      a.turn.Token.TotalTokens,
		},
	}
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventTurnStatus, data))
}

func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}

// beginFlowMessage 为本次模型调用创建独立消息（每个模型调用一条消息）。
// 正文 part 先建立；reasoning part 仅在提供方实际返回可展示内容时才建立。
func (a *Agent) beginFlowMessage(ctx context.Context) {
	if a.Flow == nil || a.turn == nil {
		return
	}
	m := &sse.Message{
		MessageID:    "msg_" + newID(),
		TurnID:       a.turn.ID,
		InvocationID: "inv_" + newID(),
		Role:         "assistant",
		Purpose:      sse.PurposePending,
		Status:       sse.MessageStreaming,
		Parts:        []sse.Part{{PartID: "part_" + newID(), Type: sse.PartText}},
	}
	a.flowMsg = m
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageStarted, sse.MessageStartedData{Message: m}))
}

// flowMsgID 返回当前模型调用消息 ID（终态事件用它标识最终消息）。
func (a *Agent) flowMsgID() string {
	if a.flowMsg == nil {
		return ""
	}
	return a.flowMsg.MessageID
}

// flowTextPartID 返回当前消息的正文 part（模型每个调用固定一个正文 part）。
func (a *Agent) flowTextPartID() string {
	if a.flowMsg == nil {
		return ""
	}
	for _, p := range a.flowMsg.Parts {
		if p.Type == sse.PartText {
			return p.PartID
		}
	}
	return ""
}

// ensureReasoningPart 在首个 reasoning 增量到来时建立独立的 reasoning part，
// 并下发一次 message.started 更新（幂等 upsert，前端按 part_id 归并）。
func (a *Agent) ensureReasoningPart(ctx context.Context) string {
	if a.flowMsg == nil {
		return ""
	}
	for _, p := range a.flowMsg.Parts {
		if p.Type == sse.PartReasoning {
			return p.PartID
		}
	}
	a.flowMsg.Parts = append(a.flowMsg.Parts, sse.Part{PartID: "part_" + newID(), Type: sse.PartReasoning})
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageStarted, sse.MessageStartedData{Message: a.flowMsg}))
	return a.flowMsg.Parts[len(a.flowMsg.Parts)-1].PartID
}

// flowDelta 追加一条安全增量。index 是 part 内从 0 起的连续序号，
// 客户端只用 index 归并，不用字符串长度换算偏移。
func (a *Agent) flowDelta(ctx context.Context, partID, text string) {
	if a.flowMsg == nil || partID == "" || text == "" {
		return
	}
	for i := range a.flowMsg.Parts {
		if a.flowMsg.Parts[i].PartID != partID {
			continue
		}
		index := a.flowMsg.Parts[i].LastIndex
		a.flowMsg.Parts[i].Text += text
		a.flowMsg.Parts[i].LastIndex = index + 1
		a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageDelta, sse.MessageDeltaData{
			MessageID: a.flowMsg.MessageID, PartID: partID, Index: index, Delta: text,
		}))
		return
	}
}

// completeFlowMessage 用完整安全对象收口本消息，并确定用途。
func (a *Agent) completeFlowMessage(ctx context.Context, purpose sse.MessagePurpose, text string, failure error) {
	if a.flowMsg == nil {
		return
	}
	m := a.flowMsg
	a.flowMsg = nil
	a.flowLastMessageID = m.MessageID
	// 完成对象只保留非空 part；正文以完整文本覆盖增量暂存。
	parts := make([]sse.Part, 0, len(m.Parts))
	for _, p := range m.Parts {
		if p.Type == sse.PartReasoning {
			if p.Text == "" {
				// reasoning 文本由增量累积在服务端；完成对象以服务端组装结果为准，
				// 这里保留 part 元数据即可（last_index 已连续）。
				parts = append(parts, p)
				continue
			}
		}
		parts = append(parts, p)
	}
	m.Parts = parts
	if len(m.Parts) > 0 {
		for i := range m.Parts {
			if m.Parts[i].Type == sse.PartText {
				m.Parts[i].Text = text
			}
		}
	}
	if failure != nil {
		m.Status = sse.MessageFailed
		m.Error = failure.Error()
		a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageFailed, sse.MessageFailedData{
			Message: m, Error: failure.Error(),
		}))
		return
	}
	m.Status = sse.MessageCompleted
	m.Purpose = purpose
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageCompleted, sse.MessageCompletedData{Message: m}))
}

// interruptFlowMessage 在模型断流时保留已提交的部分正文，标记中断而不是伪装完整。
func (a *Agent) interruptFlowMessage(ctx context.Context, cause error) {
	if a.flowMsg == nil {
		return
	}
	m := a.flowMsg
	a.flowMsg = nil
	a.flowLastMessageID = m.MessageID
	m.Status = sse.MessageInterrupted
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	m.Error = msg
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventMessageFailed, sse.MessageFailedData{
		Message: m, Error: msg, Code: "STREAM_INTERRUPTED",
	}))
}

// flowTool 发布单个工具调用的创建/状态变化（同一个 tool_call_id 复用）。
func (a *Agent) flowTool(ctx context.Context, tool *sse.ToolCall) {
	if tool == nil || a.turn == nil {
		return
	}
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventToolUpdated, sse.ToolUpdatedData{Tool: tool}))
}

// flowInteraction 发布问题/审批/敏感读取的统一交互外壳。
func (a *Agent) flowInteraction(ctx context.Context, in *sse.Interaction) {
	if in == nil || a.turn == nil {
		return
	}
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: in}))
}

// flowNotice 发布用户可见告警/错误。
func (a *Agent) flowNotice(ctx context.Context, severity sse.NoticeSeverity, code, message string) {
	if a.turn == nil {
		return
	}
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventNoticeCreated, sse.NoticeCreatedData{Notice: &sse.Notice{
		NoticeID: "notice_" + newID(), TurnID: a.turn.ID,
		Severity: severity, Code: code, Message: message,
	}}))
}

// flowUsage 发布本 Turn 的累计用量快照（覆盖语义）。
func (a *Agent) flowUsage(ctx context.Context) {
	if a.turn == nil {
		return
	}
	a.flowEmit(ctx, sse.NewEvent(a.turn.SessionID, a.turn.ID, sse.EventUsageUpdated, sse.UsageUpdatedData{
		TurnID: a.turn.ID,
		Usage: sse.TokenUsage{
			PromptTokens:     a.turn.Token.PromptTokens,
			CompletionTokens: a.turn.Token.CompletionTokens,
			TotalTokens:      a.turn.Token.TotalTokens,
		},
	}))
}
