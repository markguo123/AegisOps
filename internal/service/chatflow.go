package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/decision"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// Chat 单一输出管道（方案 25 §4/§5.2）：每个会话一个有界顺序写队列。
// Agent 通过 Flow 提交带稳定 ID 的语义事件；本层负责安全处理、100ms/4KiB
// 微批、规范实体的原子提交，并只在提交成功后广播。
const (
	flowQueueDepth  = 512
	flowBatchBytes  = 4 * 1024
	flowBatchDelay  = 100 * time.Millisecond
	flowCommitWait  = 15 * time.Second
	flowEmitWait    = 30 * time.Second
	flowMaxAttempts = 3
)

// chatFlow 管理所有会话的输出管道。
type chatFlow struct {
	store store.ChatStore

	mu      sync.Mutex
	writers map[string]*chatWriter

	publishMu sync.RWMutex
	publish   func(sse.Event)
	fatal     func(sessionID string, data sse.FatalData)
}

func newChatFlow(st store.ChatStore) *chatFlow {
	return &chatFlow{store: st, writers: map[string]*chatWriter{}}
}

// SetPublisher 注入“提交成功后广播”钩子；nil 表示无订阅者。
func (f *chatFlow) SetPublisher(fn func(sse.Event)) {
	f.publishMu.Lock()
	f.publish = fn
	f.publishMu.Unlock()
}

// SetFatal 注入非持久终止帧钩子（无法继续可靠输出时使用）。
func (f *chatFlow) SetFatal(fn func(string, sse.FatalData)) {
	f.publishMu.Lock()
	f.fatal = fn
	f.publishMu.Unlock()
}

func (f *chatFlow) emitPublish(e sse.Event) {
	f.publishMu.RLock()
	fn := f.publish
	f.publishMu.RUnlock()
	if fn != nil {
		fn(e)
	}
}

func (f *chatFlow) emitFatal(sessionID string, data sse.FatalData) {
	f.publishMu.RLock()
	fn := f.fatal
	f.publishMu.RUnlock()
	if fn != nil {
		fn(sessionID, data)
	}
}

// Open 返回会话的输出管道（不存在时创建）。会话在进程内复用同一个写者，
// 从而保证 seq/item_order 连续。
func (f *chatFlow) Open(sessionID string) *chatWriter {
	f.mu.Lock()
	defer f.mu.Unlock()
	if w, ok := f.writers[sessionID]; ok {
		if !w.failed.Load() {
			return w
		}
		<-w.done
	}
	w := &chatWriter{
		flow: f, sessionID: sessionID, cmds: make(chan flowCommand, flowQueueDepth),
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
		objects:    map[string]*flowObject{},
		redactors:  map[string]*streamRedactor{},
		deltaIndex: map[string]int{},
		collapse:   map[string]bool{},
	}
	// 初始化水位与条目顺序：从存储读取当前 head 和最近条目的 item_order。
	head, _, err := f.store.ChatHead(context.Background(), sessionID)
	if err != nil {
		slog.Warn("chatflow: 读取会话水位失败，按 0 开始", "session", sessionID, "error", err)
	}
	w.head = head
	if snap, err := f.store.ChatSnapshot(context.Background(), sessionID, 5); err == nil {
		for _, item := range snap.Items {
			if item.ItemOrder >= w.nextOrder {
				w.nextOrder = item.ItemOrder + 1
			}
		}
	}
	f.writers[sessionID] = w
	go w.run()
	return w
}

// Close 停止会话写者（会话删除/进程收尾）。
func (f *chatFlow) Close(sessionID string) {
	f.mu.Lock()
	w, ok := f.writers[sessionID]
	delete(f.writers, sessionID)
	f.mu.Unlock()
	if ok {
		close(w.stop)
	}
}

// Notice 在指定 Turn 上提交一条用户可见告警。
func (f *chatFlow) Notice(sessionID, turnID string, severity sse.NoticeSeverity, code, message string) {
	w := f.Open(sessionID)
	w.Emit(context.Background(), sse.NewEvent(sessionID, turnID, sse.EventNoticeCreated, sse.NoticeCreatedData{
		Notice: &sse.Notice{NoticeID: "notice_" + flowID(), TurnID: turnID, Severity: severity, Code: code, Message: redactText(message)},
	}))
}

