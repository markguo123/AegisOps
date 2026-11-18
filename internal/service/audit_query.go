package service

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

// ErrAuditUnavailable 表示审计查询面没有装配 Store（服务端未配置平台库）。
var ErrAuditUnavailable = errors.New("audit: store not configured")

// 审计查询边界（方案 28 §2）：默认 50 条、上限 100 条，时间线同样有界。
const (
	auditQueryDefaultLimit = 50
	auditQueryMaxLimit     = 100
	auditTimelineLimit     = 100
)

// AuditCursor 是审计列表的 keyset 游标（timestamp DESC, id DESC 的排序键）。
// 编码为不透明字符串交给前端回传，页面无须理解内部格式。
type AuditCursor struct {
	Timestamp time.Time
	ID        int64
}

// EncodeAuditCursor 把游标编码成 URL 安全的字符串。
// 使用纳秒整数而不是格式化时间：MySQL TIMESTAMP(3) 与 SQLite 的精度差异
// 只在往返过程中丢失一次，把数据库读回的原值直接编码即可精确复现。
func EncodeAuditCursor(c AuditCursor) string {
	raw := strconv.FormatInt(c.Timestamp.UnixNano(), 10) + ":" + strconv.FormatInt(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeAuditCursor 解析 EncodeAuditCursor 的产物；非法输入返回错误，由
// handler 转成 400，而不是静默当作第一页（那会让翻页看起来"跳回开头"）。
func DecodeAuditCursor(raw string) (AuditCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return AuditCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return AuditCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	nsPart, idPart, ok := strings.Cut(string(decoded), ":")
	if !ok {
		return AuditCursor{}, errors.New("invalid cursor")
	}
	ns, err := strconv.ParseInt(nsPart, 10, 64)
	if err != nil {
		return AuditCursor{}, fmt.Errorf("invalid cursor timestamp: %w", err)
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil || id <= 0 {
		return AuditCursor{}, errors.New("invalid cursor id")
	}
	return AuditCursor{Timestamp: time.Unix(0, ns).UTC(), ID: id}, nil
}

// AuditFilter 是审计列表的查询条件（方案 28 §2）。
// 所有过滤都在 Store 层下推 SQL，服务层只做边界收敛与字段投影。
type AuditFilter struct {
	From      time.Time
	To        time.Time
	EventType string
	UserID    string
	ClusterID string
	SessionID string
	Status    string
	Action    string
	Limit     int
	Cursor    *AuditCursor
}

// AuditEventView 是审计页面可见的事件投影（方案 28 §4）。
//
// 它刻意不是 store.AuditEvent：存储层的 Payload 可能包含原始工具参数与终端
// 输出，查询接口绝不能直接透传。这里只给写入时已经脱敏并截断的摘要字段，
// 外加关联 ID 供页面串起时间线。
type AuditEventView struct {
	ID               int64     `json:"id"`
	Timestamp        time.Time `json:"timestamp"`
	RequestID        string    `json:"requestId,omitempty"`
	SessionID        string    `json:"sessionId,omitempty"`
	TurnID           string    `json:"turnId,omitempty"`
	StepID           string    `json:"stepId,omitempty"`
	UserID           string    `json:"userId,omitempty"`
	UserName         string    `json:"userName,omitempty"`
	ClusterID        string    `json:"clusterId,omitempty"`
	EventType        string    `json:"eventType"`
	Action           string    `json:"action"`
	Operation        string    `json:"operation,omitempty"`
	ResourceKind     string    `json:"resourceKind,omitempty"`
	ResourceName     string    `json:"resourceName,omitempty"`
	Risk             string    `json:"risk,omitempty"`
	Decision         string    `json:"decision,omitempty"`
	ApprovalID       string    `json:"approvalId,omitempty"`
	GrantID          string    `json:"grantId,omitempty"`
	MatchReason      string    `json:"matchReason,omitempty"`
	ParameterSummary string    `json:"parameterSummary,omitempty"`
	ResultSummary    string    `json:"resultSummary,omitempty"`
	Status           string    `json:"status,omitempty"`
}

// AuditListResult 是游标分页的列表响应。HasMore 为真时 NextCursor 非空。
type AuditListResult struct {
	Items      []AuditEventView `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
	HasMore    bool             `json:"hasMore"`
}

// AuditDetailResult 是详情响应：当前事件 + 按明确关联键查出的有界时间线。
type AuditDetailResult struct {
	Event             AuditEventView   `json:"event"`
	Timeline          []AuditEventView `json:"timeline"`
	TimelineTruncated bool             `json:"timelineTruncated"`
}

// AuditQueryService 提供审计只读查询（P4.7）。鉴权在 HTTP 网关（audit.read）
// 完成，本服务只负责有界查询、字段投影与操作者名称解析。
type AuditQueryService struct {
	store store.Store
}

// NewAuditQueryService 创建审计查询服务。
func NewAuditQueryService(st store.Store) *AuditQueryService {
	return &AuditQueryService{store: st}
}

func (s *AuditQueryService) available() error {
	if s == nil || s.store == nil {
		return ErrAuditUnavailable
	}
	return nil
}

// List 按条件查询审计事件。返回 limit+1 的判定在 Store 内完成，这里截断并
// 生成下一页游标。
func (s *AuditQueryService) List(ctx context.Context, f AuditFilter) (*AuditListResult, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	limit := f.Limit
	switch {
	case limit <= 0:
		limit = auditQueryDefaultLimit
	case limit > auditQueryMaxLimit:
		limit = auditQueryMaxLimit
	}
	q := store.AuditQuery{
		From:      f.From,
		To:        f.To,
		EventType: f.EventType,
		UserID:    f.UserID,
		ClusterID: f.ClusterID,
		SessionID: f.SessionID,
		Status:    f.Status,
		Action:    f.Action,
		Limit:     limit,
	}
	if f.Cursor != nil {
		q.BeforeTimestamp = f.Cursor.Timestamp
		q.BeforeID = f.Cursor.ID
	}
	rows, err := s.store.Audit().ListFiltered(ctx, q)
	if err != nil {
		return nil, err
	}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	names := s.userNames(ctx)
	items := make([]AuditEventView, 0, len(rows))
	for _, e := range rows {
		items = append(items, auditEventView(e, names))
	}
	next := ""
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		next = EncodeAuditCursor(AuditCursor{Timestamp: last.Timestamp, ID: last.ID})
	}
	return &AuditListResult{Items: items, NextCursor: next, HasMore: hasMore}, nil
}

// Get 返回单条审计事件及其关联时间线。事件不存在时返回 store.ErrNotFound，
// 由 handler 映射 404。
func (s *AuditQueryService) Get(ctx context.Context, id int64) (*AuditDetailResult, error) {
	if err := s.available(); err != nil {
		return nil, err
	}
	e, err := s.store.Audit().Get(ctx, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.Audit().Timeline(ctx, store.AuditRelation{
		RequestID:  e.RequestID,
		SessionID:  e.SessionID,
		TurnID:     e.TurnID,
		ApprovalID: e.ApprovalID,
		GrantID:    e.GrantID,
	}, auditTimelineLimit)
	if err != nil {
		return nil, err
	}
	names := s.userNames(ctx)
	timeline := make([]AuditEventView, 0, len(rows))
	for _, row := range rows {
		timeline = append(timeline, auditEventView(row, names))
	}
	return &AuditDetailResult{
		Event:             auditEventView(e, names),
		Timeline:          timeline,
		TimelineTruncated: len(rows) >= auditTimelineLimit,
	}, nil
}

// auditEventView 把存储模型投影成页面可见字段。新增字段必须同时问一句
// "它会不会把 Secret/kubeconfig/原始输出带出去"，默认不加。
func auditEventView(e *store.AuditEvent, names map[string]string) AuditEventView {
	eventType := e.EventType
	if eventType == "" {
		eventType = store.EventTypeForAction(e.Action)
	}
	return AuditEventView{
		ID:               e.ID,
		Timestamp:        e.Timestamp,
		RequestID:        e.RequestID,
		SessionID:        e.SessionID,
		TurnID:           e.TurnID,
		StepID:           e.StepID,
		UserID:           e.UserID,
		UserName:         names[e.UserID],
		ClusterID:        e.ClusterID,
		EventType:        eventType,
		Action:           e.Action,
		Operation:        e.Operation,
		ResourceKind:     e.ResourceKind,
		ResourceName:     e.ResourceName,
		Risk:             e.Risk,
		Decision:         e.Decision,
		ApprovalID:       e.ApprovalID,
		GrantID:          e.GrantID,
		MatchReason:      e.MatchReason,
		ParameterSummary: e.ParameterSummary,
		ResultSummary:    e.ResultSummary,
		Status:           e.Status,
	}
}

// userNames 解析审计事件的操作者显示名。账号已删除或读取失败时返回空表，
// 页面退回显示用户 ID——审计记录本身必须保留，不因用户表不可读而丢失。
func (s *AuditQueryService) userNames(ctx context.Context) map[string]string {
	users, err := s.store.Users().List(ctx)
	if err != nil {
		return map[string]string{}
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		name := strings.TrimSpace(u.DisplayName)
		if name == "" {
			name = u.Username
		}
		names[u.ID] = name
	}
	return names
}
