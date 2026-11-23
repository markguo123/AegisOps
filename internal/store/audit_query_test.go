package store

import (
	"context"
	"testing"
	"time"
)

// auditFixture 写入一组时间戳受控的审计事件，供列表/游标/时间线用例共用。
func auditFixture(t *testing.T, st Store) []*AuditEvent {
	t.Helper()
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	rows := []*AuditEvent{
		{Timestamp: base, Action: "auth.login", EventType: AuditEventLogin, UserID: "u1", ClusterID: "c1", Status: "success"},
		{Timestamp: base.Add(1 * time.Second), Action: "tool-call.response", EventType: AuditEventToolCall, UserID: "u2", ClusterID: "c1", RequestID: "r1", SessionID: "s1", TurnID: "t1", ApprovalID: "a1", Status: "success"},
		{Timestamp: base.Add(2 * time.Second), Action: "approval.decision", EventType: AuditEventApproval, UserID: "u1", ClusterID: "c2", ApprovalID: "a1", Status: "success"},
		{Timestamp: base.Add(2 * time.Second), Action: "execution.failed", EventType: AuditEventResourceMutation, UserID: "u1", ClusterID: "c1", SessionID: "s1", TurnID: "t1", ApprovalID: "a1", Status: "error"},
		{Timestamp: base.Add(3 * time.Second), Action: ActionConfigChange, EventType: AuditEventConfigChange, UserID: "u3", ClusterID: "c1", SessionID: "s2", Status: "success"},
	}
	for _, e := range rows {
		if err := st.Audit().Write(context.Background(), e); err != nil {
			t.Fatalf("write audit event %q: %v", e.Action, err)
		}
	}
	return rows
}

func auditIDs(events []*AuditEvent) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.ID)
	}
	return out
}

func equalIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestAuditListFilteredAndCursor 锁 P4.7 的列表契约：过滤下推、倒序稳定、
// 游标在同一时间戳处也按 id 确定顺序且不重复返回上一页末行。
func TestAuditListFilteredAndCursor(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite("file:audit-query-" + randID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	auditFixture(t, st)
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

	all, err := st.Audit().ListFiltered(ctx, AuditQuery{Limit: 2})
	if err != nil {
		t.Fatalf("list first page: %v", err)
	}
	if !equalIDs(auditIDs(all), 5, 4, 3) {
		t.Fatalf("第一页应多取一行（limit+1）并按 timestamp DESC,id DESC 排序，got %v", auditIDs(all))
	}

	// 以第二条（id=4, 与 id=3 同一时间戳）作为游标继续翻页：必须先出 id=3，
	// 再按时间倒序出 id=2，且不重复 id=4。
	second, err := st.Audit().ListFiltered(ctx, AuditQuery{Limit: 1, BeforeTimestamp: all[1].Timestamp, BeforeID: all[1].ID})
	if err != nil {
		t.Fatalf("list second page: %v", err)
	}
	if !equalIDs(auditIDs(second), 3, 2) {
		t.Fatalf("同时间戳游标翻页应得 [3 2]，got %v", auditIDs(second))
	}

	filters := []struct {
		name string
		q    AuditQuery
		want []int64
	}{
		{"按事件类型", AuditQuery{EventType: AuditEventToolCall}, []int64{2}},
		{"按操作者", AuditQuery{UserID: "u1"}, []int64{4, 3, 1}},
		{"按集群", AuditQuery{ClusterID: "c1"}, []int64{5, 4, 2, 1}},
		{"按结果状态", AuditQuery{Status: "error"}, []int64{4}},
		{"按动作", AuditQuery{Action: ActionConfigChange}, []int64{5}},
		{"按时间范围（左闭右闭）", AuditQuery{From: base.Add(2 * time.Second), To: base.Add(2 * time.Second)}, []int64{4, 3}},
		{"起始时间（含）", AuditQuery{From: base.Add(3 * time.Second)}, []int64{5}},
	}
	for _, tc := range filters {
		got, err := st.Audit().ListFiltered(ctx, tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !equalIDs(auditIDs(got), tc.want...) {
			t.Errorf("%s: got %v, want %v", tc.name, auditIDs(got), tc.want)
		}
	}

	// 超出上限的 limit 必须被收敛到 100 而不是让调用方决定读多少。
	if got := (AuditQuery{Limit: 500}).normalizedLimit(); got != auditMaxLimit {
		t.Errorf("limit 上限收敛 = %d, want %d", got, auditMaxLimit)
	}
	if got := (AuditQuery{}).normalizedLimit(); got != auditDefaultLimit {
		t.Errorf("limit 默认值 = %d, want %d", got, auditDefaultLimit)
	}
}

// TestAuditTimelineUsesExplicitRelations 锁方案 28 §3：时间线只按明确关联键取
// 事件，并按时间正序返回；没有关联键时不得凭"相邻"编造时间线。
func TestAuditTimelineUsesExplicitRelations(t *testing.T) {
	ctx := context.Background()
	st, err := OpenSQLite("file:audit-timeline-" + randID() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	auditFixture(t, st)

	rows, err := st.Audit().Timeline(ctx, AuditRelation{ApprovalID: "a1"}, 10)
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	if !equalIDs(auditIDs(rows), 2, 3, 4) {
		t.Fatalf("审批关联时间线应按时间正序为 [2 3 4]，got %v", auditIDs(rows))
	}
	for _, event := range []*AuditEvent{
		{SessionID: "s1", TurnID: "t1", StepID: "step-a", Action: "tool-call.request"},
		{SessionID: "s1", TurnID: "t1", StepID: "step-b", Action: "tool-call.request"},
		{SessionID: "s1", TurnID: "t1", StepID: "step-a", Action: "k8s.access"},
		{SessionID: "s2", TurnID: "t1", StepID: "step-a", Action: "k8s.access"},
	} {
		if err := st.Audit().Write(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	rows, err = st.Audit().Timeline(ctx, AuditRelation{SessionID: "s1", StepID: "step-a", ApprovalID: "a1"}, 10)
	if err != nil || !equalIDs(auditIDs(rows), 6, 8) {
		t.Fatalf("时间线应只关联当前会话的同一工具步骤: ids=%v err=%v", auditIDs(rows), err)
	}

	// 会话不是单次操作关联键。
	rows, err = st.Audit().Timeline(ctx, AuditRelation{SessionID: "s2"}, 10)
	if err != nil {
		t.Fatalf("session timeline: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("会话时间线应为空，got %v", auditIDs(rows))
	}

	// 无任何关联键：空时间线，而不是返回最近若干条。
	rows, err = st.Audit().Timeline(ctx, AuditRelation{}, 10)
	if err != nil {
		t.Fatalf("empty timeline: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("无关联键时应返回空时间线，got %d 条", len(rows))
	}
}