// Snapshot/EventsAfter/Head 暴露给 HTTP 层（规范快照与游标补齐）。
func (f *chatFlow) Snapshot(ctx context.Context, sessionID string, turnLimit int, target ...string) (sse.Snapshot, error) {
	return f.store.ChatSnapshot(ctx, sessionID, turnLimit, target...)
}

func (f *chatFlow) EventsAfter(ctx context.Context, sessionID string, cursor uint64, limit int) ([]sse.Event, error) {
	return f.store.ChatEventsAfter(ctx, sessionID, cursor, limit)
}

func (f *chatFlow) Head(ctx context.Context, sessionID string) (uint64, uint64, error) {
	return f.store.ChatHead(ctx, sessionID)
}

// Snapshot 返回会话规范快照（HTTP 时间线与 SSE 首屏共用）。
func (s *chatService) Snapshot(ctx context.Context, sessionID string, turnLimit int, target ...string) (sse.Snapshot, error) {
	if s.startupErr != nil {
		return sse.Snapshot{}, s.startupErr
	}
	s.ensureFlow()
	if s.flow == nil {
		return sse.Snapshot{ProtocolVersion: sse.ProtocolVersion, SessionID: sessionID}, nil
	}
	return s.flow.Snapshot(ctx, sessionID, turnLimit, target...)
}

// EventsAfter 读取游标之后的持久事件。
func (s *chatService) EventsAfter(ctx context.Context, sessionID string, cursor uint64, limit int) ([]sse.Event, error) {
	s.ensureFlow()
	if s.flow == nil {
		return nil, nil
	}
	return s.flow.EventsAfter(ctx, sessionID, cursor, limit)
}

// Head 返回会话当前水位与可回放下限。
func (s *chatService) Head(ctx context.Context, sessionID string) (uint64, uint64, error) {
	s.ensureFlow()
	if s.flow == nil {
		return 0, 0, nil
	}
	return s.flow.Head(ctx, sessionID)
}

// CommitApproval records the public decision before the HTTP layer wakes the
// approval waiter. The original permission checks still govern execution.
func (s *chatService) CommitApproval(ctx context.Context, sessionID, interactionID, choice string, revision uint64) error {
	return s.updatePendingInteraction(ctx, sessionID, interactionID, revision, func(in *sse.Interaction) error {
		if in.Kind == sse.InteractionQuestion {
			return store.ErrChatHeadConflict
		}
		if in.Kind != sse.InteractionSecretRead && choice != "yes" && choice != "no" && choice != "yes_and_dont_ask_again" {
			return store.ErrChatHeadConflict
		}
		in.Status = sse.InteractionApproved
		if choice == "no" {
			in.Status = sse.InteractionRejected
		}
		in.Answer = ""
		if in.Kind != sse.InteractionSecretRead {
			in.Answer = choice
		}
		return nil
	})
}

func (s *chatService) CommitQuestion(ctx context.Context, answer decision.Answer) error {
	return s.updatePendingInteraction(ctx, answer.SessionID, answer.ID, 0, func(in *sse.Interaction) error {
		if in.Kind != sse.InteractionQuestion || in.TurnID != answer.TurnID {
			return store.ErrChatHeadConflict
		}
		in.Status = sse.InteractionAnswered
		in.Answer = redactText(answer.OptionID + answer.Text)
		if answer.Status == "skipped" {
			in.Status = sse.InteractionSkipped
		}
		return nil
	})
}

func (s *chatService) updatePendingInteraction(ctx context.Context, sessionID, interactionID string, revision uint64, update func(*sse.Interaction) error) error {
	snap, err := s.Snapshot(ctx, sessionID, 30)
	if err != nil {
		return err
	}
	for _, item := range snap.Items {
		in := item.Interaction
		if in == nil || in.InteractionID != interactionID {
			continue
		}
		if in.Status != sse.InteractionPending || (revision != 0 && revision != in.Revision) ||
			(in.ExpiresAt != nil && !time.Now().Before(*in.ExpiresAt)) {
			return store.ErrChatHeadConflict
		}
		if err := update(in); err != nil {
			return err
		}
		result, err := s.flow.store.CommitChat(ctx, store.ChatCommit{
			SessionID: sessionID, CommandID: "answer-" + flowID(), ExpectedHead: snap.Cursor,
			Interactions: []*sse.Interaction{in},
			Events:       []sse.Event{sse.NewEvent(sessionID, in.TurnID, sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: in})},
		})
		if err != nil {
			return err
		}
		for _, event := range result.Events {
			s.flow.emitPublish(event)
		}
		return nil
	}
	return store.ErrChatHeadConflict
}

