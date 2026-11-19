package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/kubernetes"
	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/scanner"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

type rejectFinishStore struct{ store.ChatStore }

func TestApprovalCommitChecksRevisionAndPersistsDecision(t *testing.T) {
	svc := testChatServiceWithFlow(t, &scriptedLLM{}, &collector{}).(*chatService)
	sess, err := svc.GetOrCreateSession("approval-commit", "", "model")
	if err != nil {
		t.Fatal(err)
	}
	w := svc.flow.Open(sess.ID)
	ctx := context.Background()
	w.Emit(ctx, sse.NewEvent(sess.ID, "turn", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "turn", Status: sse.TurnWaiting}))
	w.Emit(ctx, sse.NewEvent(sess.ID, "turn", sse.EventInteractionUpdated, sse.InteractionUpdatedData{Interaction: &sse.Interaction{
		InteractionID: "approval", TurnID: "turn", Kind: sse.InteractionApproval, Status: sse.InteractionPending,
	}}))
	snap, err := svc.Snapshot(ctx, sess.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	revision := snap.Items[0].Interaction.Revision
	if err := svc.CommitApproval(ctx, sess.ID, "approval", "yes", revision+1); !errors.Is(err, store.ErrChatHeadConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if err := svc.CommitApproval(ctx, sess.ID, "approval", "yes", revision); err != nil {
		t.Fatal(err)
	}
	snap, err = svc.Snapshot(ctx, sess.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Items[0].Interaction.Status != sse.InteractionApproved {
		t.Fatal("decision was not persisted")
	}
	if err := svc.CommitApproval(ctx, sess.ID, "approval", "yes", revision); !errors.Is(err, store.ErrChatHeadConflict) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func (s rejectFinishStore) CommitChat(ctx context.Context, commit store.ChatCommit) (store.ChatCommitResult, error) {
	if commit.Session != nil {
		return store.ChatCommitResult{}, errors.New("injected final save failure")
	}
	return s.ChatStore.CommitChat(ctx, commit)
}

func TestChatFinalSaveFailureNeverPublishesDone(t *testing.T) {
	col := &collector{}
	svc := testChatServiceWithFlow(t, &scriptedLLM{responses: []*llm.ChatResponse{{Content: "完成"}}}, col).(*chatService)
	svc.flow.store = rejectFinishStore{svc.flow.store}
	sess, err := svc.GetOrCreateSession("save-failure", "", "model")
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Chat(context.Background(), ChatRequest{SessionID: sess.ID, Query: "检查"}, make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}
	for _, event := range col.snapshot() {
		if data, ok := event.Data.(sse.TurnStatusData); ok && data.Status == sse.TurnDone {
			t.Fatal("done escaped before model history was saved")
		}
	}
	snap, err := svc.Snapshot(context.Background(), sess.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Turns) != 1 || snap.Turns[0].Status != sse.TurnFailed {
		t.Fatalf("save failure left a stale run: %+v", snap.Turns)
	}
}

// collector 是测试用“提交后广播”收集器；publish 与数据库提交在同一写者 goroutine 调用。
type collector struct {
	mu     sync.Mutex
	events []sse.Event
}

func (c *collector) sink(e sse.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, e)
}

func (c *collector) snapshot() []sse.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]sse.Event(nil), c.events...)
}

func (c *collector) types() []sse.EventType {
	events := c.snapshot()
	out := make([]sse.EventType, 0, len(events))
	for _, e := range events {
		out = append(out, e.EventType)
	}
	return out
}

func (c *collector) contains(t sse.EventType) bool {
	for _, e := range c.snapshot() {
		if e.EventType == t {
			return true
		}
	}
	return false
}

