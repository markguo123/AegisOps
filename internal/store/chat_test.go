package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/sse"
)

func chatStoreOf(t *testing.T) ChatStore {
	t.Helper()
	st := newTestStore(t)
	cs, ok := st.(ChatStore)
	if !ok {
		t.Fatal("store does not implement ChatStore")
	}
	return cs
}

func startMessage(id, partID string, order int64) *sse.Message {
	return &sse.Message{
		MessageID: id, TurnID: "t1", InvocationID: "inv-" + id, Role: "assistant",
		Purpose: sse.PurposePending, Status: sse.MessageStreaming,
		Parts: []sse.Part{{PartID: partID, Type: sse.PartText}}, ItemOrder: order,
	}
}

func TestRecoverInterruptedChat(t *testing.T) {
	cs := chatStoreOf(t)
	ctx := context.Background()
	_, err := cs.CommitChat(ctx, ChatCommit{
		SessionID: "restart", CommandID: "waiting",
		Turn:         &session.Turn{ID: "turn", SessionID: "restart", Status: session.TurnWaiting},
		Interactions: []*sse.Interaction{{InteractionID: "question", TurnID: "turn", Kind: sse.InteractionQuestion, Status: sse.InteractionPending}},
		Events:       []sse.Event{sse.NewEvent("restart", "turn", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "turn", Status: sse.TurnWaiting})},
	})
	if err != nil {
		t.Fatal(err)
	}
	recovery := cs.(interface{ RecoverChatRuns(context.Context) error })
	if err := recovery.RecoverChatRuns(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := cs.ChatSnapshot(ctx, "restart", 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Turns) != 1 || snap.Turns[0].Status != sse.TurnFailed {
		t.Fatalf("stale run: %+v", snap)
	}
	if len(snap.Items) != 1 || snap.Items[0].Interaction.Status != sse.InteractionInvalidated {
		t.Fatalf("stale question: %+v", snap.Items)
	}
	if err := recovery.RecoverChatRuns(ctx); err != nil {
		t.Fatal(err)
	}
	head, _, _ := cs.ChatHead(ctx, "restart")
	if head != snap.Cursor {
		t.Fatal("repeated recovery changed terminal history")
	}
}