// Notice 在指定 Turn 上提交一条用户可见告警（终态之后仍可用的独立告警路径）。
func (s *chatService) Notice(sessionID, turnID string, severity sse.NoticeSeverity, code, message string) {
	s.ensureFlow()
	if s.flow == nil {
		return
	}
	s.flow.Notice(sessionID, turnID, severity, code, message)
}

// flowObject 是未提交的规范对象（按 ID 去重，类型固定）。
type flowObject struct {
	kind        sse.ItemType
	turnID      string
	order       int64
	message     *sse.Message
	tool        *sse.ToolCall
	interaction *sse.Interaction
	notice      *sse.Notice
}

type flowCommand struct {
	event  *sse.Event
	finish *flowFinish
	ack    chan error
}

type flowFinish struct {
	turn           *session.Turn
	modelSession   *session.Session
	finalMessageID string
}

// chatWriter 是单会话的有界顺序写者。
type chatWriter struct {
	contextBudget *sse.ContextBudget
	flow          *chatFlow
	sessionID     string
	cmds          chan flowCommand
	stop          chan struct{}
	done          chan struct{}
	cancelMu      sync.Mutex

	// runCancel 在输出无法可靠保存时取消本轮 Agent 运行（停止生成与新工具调度）。
	runCancel func()
	head      uint64
	nextOrder int64
	failed    atomic.Bool

	events       []sse.Event
	deltas       []store.ChatDelta
	objects      map[string]*flowObject
	turn         *session.Turn
	modelSession *session.Session
	// deltaIndex 是服务端分配的 part 内连续增量下标（脱敏可能丢弃分片，
	// 客户端只依赖这里的连续性）。
	deltaIndex map[string]int
	redactors  map[string]*streamRedactor
	pendingBuf int
	collapse   map[string]bool
}

// SetRunCancel 绑定本轮运行的取消函数；输出失败时立即停止生成。
func (w *chatWriter) SetRunCancel(cancel func()) {
	if w == nil {
		return
	}
	w.cancelMu.Lock()
	w.runCancel = cancel
	w.cancelMu.Unlock()
}

// Emit 实现 agent.Flow。delta 走微批；控制事件等待提交确认后才返回，
// 保证工具调度前 tool.running 已持久化（方案 25 §5.2）。
func (w *chatWriter) Emit(ctx context.Context, e sse.Event) {
	if w == nil {
		return
	}
	if w.failed.Load() {
		return
	}
	if e.ProtocolVersion == 0 {
		e.ProtocolVersion = sse.ProtocolVersion
	}
	cmd := flowCommand{event: &e}
	wait := e.EventType != sse.EventMessageDelta
	if wait {
		cmd.ack = make(chan error, 1)
	}
	timeout := time.NewTimer(flowEmitWait)
	defer timeout.Stop()
	select {
	case w.cmds <- cmd:
	case <-ctx.Done():
		w.failOutput(fmt.Errorf("enqueue chat event: %w", ctx.Err()))
		return
	case <-timeout.C:
		w.failOutput(errors.New("enqueue chat event timed out"))
		return
	}
	if !wait {
		return
	}
	select {
	case err := <-cmd.ack:
		if err != nil {
			w.failOutput(err)
		}
	case <-ctx.Done():
	case <-timeout.C:
		w.failOutput(errors.New("waiting for chat event commit timed out"))
	}
}

// Finish 提交本轮终态与模型历史（同一事务），并等待提交完成。
func (w *chatWriter) Finish(ctx context.Context, turn *session.Turn, sess *session.Session, finalMessageID string) error {
	if w == nil {
		return nil
	}
	if w.failed.Load() {
		return fmt.Errorf("%w: chat output failed", errChatOutputUnavailable)
	}
	ack := make(chan error, 1)
	cmd := flowCommand{finish: &flowFinish{turn: turn, modelSession: sess, finalMessageID: finalMessageID}, ack: ack}
	timer := time.NewTimer(flowEmitWait)
	defer timer.Stop()
	select {
	case w.cmds <- cmd:
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		w.failOutput(errors.New("enqueue turn finish timed out"))
		return fmt.Errorf("%w: turn finish enqueue timed out", errChatOutputUnavailable)
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		w.failOutput(errors.New("waiting for turn finish timed out"))
		return fmt.Errorf("%w: turn finish commit timed out", errChatOutputUnavailable)
	}
}

