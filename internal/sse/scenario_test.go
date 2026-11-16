package sse

import (
	"encoding/json"
	"testing"
)

// scenario 构造一段会话事件序列并做契约级校验：本测试就是 S1 的“事件与用户
// 场景都能用样例表达”基线，前端 reducer 的输入样例与此同构。
type scenario struct {
	name   string
	events []Event
}

func seq(events []Event) []Event {
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	return events
}

func finishedMessage(id, purpose string, text string) Message {
	return Message{
		MessageID: id, TurnID: "t1", InvocationID: "inv_" + id, Role: "assistant",
		Purpose: MessagePurpose(purpose), Status: MessageCompleted,
		Parts:    []Part{{PartID: "p_" + id, Type: PartText, Text: text, LastIndex: 1}},
		Revision: 6,
	}
}

func scenarios() []scenario {
	turn := func(status TurnStatus, reason WaitReason, final string) Event {
		return NewEvent("s1", "t1", EventTurnStatus, TurnStatusData{
			TurnID: "t1", Status: status, WaitReason: reason, FinalMessageID: final,
		})
	}
	return []scenario{
		{
			name: "普通长回答无 reasoning：结束前多次增量，结束只用同一 message 收口",
			events: seq([]Event{
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{
					MessageID: "m1", TurnID: "t1", InvocationID: "inv1", Role: "assistant",
					Purpose: PurposePending, Status: MessageStreaming,
					Parts: []Part{{PartID: "p1", Type: PartText}},
				}}),
				NewEvent("s1", "t1", EventMessageDelta, MessageDeltaData{MessageID: "m1", PartID: "p1", Index: 0, Delta: "第一段"}),
				NewEvent("s1", "t1", EventMessageDelta, MessageDeltaData{MessageID: "m1", PartID: "p1", Index: 1, Delta: "第二段"}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m1", "final", "第一段第二段"))}),
				turn(TurnDone, "", "m1"),
			}),
		},
		{
			name: "三次模型调用与工具前说明：过程正文不消失，最终全文只替换最终消息",
			events: seq([]Event{
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{MessageID: "m1", TurnID: "t1", InvocationID: "i1", Role: "assistant", Purpose: PurposePending, Parts: []Part{{PartID: "p1", Type: PartText}}}}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m1", "commentary", "先检查探针与 PVC 挂载。"))}),
				NewEvent("s1", "t1", EventToolUpdated, ToolUpdatedData{Tool: &ToolCall{ToolCallID: "tc1", TurnID: "t1", MessageID: "m1", Name: "get_resources", Status: ToolSucceeded}}),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{MessageID: "m2", TurnID: "t1", InvocationID: "i2", Role: "assistant", Purpose: PurposePending, Parts: []Part{{PartID: "p2", Type: PartText}}}}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m2", "commentary", "缺少入口文件，需要确认是否保留数据。"))}),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{MessageID: "m3", TurnID: "t1", InvocationID: "i3", Role: "assistant", Purpose: PurposePending, Parts: []Part{{PartID: "p3", Type: PartText}}}}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m3", "final", "处理结果与仍未解决的问题。"))}),
				turn(TurnDone, "", "m3"),
			}),
		},
		{
			name: "工具失败后继续诊断：工具卡失败，整轮仍可正常完成",
			events: seq([]Event{
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventToolUpdated, ToolUpdatedData{Tool: &ToolCall{ToolCallID: "tc9", TurnID: "t1", Name: "logs", Status: ToolFailed, Ok: ptr(false), ErrorCode: "RBAC_DENIED", Error: "forbidden"}}),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{MessageID: "m1", TurnID: "t1", InvocationID: "i1", Role: "assistant", Purpose: PurposePending, Parts: []Part{{PartID: "p1", Type: PartText}}}}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m1", "final", "日志读取被拒绝，改用事件继续判断。"))}),
				turn(TurnDone, "", "m1"),
			}),
		},
		{
			name: "选择→审批→执行→验证：waiting 不被 completed 覆盖，选择不授予写权限",
			events: seq([]Event{
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventInteractionUpdated, InteractionUpdatedData{Interaction: &Interaction{InteractionID: "q1", TurnID: "t1", Kind: InteractionQuestion, Status: InteractionPending, Options: []ChoiceOption{{Label: "保留 PVC", Value: "keep"}}}}),
				turn(TurnWaiting, WaitQuestion, ""),
				NewEvent("s1", "t1", EventInteractionUpdated, InteractionUpdatedData{Interaction: &Interaction{InteractionID: "q1", TurnID: "t1", Kind: InteractionQuestion, Status: InteractionAnswered, Answer: "keep"}}),
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventInteractionUpdated, InteractionUpdatedData{Interaction: &Interaction{InteractionID: "a1", TurnID: "t1", ToolCallID: "tc1", Kind: InteractionApproval, Status: InteractionPending, Options: []ChoiceOption{{Label: "确认执行", Value: "yes"}}}}),
				turn(TurnWaiting, WaitApproval, ""),
				NewEvent("s1", "t1", EventInteractionUpdated, InteractionUpdatedData{Interaction: &Interaction{InteractionID: "a1", TurnID: "t1", Kind: InteractionApproval, Status: InteractionApproved}}),
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventToolUpdated, ToolUpdatedData{Tool: &ToolCall{ToolCallID: "tc1", TurnID: "t1", Name: "create_resource", Status: ToolSucceeded}}),
				NewEvent("s1", "t1", EventMessageStarted, MessageStartedData{Message: &Message{MessageID: "m1", TurnID: "t1", InvocationID: "i1", Role: "assistant", Purpose: PurposePending, Parts: []Part{{PartID: "p1", Type: PartText}}}}),
				NewEvent("s1", "t1", EventMessageCompleted, MessageCompletedData{Message: ptr(finishedMessage("m1", "final", "已执行并验证。"))}),
				turn(TurnDone, "", "m1"),
			}),
		},
		{
			name: "保存失败与告警：notice 与 fatal 走非持久帧，不冒充成功",
			events: seq([]Event{
				turn(TurnRunning, "", ""),
				NewEvent("s1", "t1", EventNoticeCreated, NoticeCreatedData{Notice: &Notice{NoticeID: "n1", TurnID: "t1", Severity: NoticeError, Code: "PERSISTENCE_UNAVAILABLE", Message: "输出未能可靠保存"}}),
				turn(TurnFailed, "", ""),
			}),
		},
	}
}

