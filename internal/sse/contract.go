// Package sse 定义 AegisOps Chat 展示协议 v2（方案 25 / ADR-055）。
//
// 这是 Chat 输出、SSE 传输、历史回放与前端归并的唯一公共契约：
//   - 规范展示对象：Turn / Message / Part / ToolCall / Interaction / Notice
//   - 语义事件：turn.status / message.* / tool.updated / interaction.updated /
//     notice.created / usage.updated
//   - 连接控制帧：stream.snapshot / stream.reset_required / stream.fatal
//
// 协议 v2 表示 Chat 流契约版本，不是平台 API 大版本；路径仍在 /api/v1/sessions*。
//
// 安全约定：任何写入事件流的内容在封装前必须脱敏。Secret 明文永不进入事件、
// 快照、会话文件或浏览器存储；Interaction 只承载目标/key、长度与指纹。
package sse

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ProtocolVersion 是 Chat 流契约版本。
const ProtocolVersion = 2

// EventType 是 v2 语义事件与连接控制帧的类型。
type EventType string

const (
	EventTurnStatus         EventType = "turn.status"
	EventMessageStarted     EventType = "message.started"
	EventMessageDelta       EventType = "message.delta"
	EventMessageCompleted   EventType = "message.completed"
	EventMessageFailed      EventType = "message.failed"
	EventToolUpdated        EventType = "tool.updated"
	EventInteractionUpdated EventType = "interaction.updated"
	EventNoticeCreated      EventType = "notice.created"
	EventUsageUpdated       EventType = "usage.updated"

	// 连接控制帧：不占用持久业务 seq，也不进入事件日志。
	FrameSnapshot      EventType = "stream.snapshot"
	FrameResetRequired EventType = "stream.reset_required"
	FrameFatal         EventType = "stream.fatal"
)

// IsControlFrame 判断事件类型是否为连接控制帧。
func IsControlFrame(t EventType) bool {
	switch t {
	case FrameSnapshot, FrameResetRequired, FrameFatal:
		return true
	}
	return false
}

// TurnStatus 是本轮运行状态，也是 UI 判断运行的唯一依据（不用卡片推测）。
type TurnStatus string

const (
	TurnPending  TurnStatus = "pending"
	TurnRunning  TurnStatus = "running"
	TurnWaiting  TurnStatus = "waiting"
	TurnDone     TurnStatus = "done"
	TurnFailed   TurnStatus = "failed"
	TurnCanceled TurnStatus = "canceled"
)

// Terminal 判断状态是否已终结（终态之后不得再追加正文）。
func (s TurnStatus) Terminal() bool {
	return s == TurnDone || s == TurnFailed || s == TurnCanceled
}

// WaitReason 说明 waiting 的具体原因（与运行状态分开，连接状态不参与）。
type WaitReason string

const (
	WaitQuestion WaitReason = "question"
	WaitApproval WaitReason = "approval"
	WaitSecret   WaitReason = "secret"
)

// Activity 是运行中的当前动作，只由事实产生（不按最后一张卡片猜测）。
type Activity string

const (
	ActivityChecking   Activity = "checking"
	ActivityGenerating Activity = "generating"
	ActivityExecuting  Activity = "executing"
	ActivityVerifying  Activity = "verifying"
)

// MessagePurpose 是本次模型调用正文的用途：pending 表示尚未确定，
// 结束后有工具调用为 commentary，无工具且正常结束为 final。
type MessagePurpose string

const (
	PurposePending    MessagePurpose = "pending"
	PurposeCommentary MessagePurpose = "commentary"
	PurposeFinal      MessagePurpose = "final"
)

// MessageStatus 是消息生命周期状态。
type MessageStatus string

const (
	MessageStreaming   MessageStatus = "streaming"
	MessageCompleted   MessageStatus = "completed"
	MessageFailed      MessageStatus = "failed"
	MessageInterrupted MessageStatus = "interrupted"
)

// PartType 是内容块类型。reasoning 仅在提供方实际返回可展示内容时创建。
type PartType string

const (
	PartText      PartType = "text"
	PartReasoning PartType = "reasoning"
)

// ToolStatus 是单个工具调用的状态。
type ToolStatus string