var errChatOutputUnavailable = errors.New("chat output unavailable")

// failOutput 在输出无法可靠保存时停止生成，并通知订阅者非持久 fatal 帧。
func (w *chatWriter) failOutput(err error) {
	if w == nil || !w.failed.CompareAndSwap(false, true) {
		return
	}
	slog.Error("chatflow: 输出未能可靠保存，停止本轮生成", "session", w.sessionID, "error", err)
	w.flow.emitFatal(w.sessionID, sse.FatalData{
		Code: "PERSISTENCE_UNAVAILABLE", Message: "输出未能可靠保存，请重新同步", Retryable: true,
	})
	w.cancelMu.Lock()
	cancel := w.runCancel
	w.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *chatWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(flowBatchDelay)
	defer ticker.Stop()
	for {
		if w.failed.Load() {
			return
		}
		select {
		case <-w.stop:
			_ = w.flush()
			return
		case cmd := <-w.cmds:
			err := w.handle(cmd)
			if cmd.ack != nil {
				cmd.ack <- err
			}
			if err != nil {
				w.failOutput(err)
				return
			}
		case <-ticker.C:
			if w.buffered() {
				if err := w.flush(); err != nil {
					w.failOutput(err)
					return
				}
			}
		}
	}
}

func (w *chatWriter) buffered() bool {
	return len(w.events) > 0 || len(w.deltas) > 0 || len(w.objects) > 0
}

func (w *chatWriter) handle(cmd flowCommand) error {
	if cmd.finish != nil {
		return w.handleFinish(cmd.finish)
	}
	if cmd.event == nil {
		return nil
	}
	if err := w.ingest(*cmd.event); err != nil {
		return err
	}
	// 控制事件（message/tool/interaction/notice/turn）前必须冲刷已安全可释放的文本。
	if cmd.event.EventType != sse.EventMessageDelta || w.pendingBuf >= flowBatchBytes {
		return w.flush()
	}
	return nil
}

func (w *chatWriter) handleFinish(f *flowFinish) error {
	if f.turn != nil {
		w.events = append(w.events,
			sse.NewEvent(w.sessionID, f.turn.ID, sse.EventUsageUpdated, sse.UsageUpdatedData{
				TurnID: f.turn.ID,
				Usage: sse.TokenUsage{
					PromptTokens: f.turn.Token.PromptTokens, CompletionTokens: f.turn.Token.CompletionTokens,
					TotalTokens: f.turn.Token.TotalTokens,
				},
			}),
			sse.NewEvent(w.sessionID, f.turn.ID, sse.EventTurnStatus, sse.TurnStatusData{
				ContextBudget: w.contextBudget,
				TurnID:        f.turn.ID, SessionID: w.sessionID, Status: sse.TurnStatus(f.turn.Status),
				Input: f.turn.Input, Error: f.turn.Error, StartedAt: timePtr(f.turn.StartedAt),
				FinishedAt: f.turn.FinishedAt, FinalMessageID: f.finalMessageID,
				Usage: sse.TokenUsage{
					PromptTokens: f.turn.Token.PromptTokens, CompletionTokens: f.turn.Token.CompletionTokens,
					TotalTokens: f.turn.Token.TotalTokens,
				},
			}),
		)
	}
	w.turn = f.turn
	w.modelSession = f.modelSession
	if err := w.flush(); err != nil {
		return err
	}
	// 一轮结束：清空本轮缓冲，保留水位、条目顺序与会话写者。
	w.events, w.deltas, w.objects, w.collapse = nil, nil, map[string]*flowObject{}, map[string]bool{}
	w.deltaIndex, w.redactors, w.pendingBuf = map[string]int{}, map[string]*streamRedactor{}, 0
	w.turn, w.modelSession = nil, nil
	return nil
}

