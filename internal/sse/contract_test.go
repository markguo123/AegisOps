package sse

import (
	"encoding/json"
	"testing"
)

func sampleMessage() Message {
	return Message{
		MessageID: "m_1", TurnID: "t_1", InvocationID: "inv_1", Role: "assistant",
		Purpose: PurposePending, Status: MessageStreaming,
		Parts:     []Part{{PartID: "p_1", Type: PartText, LastIndex: 2}},
		ItemOrder: 10, Revision: 3,
	}
}

func TestEventEnvelopeRequiredFields(t *testing.T) {
	e := NewEvent("s1", "t1", EventMessageDelta, MessageDeltaData{
		MessageID: "m1", PartID: "p1", Revision: 4, Index: 2, Delta: "已确认",
	})
	e.Seq = 128
	raw, err := MarshalEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"protocol_version", "session_id", "turn_id", "seq", "event_type", "timestamp", "data"} {
		if _, ok := m[key]; !ok {
			t.Errorf("缺少字段 %q: %s", key, raw)
		}
	}
	if m["protocol_version"].(float64) != 2 {
		t.Errorf("protocol_version=%v", m["protocol_version"])
	}
	if _, ok := m["sessionId"]; ok {
		t.Errorf("不应出现 camelCase 字段")
	}
	data := m["data"].(map[string]any)
	for _, key := range []string{"message_id", "part_id", "revision", "index", "delta"} {
		if _, ok := data[key]; !ok {
			t.Errorf("data 缺少字段 %q: %s", key, raw)
		}
	}
	if _, ok := data["index"]; !ok {
		t.Errorf("index=0 必须显式出现，不能 omitempty")
	}
}

func TestValidateEventByType(t *testing.T) {
	base := NewEvent("s1", "t1", EventTurnStatus, TurnStatusData{TurnID: "t1", Status: TurnRunning})
	base.Seq = 1
	if err := ValidateEvent(base); err != nil {
		t.Fatalf("turn.status: %v", err)
	}
	noTurn := base
	noTurn.TurnID = ""
	if err := ValidateEvent(noTurn); err == nil {
		t.Fatal("turn.status 缺 turn_id 应报错")
	}
	noSeq := base
	noSeq.Seq = 0
	if err := ValidateEvent(noSeq); err == nil {
		t.Fatal("语义事件缺 seq 应报错")
	}
	ctrl := NewSnapshotFrame(Snapshot{ProtocolVersion: 2, SessionID: "s1", Cursor: 9})
	if err := ValidateEvent(Event{ProtocolVersion: 2, SessionID: "s1", EventType: ctrl.EventType}); err != nil {
		t.Fatalf("控制帧不应要求 seq: %v", err)
	}
}

func TestSSEIDRoundTrip(t *testing.T) {
	id := SSEID("session-1", 42)
	if id != "session-1:42" {
		t.Fatalf("id=%q", id)
	}
	sid, seq, ok := ParseSSEID(id)
	if !ok || sid != "session-1" || seq != 42 {
		t.Fatalf("parse=%q %d %v", sid, seq, ok)
	}
	for _, bad := range []string{"", "abc", "abc:", ":1", "a:b:c"} {
		if _, _, ok := ParseSSEID(bad); ok {
			t.Errorf("非法 id %q 被接受", bad)
		}
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	msg := sampleMessage()
	tool := ToolCall{ToolCallID: "tc1", TurnID: "t1", Name: "get_resources", Status: ToolSucceeded, ItemOrder: 20}
	in := Interaction{InteractionID: "i1", TurnID: "t1", Kind: InteractionApproval, Status: InteractionPending, ItemOrder: 30}
	snap := Snapshot{
		ProtocolVersion: 2, SessionID: "s1", Cursor: 77, ReplayFloor: 3,
		Turns: []TurnView{{TurnID: "t1", SessionID: "s1", Status: TurnWaiting, WaitReason: WaitApproval, Revision: 77}},
		Items: []Item{
			{Type: ItemMessage, ItemOrder: 10, Message: &msg},
			{Type: ItemTool, ItemOrder: 20, Tool: &tool},
			{Type: ItemInteraction, ItemOrder: 30, Interaction: &in},
		},
	}
	frame := ControlFrameBytes(NewSnapshotFrame(snap))
	got, err := DecodeControlFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventType != FrameSnapshot {
		t.Fatalf("type=%v", got.EventType)
	}
	payload, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatal(err)
	}
	if back.Cursor != 77 || len(back.Items) != 3 || back.Items[2].Interaction.InteractionID != "i1" {
		t.Fatalf("snapshot round-trip 失败: %+v", back)
	}
}

func TestInteractionStatusAllowedByKind(t *testing.T) {
	if !InteractionQuestion.AllowedStatuses()[InteractionAnswered] {
		t.Error("question 应允许 answered")
	}
	if InteractionQuestion.AllowedStatuses()[InteractionApproved] {
		t.Error("question 不得出现 approved（选择不等于授权）")
	}
	if !InteractionApproval.AllowedStatuses()[InteractionApproved] {
		t.Error("approval 应允许 approved")
	}
	if InteractionApproval.AllowedStatuses()[InteractionAnswered] {
		t.Error("approval 不得出现 answered")
	}
	if !InteractionSecretRead.AllowedStatuses()[InteractionRejected] {
		t.Error("secret_read 应允许 rejected")
	}
}

func TestTerminalStatus(t *testing.T) {
	for _, s := range []TurnStatus{TurnDone, TurnFailed, TurnCanceled} {
		if !s.Terminal() {
			t.Errorf("%s 应为终态", s)
		}
	}
	for _, s := range []TurnStatus{TurnPending, TurnRunning, TurnWaiting} {
		if s.Terminal() {
			t.Errorf("%s 不应为终态", s)
		}
	}
}