func TestCommitChatSnapshotAndReplay(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)

	// 1) 开始消息 + 两次增量。
	msg := startMessage("m1", "p1", 10)
	res, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "c1", SessionID: "s1", ExpectedHead: 0,
		Turn:     &session.Turn{ID: "t1", SessionID: "s1", Status: session.TurnRunning, Input: "检查 nginx"},
		Messages: []*sse.Message{msg},
		Deltas:   []ChatDelta{{MessageID: "m1", PartID: "p1", Index: 0, Text: "第一段"}},
		Events: []sse.Event{
			sse.NewEvent("s1", "t1", sse.EventMessageStarted, sse.MessageStartedData{Message: msg}),
			sse.NewEvent("s1", "t1", sse.EventMessageDelta, sse.MessageDeltaData{MessageID: "m1", PartID: "p1", Index: 0, Delta: "第一段"}),
			sse.NewEvent("s1", "t1", sse.EventTurnStatus, sse.TurnStatusData{
				TurnID: "t1", Status: sse.TurnRunning, Activity: sse.ActivityGenerating,
				ContextBudget: &sse.ContextBudget{Window: 131072, OutputReserve: 4096, InputEstimate: 83804},
			}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != 3 || len(res.Events) != 3 || res.Events[0].Seq != 1 || res.Events[2].Seq != 3 {
		t.Fatalf("unexpected commit result: %+v", res)
	}

	// 快照：未完成消息的正文从追加块组装。
	snap, err := cs.ChatSnapshot(ctx, "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Cursor != 3 || len(snap.Items) != 1 || snap.Items[0].Message.Text() != "第一段" {
		t.Fatalf("snapshot mismatch: %+v", snap)
	}
	if len(snap.Turns) != 1 || snap.Turns[0].Status != sse.TurnRunning {
		t.Fatalf("snapshot turn mismatch: %+v", snap.Turns)
	}
	if budget := snap.Turns[0].ContextBudget; budget == nil || *budget != (sse.ContextBudget{Window: 131072, OutputReserve: 4096, InputEstimate: 83804}) {
		t.Fatalf("snapshot budget mismatch: %+v", budget)
	}

	index, err := cs.ChatSnapshot(ctx, "s1", 1)
	if err != nil || len(index.Questions) != 1 || index.Questions[0].Key != "t1" {
		t.Fatalf("question index: %+v %v", index.Questions, err)
	}
	target, err := cs.ChatSnapshot(ctx, "s1", 1, "t1")
	if err != nil || len(target.Items) != 1 || len(target.Questions) != 0 {
		t.Fatalf("targeted turn: %+v %v", target, err)
	}
	foreign, err := cs.ChatSnapshot(ctx, "another", 1, "t1")
	if err != nil || len(foreign.Turns) != 0 {
		t.Fatalf("cross-session turn: %+v %v", foreign, err)
	}

	// 2) 第二个增量 + 完成消息（块行回收）+ 终态。
	done := startMessage("m1", "p1", 10)
	done.Status = sse.MessageCompleted
	done.Purpose = sse.PurposeFinal
	done.Parts[0].Text = "第一段第二段"
	done.Parts[0].LastIndex = 2
	res, err = cs.CommitChat(ctx, ChatCommit{
		CommandID: "c2", SessionID: "s1", ExpectedHead: 3,
		Turn:     &session.Turn{ID: "t1", SessionID: "s1", Status: session.TurnDone, Input: "检查 nginx", Output: "第一段第二段"},
		Messages: []*sse.Message{done},
		Deltas:   []ChatDelta{{MessageID: "m1", PartID: "p1", Index: 1, Text: "第二段"}},
		Collapse: []string{"m1"},
		Events: []sse.Event{
			sse.NewEvent("s1", "t1", sse.EventMessageDelta, sse.MessageDeltaData{MessageID: "m1", PartID: "p1", Index: 1, Delta: "第二段"}),
			sse.NewEvent("s1", "t1", sse.EventMessageCompleted, sse.MessageCompletedData{Message: done}),
			sse.NewEvent("s1", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnDone, FinalMessageID: "m1"}),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Head != 6 {
		t.Fatalf("head=%d", res.Head)
	}
	snap, err = cs.ChatSnapshot(ctx, "s1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Cursor != 6 || snap.Items[0].Message.Text() != "第一段第二段" {
		t.Fatalf("completed snapshot mismatch: %+v", snap.Items[0].Message)
	}
	// 块行在完成后回收。
	var chunks int
	if err := cs.(*dbStore).db.QueryRow(`SELECT COUNT(*) FROM chat_chunks WHERE session_id='s1'`).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if chunks != 0 {
		t.Fatalf("chunks not collapsed: %d", chunks)
	}
	// 消息对象 revision 与携带它的事件 seq 一致。
	if snap.Items[0].Message.Revision != 5 {
		t.Fatalf("message revision=%d, want 5", snap.Items[0].Message.Revision)
	}

	// 3) 游标补齐：从 3 之后读到 4/5/6。
	events, err := cs.ChatEventsAfter(ctx, "s1", 3, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Seq != 4 || events[2].EventType != sse.EventTurnStatus {
		t.Fatalf("replay mismatch: %+v", events)
	}
	// 4) 重复 seq 读取：游标 6 之后为空。
	if events, err := cs.ChatEventsAfter(ctx, "s1", 6, 100); err != nil || len(events) != 0 {
		t.Fatalf("expected empty replay, got %v / %v", events, err)
	}
}

func TestCommitChatCommandIdempotency(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	commit := ChatCommit{
		CommandID: "same", SessionID: "s2", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s2", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning})},
	}
	first, err := cs.CommitChat(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	// 同命令重放（即便 expected head 已过期）必须返回原结果，不重复追加。
	commit.ExpectedHead = 0
	second, err := cs.CommitChat(ctx, commit)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.Head != first.Head || len(second.Events) != 1 || second.Events[0].Seq != first.Events[0].Seq {
		t.Fatalf("idempotency mismatch: %+v vs %+v", first, second)
	}
	head, _, err := cs.ChatHead(ctx, "s2")
	if err != nil || head != 1 {
		t.Fatalf("head=%d err=%v", head, err)
	}
}

func TestCommitChatHeadConflict(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	if _, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "a", SessionID: "s3", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s3", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning})},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "b", SessionID: "s3", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s3", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnDone})},
	})
	if !errors.Is(err, ErrChatHeadConflict) {
		t.Fatalf("want head conflict, got %v", err)
	}
	// 冲突事务未留下任何事件。
	head, _, _ := cs.ChatHead(ctx, "s3")
	if head != 1 {
		t.Fatalf("head advanced on conflict: %d", head)
	}
}

func TestChatReplayFloor(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	st := cs.(*dbStore)
	if _, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "c1", SessionID: "s4", ExpectedHead: 0,
		Events: []sse.Event{
			sse.NewEvent("s4", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning}),
			sse.NewEvent("s4", "t1", sse.EventNoticeCreated, sse.NoticeCreatedData{Notice: &sse.Notice{NoticeID: "n1", Severity: sse.NoticeInfo, Code: "TEST", Message: "x"}}),
		},
	}); err != nil {
		t.Fatal(err)
	}
	// 模拟日志保留推进：删掉第一条事件并把 floor 推到 1。
	if _, err := st.db.Exec(`DELETE FROM chat_events WHERE session_id='s4' AND seq=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE chat_stream_heads SET replay_floor=1 WHERE session_id='s4'`); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.ChatEventsAfter(ctx, "s4", 0, 10); !errors.Is(err, ErrChatReplayUnavailable) {
		t.Fatalf("want replay unavailable, got %v", err)
	}
	if events, err := cs.ChatEventsAfter(ctx, "s4", 1, 10); err != nil || len(events) != 1 {
		t.Fatalf("floor cursor should replay: %v / %v", events, err)
	}
}