// ingest 把语义事件转成待提交的实体与安全事件。
func (w *chatWriter) ingest(e sse.Event) error {
	switch e.EventType {
	case sse.EventMessageDelta:
		return w.ingestDelta(e)
	case sse.EventMessageStarted:
		d := asMessageStarted(e.Data)
		if d == nil || d.Message == nil {
			return nil
		}
		w.sanitizeMessage(d.Message)
		w.trackMessage(d.Message)
		e.Data = *d
	case sse.EventMessageCompleted:
		d := asMessageCompleted(e.Data)
		if d == nil || d.Message == nil {
			return nil
		}
		w.sanitizeMessage(d.Message)
		w.finishMessageText(d.Message)
		w.trackMessage(d.Message)
		w.collapse[d.Message.MessageID] = true
		w.clearRedactors(d.Message)
		e.Data = *d
	case sse.EventMessageFailed:
		d := asMessageFailed(e.Data)
		if d == nil || d.Message == nil {
			return nil
		}
		w.sanitizeMessage(d.Message)
		if d.Message.Status == sse.MessageFailed {
			w.finishMessageText(d.Message)
			w.collapse[d.Message.MessageID] = true
		}
		w.trackMessage(d.Message)
		w.clearRedactors(d.Message)
		e.Data = *d
	case sse.EventToolUpdated:
		d := asToolUpdated(e.Data)
		if d == nil || d.Tool == nil {
			return nil
		}
		w.sanitizeTool(d.Tool)
		w.trackTool(d.Tool)
		e.Data = *d
	case sse.EventInteractionUpdated:
		d := asInteractionUpdated(e.Data)
		if d == nil || d.Interaction == nil {
			return nil
		}
		w.sanitizeInteraction(d.Interaction)
		w.trackInteraction(d.Interaction)
		e.Data = *d
	case sse.EventNoticeCreated:
		d := asNoticeCreated(e.Data)
		if d == nil || d.Notice == nil {
			return nil
		}
		d.Notice.Message = redactText(d.Notice.Message)
		w.trackNotice(d.Notice)
		e.Data = *d
	case sse.EventTurnStatus:
		// 每个状态更新都同步 Turn 主记录：快照读取 turns 表时才能看到运行中的
		// 回合与它名下的条目（否则刷新会丢掉进行中的过程内容）。
		if d := asTurnStatus(e.Data); d != nil {
			w.contextBudget = d.ContextBudget
			w.turn = &session.Turn{
				ID: d.TurnID, SessionID: w.sessionID, Status: session.TurnStatus(d.Status),
				Input: redactText(d.Input), Error: redactText(d.Error),
				Token: session.TokenUsage{
					PromptTokens: d.Usage.PromptTokens, CompletionTokens: d.Usage.CompletionTokens,
					TotalTokens: d.Usage.TotalTokens,
				},
			}
			if d.StartedAt != nil {
				w.turn.StartedAt = *d.StartedAt
			}
			if d.FinishedAt != nil {
				w.turn.FinishedAt = d.FinishedAt
			}
		}
	case sse.EventUsageUpdated:
		// 累计用量由 Turn 状态与 Finish 快照承载，事件本身照常落日志供回放。
	default:
		return fmt.Errorf("unsupported chat event type %q", e.EventType)
	}
	w.events = append(w.events, e)
	return nil
}

func (w *chatWriter) ingestDelta(e sse.Event) error {
	d := asMessageDelta(e.Data)
	if d == nil || d.MessageID == "" || d.PartID == "" || d.Delta == "" {
		return nil
	}
	key := d.MessageID + "\x00" + d.PartID
	red := w.redactors[key]
	if red == nil {
		red = newStreamRedactor()
		w.redactors[key] = red
	}
	safe, err := red.Push(d.Delta)
	if err != nil {
		return err
	}
	if safe == "" {
		return nil
	}
	index := w.deltaIndex[key]
	d.Index = index
	d.Delta = safe
	w.deltaIndex[key] = index + 1
	w.pendingBuf += len(safe)
	w.deltas = append(w.deltas, store.ChatDelta{
		SessionID: w.sessionID, MessageID: d.MessageID, PartID: d.PartID, Index: index, Text: safe,
	})
	e.Data = *d
	w.events = append(w.events, e)
	return nil
}

// finishMessageText 用完整（已脱敏）正文替换暂存分片，并校准 last_index。
func (w *chatWriter) clearRedactors(m *sse.Message) {
	for _, part := range m.Parts {
		delete(w.redactors, m.MessageID+"\x00"+part.PartID)
	}
}