const (
	ToolStarted   ToolStatus = "started"
	ToolRunning   ToolStatus = "running"
	ToolSucceeded ToolStatus = "succeeded"
	ToolFailed    ToolStatus = "failed"
	ToolDenied    ToolStatus = "denied"
	ToolUnknown   ToolStatus = "unknown"
)

// InteractionKind 是统一交互外壳的种类。选择与审批授权不合并。
type InteractionKind string

const (
	InteractionQuestion   InteractionKind = "question"
	InteractionApproval   InteractionKind = "approval"
	InteractionSecretRead InteractionKind = "secret_read"
)

// InteractionStatus 是交互的公开状态；允许值按 kind 校验。
type InteractionStatus string

const (
	InteractionPending     InteractionStatus = "pending"
	InteractionAnswered    InteractionStatus = "answered"
	InteractionApproved    InteractionStatus = "approved"
	InteractionRejected    InteractionStatus = "rejected"
	InteractionSkipped     InteractionStatus = "skipped"
	InteractionTimedOut    InteractionStatus = "timed_out"
	InteractionCanceled    InteractionStatus = "canceled"
	InteractionInvalidated InteractionStatus = "invalidated"
)

// AllowedStatuses 返回某类交互允许的公开状态集合。
func (k InteractionKind) AllowedStatuses() map[InteractionStatus]bool {
	switch k {
	case InteractionQuestion:
		return map[InteractionStatus]bool{
			InteractionPending: true, InteractionAnswered: true, InteractionSkipped: true,
			InteractionTimedOut: true, InteractionCanceled: true, InteractionInvalidated: true,
		}
	case InteractionApproval:
		return map[InteractionStatus]bool{
			InteractionPending: true, InteractionApproved: true, InteractionRejected: true,
			InteractionTimedOut: true, InteractionCanceled: true, InteractionInvalidated: true,
		}
	case InteractionSecretRead:
		return map[InteractionStatus]bool{
			InteractionPending: true, InteractionAnswered: true, InteractionRejected: true,
			InteractionTimedOut: true, InteractionCanceled: true, InteractionInvalidated: true,
		}
	}
	return nil
}

// NoticeSeverity 是用户可见告警等级。
type NoticeSeverity string

const (
	NoticeInfo    NoticeSeverity = "info"
	NoticeWarning NoticeSeverity = "warning"
	NoticeError   NoticeSeverity = "error"
)

// ItemType 是规范展示条目的判别字段（快照与前端 reducer 共用）。
type ItemType string

const (
	ItemMessage     ItemType = "message"
	ItemTool        ItemType = "tool"
	ItemInteraction ItemType = "interaction"
	ItemNotice      ItemType = "notice"
)

// TokenUsage 是本 Turn 的累计用量（覆盖而不是逐事件相加）。
type TokenUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Zero 判断是否没有用量。
func (u TokenUsage) Zero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0
}

// Part 是一段内容块。一段增量只属于一个 part；last_index 是该 part 已提交的
// 连续增量下标（0 起），客户端只用 index 归并，不按字节长度计算偏移。
type Part struct {
	PartID    string   `json:"part_id"`
	Type      PartType `json:"type"`
	Text      string   `json:"text,omitempty"`
	LastIndex int      `json:"last_index"`
}

// Message 是一次模型调用的正文载体（每个模型调用独立 message_id）。
type Message struct {
	MessageID    string         `json:"message_id"`
	TurnID       string         `json:"turn_id"`
	InvocationID string         `json:"invocation_id"`
	Role         string         `json:"role"`
	Purpose      MessagePurpose `json:"purpose"`
	Status       MessageStatus  `json:"status"`
	Parts        []Part         `json:"parts"`
	ItemOrder    int64          `json:"item_order"`
	Revision     uint64         `json:"revision"`
	Error        string         `json:"error,omitempty"`
}

// Text 返回全部 part 的安全正文（顺序拼接）。
func (m Message) Text() string {
	var b strings.Builder
	for _, p := range m.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// Part 返回指定 part 的副本。
func (m Message) Part(id string) (Part, bool) {
	for _, p := range m.Parts {
		if p.PartID == id {
			return p, true
		}
	}
	return Part{}, false
}

// ChoiceOption 是问题/审批的可选项。
type ChoiceOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
	// Impact 说明该选择的业务后果（问题卡展示）。
	Impact      string `json:"impact,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// Target 是审批/敏感读取的 Kubernetes 目标。
type Target struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Group     string `json:"group,omitempty"`
	Version   string `json:"version,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
}