func TestLegacyMigrationMarkerIdempotent(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	if done, err := cs.LegacyMigrated(ctx, "s5"); err != nil || done {
		t.Fatalf("unexpected initial state: %v %v", done, err)
	}
	if err := cs.MarkLegacyMigrated(ctx, "s5", "digest-1", 3); err != nil {
		t.Fatal(err)
	}
	// 重复登记不报错，也不改变结果。
	if err := cs.MarkLegacyMigrated(ctx, "s5", "digest-1", 3); err != nil {
		t.Fatalf("repeat marker: %v", err)
	}
	if done, err := cs.LegacyMigrated(ctx, "s5"); err != nil || !done {
		t.Fatalf("marker missing: %v %v", done, err)
	}
}

// TestCommitChatRejectsControlFrameAndRollsBack 验证事务内校验失败时不会留下半提交：
// 水位、事件与实体都回滚（控制帧不得进入持久日志）。
func TestCommitChatRejectsControlFrameAndRollsBack(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	if _, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "seed", SessionID: "s7", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s7", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning})},
	}); err != nil {
		t.Fatal(err)
	}
	_, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "bad", SessionID: "s7", ExpectedHead: 1,
		Messages: []*sse.Message{startMessage("m1", "p1", 1)},
		Events:   []sse.Event{sse.NewEvent("s7", "t1", sse.FrameSnapshot, sse.Snapshot{SessionID: "s7"})},
	})
	if err == nil {
		t.Fatal("控制帧不应允许进入持久日志")
	}
	head, _, err := cs.ChatHead(ctx, "s7")
	if err != nil || head != 1 {
		t.Fatalf("失败提交推进了水位: head=%d err=%v", head, err)
	}
	snap, err := cs.ChatSnapshot(ctx, "s7", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Items) != 0 {
		t.Fatalf("失败提交留下了实体: %+v", snap.Items)
	}
}

// TestCommitChatDatabaseUnavailable 验证数据库不可用时提交明确失败，
// 不会假装成功（上层据此发送 fatal 并停止生成）。
func TestCommitChatDatabaseUnavailable(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite("file:" + randID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	cs := st.(ChatStore)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "c", SessionID: "s8", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s8", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning})},
	}); err == nil {
		t.Fatal("数据库不可用时提交必须返回错误")
	}
}

// TestCommitChatConflictRetrySafe 确保冲突后按同一命令 ID 重试不会重复追加文本。
func TestCommitChatConflictRetrySafe(t *testing.T) {
	ctx := context.Background()
	cs := chatStoreOf(t)
	if _, err := cs.CommitChat(ctx, ChatCommit{
		CommandID: "other", SessionID: "s6", ExpectedHead: 0,
		Events: []sse.Event{sse.NewEvent("s6", "t1", sse.EventTurnStatus, sse.TurnStatusData{TurnID: "t1", Status: sse.TurnRunning})},
	}); err != nil {
		t.Fatal(err)
	}
	commit := ChatCommit{
		CommandID: "retry", SessionID: "s6", ExpectedHead: 0,
		Deltas: []ChatDelta{{MessageID: "m1", PartID: "p1", Index: 0, Text: "只写一次"}},
		Events: []sse.Event{sse.NewEvent("s6", "t1", sse.EventMessageDelta, sse.MessageDeltaData{MessageID: "m1", PartID: "p1", Index: 0, Delta: "只写一次"})},
	}
	if _, err := cs.CommitChat(ctx, commit); !errors.Is(err, ErrChatHeadConflict) {
		t.Fatalf("want conflict: %v", err)
	}
	commit.ExpectedHead = 1
	if _, err := cs.CommitChat(ctx, commit); err != nil {
		t.Fatal(err)
	}
	var text string
	for i := 0; i < 2; i++ {
		if err := cs.(*dbStore).db.QueryRow(fmt.Sprintf(
			`SELECT text FROM chat_chunks WHERE session_id='s6' AND idx=%d`, i)).Scan(&text); err != nil {
			if i == 0 {
				t.Fatalf("chunk missing: %v", err)
			}
			break
		}
	}
	var n int
	if err := cs.(*dbStore).db.QueryRow(`SELECT COUNT(*) FROM chat_chunks WHERE session_id='s6'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("duplicate chunks: %d", n)
	}
	if text != "只写一次" {
		t.Fatalf("chunk text=%q", text)
	}
}