// testChatServiceWithFlow 装配带 SQLite 事实源与单一输出管道的 Chat 服务。
func testChatServiceWithFlow(t *testing.T, fl llm.LLMClient, col *collector) ChatService {
	t.Helper()
	cfg, err := config.Load(config.LoadOptions{NoEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Agent.SessionDir = t.TempDir()
	cfg.Agent.AuditDir = t.TempDir()
	st, err := store.OpenSQLite("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader := kubernetes.NewClientWithClientset(fake.NewSimpleClientset())
	wr := &fakeWriteRunner{}
	return NewChatWithStoreAndSink(cfg,
		func() (scanner.Reader, error) { return reader, nil },
		func(llm.Options) llm.LLMClient { return fl },
		func() tools.ScanRunner { return &fakeScanRunner{} },
		func() (tools.WriteRunner, error) { return wr, nil },
		st, col.sink)
}

// typedRound 是一轮 LLM 调用的脚本：思考增量 →（可选）工具调用 →（可选）正文。
type typedRound struct {
	reasoning []string
	content   string
	toolCall  *llm.ToolCall
}

// typedStreamLLM 是带语义流事件的脚本化 LLM，按轮次回放。
type typedStreamLLM struct {
	rounds []typedRound
	idx    int
}

func (t *typedStreamLLM) next() typedRound {
	if len(t.rounds) == 0 {
		return typedRound{}
	}
	r := t.rounds[t.idx]
	if t.idx < len(t.rounds)-1 {
		t.idx++
	}
	return r
}

func (t *typedStreamLLM) Chat(_ context.Context, _ llm.ChatRequest) (*llm.ChatResponse, error) {
	var last typedRound
	if len(t.rounds) > 0 {
		last = t.rounds[len(t.rounds)-1]
	}
	resp := &llm.ChatResponse{Content: last.content, Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	if last.toolCall != nil {
		resp.ToolCalls = []llm.ToolCall{*last.toolCall}
	}
	return resp, nil
}

func (t *typedStreamLLM) ChatStreamEvents(_ context.Context, _ llm.ChatRequest, onEvent func(llm.StreamEvent)) (*llm.ChatResponse, error) {
	r := t.next()
	for _, d := range r.reasoning {
		onEvent(llm.StreamEvent{Type: llm.StreamReasoningDelta, Text: d})
	}
	if r.content != "" {
		onEvent(llm.StreamEvent{Type: llm.StreamContentDelta, Text: r.content})
	}
	resp := &llm.ChatResponse{Content: r.content, Usage: llm.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	if r.toolCall != nil {
		resp.ToolCalls = []llm.ToolCall{*r.toolCall}
	}
	return resp, nil
}

func firstOf(events []sse.Event, typ sse.EventType) int {
	for i, e := range events {
		if e.EventType == typ {
			return i
		}
	}
	return -1
}

func lastOf(events []sse.Event, typ sse.EventType) int {
	last := -1
	for i, e := range events {
		if e.EventType == typ {
			last = i
		}
	}
	return last
}

// TestChatFlowSingleExit 锁定单一输出管道：展示事件只有一套，Turn 状态区分
// 运行/等待/终态，工具卡带稳定 ID，正文增量先于 complete，终态最后。
func TestChatFlowSingleExit(t *testing.T) {
	fl := &typedStreamLLM{rounds: []typedRound{
		{
			reasoning: []string{"先看 ", "Pod 列表"},
			toolCall:  &llm.ToolCall{ID: "c1", Name: "get_resources", Arguments: json.RawMessage(`{"kind":"pods","secretKey":"sk-super-secret"}`)},
		},
		{reasoning: []string{"汇总异常项"}, content: "## 体检报告\n\n发现 1 个异常项"},
	}}
	col := &collector{}
	svc := testChatServiceWithFlow(t, fl, col)
	sess, err := svc.GetOrCreateSession("flow-s1", "", "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Chat(context.Background(), ChatRequest{SessionID: sess.ID, Query: "检查集群", TurnID: "turn-fixed"}, make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	collected := col.snapshot()
	if len(collected) == 0 {
		t.Fatal("没有提交任何展示事件")
	}
	for _, typ := range []sse.EventType{
		sse.EventTurnStatus, sse.EventMessageStarted, sse.EventMessageCompleted,
		sse.EventToolUpdated, sse.EventUsageUpdated,
	} {
		if firstOf(collected, typ) < 0 {
			t.Fatalf("缺少事件 %q，实际 %v", typ, col.types())
		}
	}
	// 所有事件属于同一个预分配 Turn，且 seq 严格递增（服务端分配）。
	for i, e := range collected {
		if e.TurnID != "turn-fixed" {
			t.Fatalf("事件 %q turn_id=%q，期望预分配 turn", e.EventType, e.TurnID)
		}
		if e.Seq == 0 || (i > 0 && e.Seq <= collected[i-1].Seq) {
			t.Fatalf("seq 未严格递增: %v", col.types())
		}
	}
	// 工具事件必须带稳定 tool_call_id，且参数已脱敏。
	toolIdx := firstOf(collected, sse.EventToolUpdated)
	toolData, ok := collected[toolIdx].Data.(sse.ToolUpdatedData)
	if !ok || toolData.Tool == nil {
		t.Fatalf("tool.updated data = %#v", collected[toolIdx].Data)
	}
	if toolData.Tool.ToolCallID != "c1" {
		t.Fatalf("tool_call_id=%q", toolData.Tool.ToolCallID)
	}
	if strings.Contains(toolData.Tool.Params, "sk-super-secret") || !strings.Contains(toolData.Tool.Params, "[REDACTED]") {
		t.Fatalf("工具参数未脱敏: %q", toolData.Tool.Params)
	}
	// 两个模型调用各自一条消息；第一条为 commentary，最后一条为 final。
	terminalCount := 0
	for _, event := range collected {
		if data, ok := event.Data.(sse.TurnStatusData); ok && data.Status == sse.TurnDone {
			terminalCount++
		}
	}
	if terminalCount != 1 {
		t.Fatalf("done must be committed exactly once with model history, got %d", terminalCount)
	}
	var purposes []sse.MessagePurpose
	for _, e := range collected {
		if e.EventType == sse.EventMessageCompleted {
			if d, ok := e.Data.(sse.MessageCompletedData); ok && d.Message != nil {
				for _, part := range d.Message.Parts {
					if part.Type == sse.PartReasoning && part.Text == "" {
						t.Fatal("completed reasoning lost its text")
					}
				}
				purposes = append(purposes, d.Message.Purpose)
			}
		}
	}
	if len(purposes) != 2 || purposes[0] != sse.PurposeCommentary || purposes[1] != sse.PurposeFinal {
		t.Fatalf("消息用途 = %v", purposes)
	}
	// 终态必须最后，且 done 不是 waiting。
	last := collected[len(collected)-1]
	if last.EventType != sse.EventTurnStatus {
		t.Fatalf("最后一个事件 = %q", last.EventType)
	}
	if d, ok := last.Data.(sse.TurnStatusData); !ok || d.Status != sse.TurnDone {
		t.Fatalf("终态 = %#v", last.Data)
	}
	// 正文增量 index 在每个 part 内从 0 连续。
	nextIndex := map[string]int{}
	for _, e := range collected {
		if e.EventType != sse.EventMessageDelta {
			continue
		}
		d, ok := e.Data.(sse.MessageDeltaData)
		if !ok {
			t.Fatalf("delta data = %#v", e.Data)
		}
		key := d.MessageID + "/" + d.PartID
		if d.Index != nextIndex[key] {
			t.Fatalf("%s index=%d want %d", key, d.Index, nextIndex[key])
		}
		nextIndex[key]++
	}
}

// TestSnapshotDuringRunIncludesActiveTurn 锁定刷新语义：运行中的 Turn 与它名下
// 已提交的正文必须出现在规范快照里，而不是等终态才可见。
func TestSnapshotDuringRunIncludesActiveTurn(t *testing.T) {
	svc := testChatServiceWithFlow(t, &scriptedLLM{}, &collector{}).(*chatService)
	svc.ensureFlow()
	if svc.flow == nil {
		t.Fatal("flow 未装配")
	}
	w := svc.flow.Open("live-s1")
	msg := &sse.Message{
		MessageID: "m1", TurnID: "t-live", InvocationID: "i1", Role: "assistant",
		Purpose: sse.PurposePending, Status: sse.MessageStreaming,
		Parts: []sse.Part{{PartID: "p1", Type: sse.PartText}},
	}
	w.Emit(context.Background(), sse.NewEvent("live-s1", "t-live", sse.EventTurnStatus, sse.TurnStatusData{
		TurnID: "t-live", SessionID: "live-s1", Status: sse.TurnRunning, Activity: sse.ActivityGenerating, Input: "检查 nginx",
	}))
	w.Emit(context.Background(), sse.NewEvent("live-s1", "t-live", sse.EventMessageStarted, sse.MessageStartedData{Message: msg}))
	w.Emit(context.Background(), sse.NewEvent("live-s1", "t-live", sse.EventMessageDelta, sse.MessageDeltaData{
		MessageID: "m1", PartID: "p1", Index: 0, Delta: "已确认",
	}))
	// delta 走微批（不等待 ack）；再发一个控制事件作为提交屏障，模拟真实 Agent
	// 在后续步骤前先确保前序文本已落库。
	w.Emit(context.Background(), sse.NewEvent("live-s1", "t-live", sse.EventTurnStatus, sse.TurnStatusData{
		TurnID: "t-live", SessionID: "live-s1", Status: sse.TurnRunning, Activity: sse.ActivityExecuting,
	}))

	snap, err := svc.Snapshot(context.Background(), "live-s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Turns) != 1 || snap.Turns[0].Status != sse.TurnRunning {
		t.Fatalf("运行中 Turn 未进入快照: %+v", snap.Turns)
	}
	if len(snap.Items) != 1 || snap.Items[0].Message == nil {
		t.Fatalf("运行中条目未进入快照: %+v", snap.Items)
	}
	if got := snap.Items[0].Message.Text(); got != "已确认" {
		t.Fatalf("快照正文未从追加块组装: %q", got)
	}
}

// TestChatFlowSnapshotMatchesEvents 验证历史快照与实时事件同源：快照消息与
// 事件完整对象一致，游标等于最后 seq。
func TestChatFlowSnapshotMatchesEvents(t *testing.T) {
	fl := &scriptedLLM{responses: []*llm.ChatResponse{{Content: "全部正常", Usage: llm.TokenUsage{TotalTokens: 3}}}}
	col := &collector{}
	svc := testChatServiceWithFlow(t, fl, col)
	sess, err := svc.GetOrCreateSession("flow-snap", "", "qwen-plus")
	if err != nil {
		t.Fatal(err)
	}
	events, err := svc.Chat(context.Background(), ChatRequest{SessionID: sess.ID, Query: "体检"}, make(chan string, 1))
	if err != nil {
		t.Fatal(err)
	}
	for range events {
	}

	provider, ok := svc.(interface {
		Snapshot(ctx context.Context, sessionID string, turnLimit int, target ...string) (sse.Snapshot, error)
	})
	if !ok {
		t.Fatal("ChatService 未暴露展示读取能力")
	}
	snap, err := provider.Snapshot(context.Background(), sess.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	collected := col.snapshot()
	if snap.Cursor != collected[len(collected)-1].Seq {
		t.Fatalf("快照游标 %d != 最后 seq %d", snap.Cursor, collected[len(collected)-1].Seq)
	}
	if len(snap.Items) == 0 || snap.Items[0].Message == nil {
		t.Fatalf("快照缺少消息条目: %+v", snap.Items)
	}
	if snap.Items[0].Message.Text() != "全部正常" {
		t.Fatalf("快照消息正文 = %q", snap.Items[0].Message.Text())
	}
}