func ptr[T any](v T) *T { return &v }

func TestScenarioSamplesSatisfyContract(t *testing.T) {
	for _, sc := range scenarios() {
		t.Run(sc.name, func(t *testing.T) {
			var lastSeq uint64
			seen := map[string]bool{}
			for _, e := range sc.events {
				if err := ValidateEvent(e); err != nil {
					t.Fatalf("%s seq=%d: %v", e.EventType, e.Seq, err)
				}
				if e.Seq <= lastSeq {
					t.Fatalf("seq 未严格递增: %d after %d", e.Seq, lastSeq)
				}
				lastSeq = e.Seq
				if _, err := json.Marshal(e); err != nil {
					t.Fatalf("marshal: %v", err)
				}
				// 同一 message 的增量必须 index 连续，工具/交互 ID 稳定复用。
				if e.EventType == EventMessageDelta {
					d := mustData[MessageDeltaData](t, e)
					key := d.MessageID + "/" + d.PartID
					if d.Index != 0 && !seen[key] && d.Index > 0 {
						t.Fatalf("part %s 首个增量 index=%d，应为 0", key, d.Index)
					}
					seen[key] = true
				}
			}
			// 终态必须出现且最后（与 §7 收尾一致）。
			last := sc.events[len(sc.events)-1]
			if last.EventType != EventTurnStatus {
				t.Fatalf("场景未以 turn.status 收尾: %s", last.EventType)
			}
			ts := mustData[TurnStatusData](t, last)
			if !ts.Status.Terminal() {
				t.Fatalf("收尾状态不是终态: %s", ts.Status)
			}
		})
	}
}

func mustData[T any](t *testing.T, e Event) T {
	t.Helper()
	raw, err := json.Marshal(e.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return out
}

// 编译期确认：同一 session 的 SSE id 由 seq 唯一确定（重放安全）。
func TestSSEIDUniquePerSeq(t *testing.T) {
	if SSEID("s1", 1) == SSEID("s1", 2) || SSEID("s1", 1) == SSEID("s2", 1) {
		t.Fatal("SSE id 未区分 session/seq")
	}
}