func (w *chatWriter) finishMessageText(m *sse.Message) {
	for i := range m.Parts {
		if m.Parts[i].Type != sse.PartText && m.Parts[i].Type != sse.PartReasoning {
			continue
		}
		key := m.MessageID + "\x00" + m.Parts[i].PartID
		m.Parts[i].Text = redactText(m.Parts[i].Text)
		m.Parts[i].LastIndex = w.deltaIndex[key]
	}
}

func (w *chatWriter) trackMessage(m *sse.Message) {
	w.track(m.MessageID, sse.ItemMessage, m.TurnID, func(o *flowObject) {
		if m.ItemOrder == 0 {
			m.ItemOrder = w.takeOrder()
		}
		o.message = m
	})
}

func (w *chatWriter) trackTool(t *sse.ToolCall) {
	w.track(t.ToolCallID, sse.ItemTool, t.TurnID, func(o *flowObject) {
		if t.ItemOrder == 0 {
			t.ItemOrder = w.takeOrder()
		}
		o.tool = t
	})
}

func (w *chatWriter) trackInteraction(in *sse.Interaction) {
	w.track(in.InteractionID, sse.ItemInteraction, in.TurnID, func(o *flowObject) {
		if in.ItemOrder == 0 {
			in.ItemOrder = w.takeOrder()
		}
		o.interaction = in
	})
}

func (w *chatWriter) trackNotice(n *sse.Notice) {
	w.track(n.NoticeID, sse.ItemNotice, n.TurnID, func(o *flowObject) {
		if n.ItemOrder == 0 {
			n.ItemOrder = w.takeOrder()
		}
		o.notice = n
	})
}

func (w *chatWriter) track(id string, kind sse.ItemType, turnID string, apply func(*flowObject)) {
	o := w.objects[id]
	if o == nil {
		o = &flowObject{kind: kind, turnID: turnID}
		w.objects[id] = o
	}
	apply(o)
}

func (w *chatWriter) takeOrder() int64 {
	w.nextOrder++
	return w.nextOrder
}

func (w *chatWriter) flush() error {
	if len(w.events) == 0 && len(w.deltas) == 0 && len(w.objects) == 0 &&
		w.turn == nil && w.modelSession == nil {
		return nil
	}
	commit := store.ChatCommit{
		CommandID:    flowID(),
		SessionID:    w.sessionID,
		ExpectedHead: w.head,
		Turn:         w.turn,
		Session:      w.modelSession,
		Deltas:       append([]store.ChatDelta(nil), w.deltas...),
		Events:       append([]sse.Event(nil), w.events...),
	}
	if len(w.collapse) > 0 {
		commit.Collapse = make([]string, 0, len(w.collapse))
		for id := range w.collapse {
			commit.Collapse = append(commit.Collapse, id)
		}
	}
	for _, o := range w.objects {
		if o.message != nil {
			commit.Messages = append(commit.Messages, o.message)
		}
		if o.tool != nil {
			commit.Tools = append(commit.Tools, o.tool)
		}
		if o.interaction != nil {
			commit.Interactions = append(commit.Interactions, o.interaction)
		}
		if o.notice != nil {
			commit.Notices = append(commit.Notices, o.notice)
		}
	}
	var (
		result store.ChatCommitResult
		err    error
	)
	for attempt := 0; attempt < flowMaxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), flowCommitWait)
		result, err = w.flow.store.CommitChat(ctx, commit)
		cancel()
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrChatHeadConflict) {
			return fmt.Errorf("commit chat output: %w", err)
		}
		head, _, headErr := w.flow.store.ChatHead(context.Background(), w.sessionID)
		if headErr != nil {
			return fmt.Errorf("resolve chat head conflict: %w", headErr)
		}
		commit.ExpectedHead = head
		w.head = head
	}
	if err != nil {
		return fmt.Errorf("commit chat output after retries: %w", err)
	}
	w.head = result.Head
	for _, e := range result.Events {
		w.flow.emitPublish(e)
	}
	w.events, w.deltas = nil, nil
	w.objects, w.collapse = map[string]*flowObject{}, map[string]bool{}
	w.pendingBuf = 0
	// 只在 finish 提交成功后清理本轮 Turn 引用（普通提交保留以便后续事件）。
	w.turn, w.modelSession = nil, nil
	return nil
}

