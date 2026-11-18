package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditQuery 是审计列表的有界查询条件（P4.7 / 方案 28 §2）。
//
// 过滤全部下推到 SQL：页面不允许"先全量读出来再在内存里筛"，那会让审计库的
// 体积直接变成页面响应时间。Cursor 采用 keyset 分页（timestamp DESC, id DESC），
// 同一时间戳的并发插入不会导致翻页重复或漏行。
type AuditQuery struct {
	From      time.Time
	To        time.Time
	EventType string
	UserID    string
	ClusterID string
	SessionID string
	Status    string
	Action    string

	// BeforeTimestamp / BeforeID 是上一页最后一条的排序键；零值表示第一页。
	BeforeTimestamp time.Time
	BeforeID        int64

	Limit int
}

// AuditRelation 是详情时间线使用的显式关联键（方案 28 §3）。
//
// 只认明确的身份字段，不按"时间相邻"猜关联：相邻的两条记录可能毫无关系，
// 拿它们拼成一条时间线等于编造因果。
type AuditRelation struct {
	RequestID  string
	SessionID  string
	TurnID     string
	ApprovalID string
	GrantID    string
}

// auditDefaultLimit / auditMaxLimit 与方案 28 §2 一致：默认 50 条、上限 100 条。
const (
	auditDefaultLimit = 50
	auditMaxLimit     = 100
)

func (q AuditQuery) normalizedLimit() int {
	switch {
	case q.Limit <= 0:
		return auditDefaultLimit
	case q.Limit > auditMaxLimit:
		return auditMaxLimit
	default:
		return q.Limit
	}
}

// ListFiltered 按条件查询审计事件，返回 limit+1 行供调用方判定 hasMore。
// 多取一行的做法把"还有没有下一页"变成一次读，而不是再发一条 COUNT。
func (st *auditStore) ListFiltered(ctx context.Context, q AuditQuery) ([]*AuditEvent, error) {
	where := []string{"1 = 1"}
	args := make([]any, 0, 10)
	if !q.From.IsZero() {
		where = append(where, "timestamp >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		where = append(where, "timestamp <= ?")
		args = append(args, q.To.UTC())
	}
	for _, f := range []struct {
		column string
		value  string
	}{
		{"event_type", q.EventType},
		{"user_id", q.UserID},
		{"cluster_id", q.ClusterID},
		{"session_id", q.SessionID},
		{"status", q.Status},
		{"action", q.Action},
	} {
		if strings.TrimSpace(f.value) == "" {
			continue
		}
		where = append(where, f.column+" = ?")
		args = append(args, strings.TrimSpace(f.value))
	}
	if !q.BeforeTimestamp.IsZero() {
		where = append(where, "(timestamp < ? OR (timestamp = ? AND id < ?))")
		args = append(args, q.BeforeTimestamp.UTC(), q.BeforeTimestamp.UTC(), q.BeforeID)
	}
	args = append(args, q.normalizedLimit()+1)

	rows, err := st.s.db.QueryContext(ctx, `SELECT `+auditSelectColumns+`
			FROM audit_events WHERE `+strings.Join(where, ` AND `)+`
			ORDER BY timestamp DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()
	out, err := scanAuditRows(rows)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Timeline 返回与当前事件同一条操作链的有界事件（按时间正序，便于阅读）。
//
// 关联优先级：审批/授权/请求 ID / 回合 ID 是"同一次操作"的强关联；只有在
// 这些键全部缺失时才退到会话级，并把条数交给 limit 收敛——否则一个长会话的
// 全部事件都会被当成这条记录的因果链。
func (st *auditStore) Timeline(ctx context.Context, rel AuditRelation, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = auditDefaultLimit
	}
	if limit > auditMaxLimit {
		limit = auditMaxLimit
	}
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 5)
	add := func(column, value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		clauses = append(clauses, column+" = ?")
		args = append(args, strings.TrimSpace(value))
	}
	add("approval_id", rel.ApprovalID)
	add("grant_id", rel.GrantID)
	add("request_id", rel.RequestID)
	add("turn_id", rel.TurnID)
	if len(clauses) == 0 {
		add("session_id", rel.SessionID)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	args = append(args, limit)
	rows, err := st.s.db.QueryContext(ctx, `SELECT `+auditSelectColumns+`
			FROM audit_events WHERE (`+strings.Join(clauses, ` OR `)+`)
			ORDER BY timestamp ASC, id ASC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit timeline: %w", err)
	}
	defer rows.Close()
	return scanAuditRows(rows)
}

// scanAuditRows 扫描多行审计记录（列表与时间线共用）。
func scanAuditRows(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]*AuditEvent, error) {
	var out []*AuditEvent
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