// Change 是审批预览中的一项变更。
type Change struct {
	Path        string `json:"path"`
	Operation   string `json:"operation,omitempty"`
	Before      any    `json:"before,omitempty"`
	After       any    `json:"after,omitempty"`
	Description string `json:"description,omitempty"`
}

// Check 是审批前门禁核验项。
type Check struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ToolCall 是一次工具调用的规范对象。tool_call_id 区分重试调用，
// 开始、结果与历史共用同一个 ID；不可用工具名或命令文本去重。
type ToolCall struct {
	ToolCallID  string     `json:"tool_call_id"`
	TurnID      string     `json:"turn_id"`
	StepID      string     `json:"step_id,omitempty"`
	MessageID   string     `json:"message_id,omitempty"`
	Name        string     `json:"name"`
	Params      string     `json:"params,omitempty"`
	Status      ToolStatus `json:"status"`
	Ok          *bool      `json:"ok,omitempty"`
	Result      string     `json:"result,omitempty"`
	Summary     string     `json:"summary,omitempty"`
	DisplayType string     `json:"display_type,omitempty"`
	DurationMs  int64      `json:"duration_ms,omitempty"`
	Risk        string     `json:"risk,omitempty"`
	Command     string     `json:"command,omitempty"`
	Stdout      string     `json:"stdout,omitempty"`
	Stderr      string     `json:"stderr,omitempty"`
	ExitCode    int        `json:"exit_code,omitempty"`
	ErrorCode   string     `json:"error_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	Data        any        `json:"data,omitempty"`
	ItemOrder   int64      `json:"item_order"`
	Revision    uint64     `json:"revision"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Interaction 是问题、审批、敏感读取的统一展示外壳。
// 问题的 answered 永远不等于审批 approved；选择不授予写权限。
type Interaction struct {
	InteractionID string            `json:"interaction_id"`
	TurnID        string            `json:"turn_id"`
	ToolCallID    string            `json:"tool_call_id,omitempty"`
	Kind          InteractionKind   `json:"kind"`
	Status        InteractionStatus `json:"status"`
	Revision      uint64            `json:"revision"`
	ExpiresAt     *time.Time        `json:"expires_at,omitempty"`
	// 公开载荷（已脱敏）。
	Title        string         `json:"title,omitempty"`
	Prompt       string         `json:"prompt,omitempty"`
	Tool         string         `json:"tool,omitempty"`
	Operation    string         `json:"operation,omitempty"`
	Risk         string         `json:"risk,omitempty"`
	Summary      string         `json:"summary,omitempty"`
	Command      string         `json:"command,omitempty"`
	Options      []ChoiceOption `json:"options,omitempty"`
	Target       *Target        `json:"target,omitempty"`
	Changes      []Change       `json:"changes,omitempty"`
	Checks       []Check        `json:"checks,omitempty"`
	Verification []string       `json:"verification,omitempty"`
	Secret       bool           `json:"secret,omitempty"`
	// Answer 是脱敏后的答案（选择的 option_id 或自由文本）；Secret 值永不出现。
	Answer    string     `json:"answer,omitempty"`
	ItemOrder int64      `json:"item_order"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// Notice 是用户可见告警/错误，覆盖降级、保存、模型与连接问题。
type Notice struct {
	NoticeID string         `json:"notice_id"`
	TurnID   string         `json:"turn_id,omitempty"`
	Severity NoticeSeverity `json:"severity"`
	Code     string         `json:"code"`
	Message  string         `json:"message"`
	// RelatedID 关联对象（message/tool/interaction），用于定位。
	RelatedID string     `json:"related_id,omitempty"`
	ItemOrder int64      `json:"item_order"`
	Revision  uint64     `json:"revision"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

type ContextBudget struct {
	ModelID       string `json:"model_id,omitempty"`
	Window        int    `json:"window"`
	OutputReserve int    `json:"output_reserve"`
	InputEstimate int    `json:"input_estimate"`
	Unavailable   bool   `json:"unavailable,omitempty"`
}

// TurnView 是快照与 turn.status 共用的完整 Turn 状态。
type TurnView struct {
	ContextBudget *ContextBudget `json:"context_budget,omitempty"`
	TurnID        string         `json:"turn_id"`
	SessionID     string         `json:"session_id"`
	Input         string         `json:"input,omitempty"`
	Status        TurnStatus     `json:"status"`
	WaitReason    WaitReason     `json:"wait_reason,omitempty"`
	Activity      Activity       `json:"activity,omitempty"`
	Error         string         `json:"error,omitempty"`
	Usage         TokenUsage     `json:"usage"`
	StartedAt     *time.Time     `json:"started_at,omitempty"`
	FinishedAt    *time.Time     `json:"finished_at,omitempty"`
	Revision      uint64         `json:"revision"`
}

// Item 是快照中的规范条目（带 type 判别）。
type Item struct {
	Type        ItemType     `json:"type"`
	ItemOrder   int64        `json:"item_order"`
	Message     *Message     `json:"message,omitempty"`
	Tool        *ToolCall    `json:"tool,omitempty"`
	Interaction *Interaction `json:"interaction,omitempty"`
	Notice      *Notice      `json:"notice,omitempty"`
}

type QuestionSummary struct {
	Key   string `json:"key"`
	Input string `json:"input"`
}

// Snapshot 是首屏/重同步的规范快照；cursor 是快照对应的会话水位。
type Snapshot struct {
	Questions       []QuestionSummary `json:"questions,omitempty"`
	ProtocolVersion int               `json:"protocol_version"`
	SessionID       string            `json:"session_id"`
	Cursor          uint64            `json:"cursor"`
	Turns           []TurnView        `json:"turns"`
	Items           []Item            `json:"items"`
	// ReplayFloor 是事件日志仍可补齐的最低 seq；cursor 低于它时必须重取快照。
	ReplayFloor uint64 `json:"replay_floor"`
}

// TurnStatusData 是 turn.status 事件载荷（完整 Turn 状态更新）。
type TurnStatusData struct {
	ContextBudget *ContextBudget `json:"context_budget,omitempty"`
	TurnID        string         `json:"turn_id"`
	SessionID     string         `json:"session_id,omitempty"`
	Status        TurnStatus     `json:"status"`
	WaitReason    WaitReason     `json:"wait_reason,omitempty"`
	Activity      Activity       `json:"activity,omitempty"`
	Input         string         `json:"input,omitempty"`
	Error         string         `json:"error,omitempty"`
	Usage         TokenUsage     `json:"usage"`
	StartedAt     *time.Time     `json:"started_at,omitempty"`
	FinishedAt    *time.Time     `json:"finished_at,omitempty"`
	// 终态时附带本轮最终回答所属 message_id（若存在），避免按卡片猜最终消息。
	FinalMessageID string `json:"final_message_id,omitempty"`
	Revision       uint64 `json:"revision,omitempty"`
}

// MessageStartedData 是 message.started 载荷。
type MessageStartedData struct {
	Message *Message `json:"message"`
}

// MessageDeltaData 是 message.delta 载荷。
type MessageDeltaData struct {
	MessageID string `json:"message_id"`
	PartID    string `json:"part_id"`
	Revision  uint64 `json:"revision"`
	Index     int    `json:"index"`
	Delta     string `json:"delta"`
}

// MessageCompletedData 是 message.completed 载荷：携带完整安全对象，
// 客户端用它替换本消息暂存文本，并确定用途。
type MessageCompletedData struct {
	Message *Message `json:"message"`
}

// MessageFailedData 是 message.failed 载荷：安全部分正文 + 错误，不冒充完整结论。
type MessageFailedData struct {
	Message *Message `json:"message"`
	Error   string   `json:"error"`
	Code    string   `json:"code,omitempty"`
}

// ToolUpdatedData 是 tool.updated 载荷。
type ToolUpdatedData struct {
	Tool *ToolCall `json:"tool"`
}

// InteractionUpdatedData 是 interaction.updated 载荷。
type InteractionUpdatedData struct {
	Interaction *Interaction `json:"interaction"`
}

// NoticeCreatedData 是 notice.created 载荷。
type NoticeCreatedData struct {
	Notice *Notice `json:"notice"`
}

// UsageUpdatedData 是 usage.updated 载荷（累计快照，覆盖语义）。
type UsageUpdatedData struct {
	TurnID string     `json:"turn_id"`
	Usage  TokenUsage `json:"usage"`
}

// Event 是 v2 事件信封。
type Event struct {
	ProtocolVersion int       `json:"protocol_version"`
	SessionID       string    `json:"session_id"`
	TurnID          string    `json:"turn_id,omitempty"`
	Seq             uint64    `json:"seq"`
	EventType       EventType `json:"event_type"`
	Timestamp       time.Time `json:"timestamp"`
	Data            any       `json:"data"`
}

// NewEvent 构造一个 v2 语义事件（seq 在提交事务内分配，构造时留空）。
func NewEvent(sessionID, turnID string, typ EventType, data any) Event {
	return Event{
		ProtocolVersion: ProtocolVersion,
		SessionID:       sessionID,
		TurnID:          turnID,
		Timestamp:       time.Now().UTC(),
		EventType:       typ,
		Data:            data,
	}
}

// ResetRequiredData 说明续传范围失效的原因。
type ResetRequiredData struct {
	Reason string `json:"reason"`
	// Cursor 是服务端当前可用的最低/最高水位提示（可选）。
	Cursor uint64 `json:"cursor,omitempty"`
}

// FatalData 表示无法继续可靠输出（非持久帧，不含未提交正文）。
type FatalData struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// ControlFrame 是连接控制帧（不占用持久业务 seq）。
type ControlFrame struct {
	EventType EventType `json:"event_type"`
	SessionID string    `json:"session_id,omitempty"`
	Data      any       `json:"data"`
}

// NewSnapshotFrame 构造规范快照控制帧。
func NewSnapshotFrame(s Snapshot) ControlFrame {
	return ControlFrame{EventType: FrameSnapshot, SessionID: s.SessionID, Data: s}
}

// NewResetRequiredFrame 构造续传失效控制帧。
func NewResetRequiredFrame(sessionID string, data ResetRequiredData) ControlFrame {
	return ControlFrame{EventType: FrameResetRequired, SessionID: sessionID, Data: data}
}

// NewFatalFrame 构造终止控制帧。
func NewFatalFrame(sessionID string, data FatalData) ControlFrame {
	return ControlFrame{EventType: FrameFatal, SessionID: sessionID, Data: data}
}

// SSEID 编码持久事件 id：<session_id>:<seq>。只用于持久事件。
func SSEID(sessionID string, seq uint64) string {
	return sessionID + ":" + strconv.FormatUint(seq, 10)
}

// ParseSSEID 解析 Last-Event-ID；格式非法时返回 ok=false。
func ParseSSEID(id string) (sessionID string, seq uint64, ok bool) {
	idx := strings.LastIndex(id, ":")
	if idx <= 0 || idx == len(id)-1 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(id[idx+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return id[:idx], n, true
}

// RemarshalData 把事件载荷在类型化结构体与通用结构之间往返标准化，供测试与
// 跨进程解析使用（前端 reducer 输入样例与 Go 单测共用同一份 JSON）。
func RemarshalData(v any) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal event data: %w", err)
	}
	return raw, nil
}

// ValidateEvent 校验语义事件的必填关联 ID（按事件类型）。
func ValidateEvent(e Event) error {
	if e.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("protocol_version %d != %d", e.ProtocolVersion, ProtocolVersion)
	}
	if e.SessionID == "" {
		return fmt.Errorf("session_id required")
	}
	if e.EventType == "" {
		return fmt.Errorf("event_type required")
	}
	if IsControlFrame(e.EventType) {
		return nil
	}
	if e.Seq == 0 {
		return fmt.Errorf("%s: seq required", e.EventType)
	}
	switch e.EventType {
	case EventTurnStatus, EventUsageUpdated:
		if e.TurnID == "" {
			return fmt.Errorf("%s: turn_id required", e.EventType)
		}
	case EventMessageStarted, EventMessageDelta, EventMessageCompleted, EventMessageFailed,
		EventToolUpdated, EventInteractionUpdated:
		if e.TurnID == "" {
			return fmt.Errorf("%s: turn_id required", e.EventType)
		}
		if e.Data == nil {
			return fmt.Errorf("%s: data required", e.EventType)
		}
	case EventNoticeCreated:
		if e.Data == nil {
			return fmt.Errorf("%s: data required", e.EventType)
		}
	}
	return nil
}