// ---- 事件载荷访问（值/指针兼容） ----

func asMessageDelta(v any) *sse.MessageDeltaData {
	switch d := v.(type) {
	case sse.MessageDeltaData:
		return &d
	case *sse.MessageDeltaData:
		return d
	}
	return nil
}

func asMessageStarted(v any) *sse.MessageStartedData {
	switch d := v.(type) {
	case sse.MessageStartedData:
		return &d
	case *sse.MessageStartedData:
		return d
	}
	return nil
}

func asMessageCompleted(v any) *sse.MessageCompletedData {
	switch d := v.(type) {
	case sse.MessageCompletedData:
		return &d
	case *sse.MessageCompletedData:
		return d
	}
	return nil
}

func asMessageFailed(v any) *sse.MessageFailedData {
	switch d := v.(type) {
	case sse.MessageFailedData:
		return &d
	case *sse.MessageFailedData:
		return d
	}
	return nil
}

func asToolUpdated(v any) *sse.ToolUpdatedData {
	switch d := v.(type) {
	case sse.ToolUpdatedData:
		return &d
	case *sse.ToolUpdatedData:
		return d
	}
	return nil
}

func asInteractionUpdated(v any) *sse.InteractionUpdatedData {
	switch d := v.(type) {
	case sse.InteractionUpdatedData:
		return &d
	case *sse.InteractionUpdatedData:
		return d
	}
	return nil
}

func asNoticeCreated(v any) *sse.NoticeCreatedData {
	switch d := v.(type) {
	case sse.NoticeCreatedData:
		return &d
	case *sse.NoticeCreatedData:
		return d
	}
	return nil
}

func asTurnStatus(v any) *sse.TurnStatusData {
	switch d := v.(type) {
	case sse.TurnStatusData:
		return &d
	case *sse.TurnStatusData:
		return d
	}
	return nil
}

// ---- 安全处理 ----

func (w *chatWriter) sanitizeMessage(m *sse.Message) {
	m.Error = redactText(m.Error)
	for i := range m.Parts {
		if m.Parts[i].Type == sse.PartText || m.Parts[i].Type == sse.PartReasoning {
			m.Parts[i].Text = redactText(m.Parts[i].Text)
		}
	}
}

func (w *chatWriter) sanitizeTool(t *sse.ToolCall) {
	t.Params = redactText(t.Params)
	t.Result = redactText(t.Result)
	t.Summary = redactText(t.Summary)
	t.Command = redactText(t.Command)
	t.Stdout = redactText(t.Stdout)
	t.Stderr = redactText(t.Stderr)
	t.Error = redactText(t.Error)
	t.ErrorCode = redactText(t.ErrorCode)
	t.DisplayType = redactText(t.DisplayType)
	t.Data = redactValue(t.Data)
}

func (w *chatWriter) sanitizeInteraction(in *sse.Interaction) {
	in.Title = redactText(in.Title)
	in.Prompt = redactText(in.Prompt)
	in.Tool = redactText(in.Tool)
	in.Operation = redactText(in.Operation)
	in.Risk = redactText(in.Risk)
	in.Summary = redactText(in.Summary)
	in.Command = redactText(in.Command)
	in.Answer = redactText(in.Answer)
	for i := range in.Options {
		in.Options[i].Label = redactText(in.Options[i].Label)
		in.Options[i].Impact = redactText(in.Options[i].Impact)
	}
	for i := range in.Changes {
		in.Changes[i].Path = redactText(in.Changes[i].Path)
		in.Changes[i].Description = redactText(in.Changes[i].Description)
		in.Changes[i].Before = redactValue(in.Changes[i].Before)
		in.Changes[i].After = redactValue(in.Changes[i].After)
	}
	for i := range in.Checks {
		in.Checks[i].Message = redactText(in.Checks[i].Message)
	}
	for i := range in.Verification {
		in.Verification[i] = redactText(in.Verification[i])
	}
}

// redactValue 对任意 JSON 值做文本脱敏（结构化工具结果进事件前统一处理）。
func redactValue(v any) any {
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	if err := json.Unmarshal([]byte(redactText(string(raw))), &out); err != nil {
		return nil
	}
	return out
}

func flowID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	tt := t
	return &tt
}
