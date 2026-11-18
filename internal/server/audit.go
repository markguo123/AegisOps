package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// auditQueryService 是审计只读端点依赖的最小接口（service.AuditQueryService 满足）。
type auditQueryService interface {
	List(ctx context.Context, f service.AuditFilter) (*service.AuditListResult, error)
	Get(ctx context.Context, id int64) (*service.AuditDetailResult, error)
}

// WithAuditQuery 装配审计只读查询面（P4.7）。链式可选，未装配时端点返回 501。
func (s *Server) WithAuditQuery(svc auditQueryService) *Server {
	if isNilService(svc) {
		s.auditQuery = nil
		return s
	}
	s.auditQuery = svc
	return s
}

// handleListAuditEvents GET /api/v1/audit/events：管理员审计检索（方案 28 §2）。
//
// 过滤与游标分页都在 Store 层下推 SQL；本 handler 只做参数解析与错误映射。
func (s *Server) handleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	if s.auditQuery == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "audit query is not configured")
		return
	}
	filter, err := parseAuditFilter(r)
	if err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
		return
	}
	result, err := s.auditQuery.List(r.Context(), filter)
	if err != nil {
		writeAuditQueryError(w, r, "list audit events", err)
		return
	}
	writeOK(w, r, http.StatusOK, result)
}

// handleGetAuditEvent GET /api/v1/audit/events/{id}：单条事件与关联时间线。
func (s *Server) handleGetAuditEvent(w http.ResponseWriter, r *http.Request) {
	if s.auditQuery == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, "audit query is not configured")
		return
	}
	id, err := strconv.ParseInt(strings.TrimSpace(r.PathValue("id")), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid audit event id")
		return
	}
	result, err := s.auditQuery.Get(r.Context(), id)
	if err != nil {
		writeAuditQueryError(w, r, "get audit event", err)
		return
	}
	writeOK(w, r, http.StatusOK, result)
}

// parseAuditFilter 解析列表查询参数并收敛边界。
// 非法时间/游标一律 400：静默忽略会让页面把一个被忽略的筛选当成"筛过了"。
func parseAuditFilter(r *http.Request) (service.AuditFilter, error) {
	q := r.URL.Query()
	f := service.AuditFilter{
		EventType: strings.TrimSpace(q.Get("eventType")),
		UserID:    strings.TrimSpace(q.Get("userId")),
		ClusterID: strings.TrimSpace(q.Get("clusterId")),
		SessionID: strings.TrimSpace(q.Get("sessionId")),
		Status:    strings.TrimSpace(q.Get("status")),
		Action:    strings.TrimSpace(q.Get("action")),
	}
	var err error
	if f.From, err = parseAuditTime(q.Get("from")); err != nil {
		return service.AuditFilter{}, err
	}
	if f.To, err = parseAuditTime(q.Get("to")); err != nil {
		return service.AuditFilter{}, err
	}
	if !f.From.IsZero() && !f.To.IsZero() && f.From.After(f.To) {
		return service.AuditFilter{}, errors.New("from must not be later than to")
	}
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return service.AuditFilter{}, errors.New("invalid limit")
		}
		f.Limit = n
	}
	cursor, err := service.DecodeAuditCursor(q.Get("cursor"))
	if err != nil {
		return service.AuditFilter{}, err
	}
	if cursor.ID > 0 {
		f.Cursor = &cursor
	}
	return f, nil
}

// parseAuditTime 解析 RFC3339 时间（浏览器 toISOString 的产物）；空串表示不限。
func parseAuditTime(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, errors.New("invalid time: " + raw)
	}
	return parsed.UTC(), nil
}

// writeAuditQueryError 把审计查询错误映射为统一错误信封。
func writeAuditQueryError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, service.ErrAuditUnavailable):
		writeError(w, r, http.StatusNotImplemented, codeInternal, "audit query is not configured")
	case errors.Is(err, store.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, "audit event not found")
	default:
		slog.Warn("server: 审计查询失败", "op", op, "request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, op+" failed")
	}
}

// compile-time interface check：查询服务需满足 server 依赖的最小接口。
var _ auditQueryService = (*service.AuditQueryService)(nil)
