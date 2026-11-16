package sse

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventFrameIncludesIDAndData(t *testing.T) {
	e := NewEvent("s1", "t1", EventToolUpdated, ToolUpdatedData{Tool: &ToolCall{
		ToolCallID: "tc1", TurnID: "t1", Name: "get_resources", Status: ToolRunning,
	}})
	e.Seq = 5
	f := string(EventFrame(e))
	if !strings.Contains(f, "id: s1:5\n") {
		t.Errorf("持久事件帧缺少 id 行: %q", f)
	}
	if !strings.Contains(f, "event: tool.updated") {
		t.Errorf("帧缺少 event 行: %q", f)
	}
	if !strings.Contains(f, "data: {") {
		t.Errorf("帧缺少 data 行: %q", f)
	}
	if !strings.HasSuffix(f, "\n\n") {
		t.Errorf("帧未以空行收尾: %q", f)
	}
}

func TestControlFrameHasNoID(t *testing.T) {
	f := string(ControlFrameBytes(NewFatalFrame("s1", FatalData{Code: "PERSISTENCE_UNAVAILABLE", Retryable: true})))
	if strings.Contains(f, "id: ") {
		t.Errorf("控制帧不应带 id: %q", f)
	}
	if !strings.Contains(f, "event: stream.fatal") {
		t.Errorf("控制帧类型错误: %q", f)
	}
}

func TestFrameDecodeRoundTrip(t *testing.T) {
	e := NewEvent("s1", "t1", EventMessageDelta, MessageDeltaData{
		MessageID: "m1", PartID: "p1", Index: 3, Delta: "多行\n内容",
	})
	e.Seq = 7
	got, err := DecodeFrame(EventFrame(e))
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != "s1" || got.TurnID != "t1" || got.EventType != EventMessageDelta || got.Seq != 7 {
		t.Fatalf("round-trip 字段不一致: %+v", got)
	}
	payload, err := RemarshalData(got.Data)
	if err != nil {
		t.Fatal(err)
	}
	var delta MessageDeltaData
	if err := json.Unmarshal(payload, &delta); err != nil {
		t.Fatal(err)
	}
	if delta.Delta != "多行\n内容" {
		t.Errorf("多行 delta 未还原: %q", delta.Delta)
	}
}

func TestDecodeBareJSON(t *testing.T) {
	e := NewEvent("s9", "t9", EventTurnStatus, TurnStatusData{TurnID: "t9", Status: TurnRunning})
	e.Seq = 2
	raw, err := MarshalEvent(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.EventType != EventTurnStatus || got.SessionID != "s9" {
		t.Errorf("裸 JSON 解析失败: %+v", got)
	}
}
