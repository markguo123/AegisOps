// Package sessionapi 实现企业平台会话（Chat Session）HTTP/SSE API：
// 会话增删改查、置顶改名、Chat 运行与停止、审批回传与事件流。
// 底层复用 service.ChatService；所有会话访问由认证主体与 owner 约束。
package sessionapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/permission"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/settings"
	"github.com/k8s-ai/k8s-ai/internal/sse"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// chatService 是会话 API 依赖的最小应用服务接口（service.ChatService 满足）。
type chatService interface {
	Chat(ctx context.Context, req service.ChatRequest, respond <-chan string) (<-chan service.ChatEvent, error)
	GetSession(id string) (*session.Session, error)
	GetOrCreateSessionForUser(id, userID, namespace, model string) (*session.Session, error)
	ListSessions() ([]session.Session, error)
	ListSessionsForUser(userID string) ([]session.Session, error)
	DeleteSession(id string) error
	RenameSession(id, name string) error
	PinSession(id string, pinned bool) error
}

// PrincipalResolver 从 Bearer Token 解析当前用户与角色。
type PrincipalResolver func(ctx context.Context, token string) (*store.User, *store.Role, error)

// ScopeResolver 解析当前用户可访问的 Kubernetes 资源范围。
type ScopeResolver func(ctx context.Context, user *store.User, role *store.Role) (*tools.AccessScope, error)

type requestPrincipal struct {
	recheck func(context.Context) (*store.User, *store.Role, error)
	User    *store.User
	Role    *store.Role
}

type principalContextKey struct{}

func withPrincipal(ctx context.Context, principal requestPrincipal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

func principalFrom(r *http.Request) (requestPrincipal, bool) {
	principal, ok := r.Context().Value(principalContextKey{}).(requestPrincipal)
	return principal, ok && principal.User != nil
}

func (p requestPrincipal) canReadAllSessions() bool {
	return p.Role != nil && permission.HasPermission(p.Role.Permissions, permission.ChatReadAll)
}

func (p requestPrincipal) canManageAllSessions() bool {
	return p.Role != nil && permission.HasPermission(p.Role.Permissions, permission.ChatManageAll)
}

// sseEmitter 是可为 ChatService 挂载统一事件广播的可选接口。
type sseEmitter interface {
	SetSink(sink func(sse.Event))
}

// Broadcaster 每会话一个，SSE 消息扇出（设计借鉴 kubectl-ai）。
type Broadcaster struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newBroadcaster() *Broadcaster {
	return &Broadcaster{clients: map[chan []byte]struct{}{}}
}

func (b *Broadcaster) add() chan []byte {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *Broadcaster) remove(ch chan []byte) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

func (b *Broadcaster) broadcast(msg []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
			// 客户端消费慢则丢消息（SSE 重连时以历史快照兜底）。
		}
	}
}

// Server 是 Web chat HTTP 服务。
type confirmationTicket struct {
	id      string
	expires time.Time
	reply   chan string
}

type Server struct {
	tickets  map[string]confirmationTicket
	chat     chatService
	model    string
	platform *service.PlatformAccess
	scope    ScopeResolver
	// audit 写跨用户会话操作留痕（U-06）；nil 时跳过，不影响业务请求。
	audit AuditSink
	// ownerNames 仅在管理员跨用户读列表时解析 owner 展示名；nil 或失败时前端
	// 回退到稳定 ownerId，不影响列表本身。
	ownerNames func(context.Context) (map[string]string, error)

	mu           sync.Mutex
	broadcasters map[string]*Broadcaster
	pending      map[string]chan string        // sessionID → 当前 Chat 的审批响应通道
	runs         map[string]context.CancelFunc // sessionID → 当前 Chat 运行取消函数
	eventSeq     map[string]uint64
	eventLogs    map[string][]bufferedEvent
}

type bufferedEvent struct {
	seq uint64
	raw []byte
}

const maxBufferedEventsPerSession = 512

// New 创建会话 API 服务。
func New(chat chatService, model string) *Server {
	s := &Server{
		chat:         chat,
		model:        model,
		broadcasters: map[string]*Broadcaster{},
		tickets:      map[string]confirmationTicket{}, pending: map[string]chan string{},
		runs:      map[string]context.CancelFunc{},
		eventSeq:  map[string]uint64{},
		eventLogs: map[string][]bufferedEvent{},
	}
	// 服务端事件路由：统一事件按 session_id 扇出到对应 Broadcaster；
	// 旧 ChatEvent JSON 仍在 handleSendMessage 里并行广播，两者不互相覆盖。
	if em, ok := chat.(sseEmitter); ok {
		em.SetSink(func(e sse.Event) {
			// 事件日志只保留当前运行回合，供重连时补齐工具/审批流；
			// 新一回合开始或会话重新发消息时清空旧日志，避免与历史快照重复。
			s.mu.Lock()
			id := e.SessionID
			if e.EventType == sse.EventSessionCreated || e.EventType == sse.EventTurnStarted {
				s.eventLogs[id] = nil
			}
			s.eventSeq[id]++
			e.Seq = s.eventSeq[id]
			raw, err := json.Marshal(e)
			if err != nil {
				s.mu.Unlock()
				slog.Warn("sessionapi: 统一事件序列化失败", "error", err)
				return
			}
			if id != "" {
				logs := append(s.eventLogs[id], bufferedEvent{seq: e.Seq, raw: raw})
				if len(logs) > maxBufferedEventsPerSession {
					logs = logs[len(logs)-maxBufferedEventsPerSession:]
				}
				s.eventLogs[id] = logs
			}
			s.mu.Unlock()
			s.broadcaster(e.SessionID).broadcast(raw)
		})
	}
	return s
}

// WithScopeResolver 为平台 Chat 注入执行前资源范围判定。
func (s *Server) WithScopeResolver(resolve ScopeResolver) *Server {
	s.scope = resolve
	return s
}

// WithOwnerNames 注入会话 owner 展示名解析（仅用于管理员跨用户列表）。
func (s *Server) WithOwnerNames(resolve func(context.Context) (map[string]string, error)) *Server {
	s.ownerNames = resolve
	return s
}

func (s *Server) WithPlatformAccess(p *service.PlatformAccess) *Server { s.platform = p; return s }

// Handler 返回路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/v1/sessions", s.handleCreateSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/rename", s.handleRenameSession)
	mux.HandleFunc("POST /api/v1/sessions/{id}/pin", s.handlePinSession)
	mux.HandleFunc("DELETE /api/v1/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("GET /api/v1/sessions/{id}/stream", s.handleStream)
	mux.HandleFunc("POST /api/v1/sessions/{id}/send-message", s.handleSendMessage)
	mux.HandleFunc("POST /api/v1/sessions/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /api/v1/sessions/{id}/cancel", s.handleCancel)
	return mux
}

// AuthenticatedHandler 为四期平台入口：先校验 Bearer Token，再把用户身份
// 注入请求上下文。会话归属判定与 Chat 本体都在该身份下执行。
func (s *Server) AuthenticatedHandler(resolve PrincipalResolver) http.Handler {
	next := s.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		token := bearerToken(r)
		recheck := func(ctx context.Context) (*store.User, *store.Role, error) { return resolve(ctx, token) }
		user, role, err := recheck(r.Context())
		if err != nil || user == nil || role == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
			return
		}
		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), requestPrincipal{
			recheck: recheck, User: user,
			Role: role,
		})))
	})
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	value := r.Header.Get("Authorization")
	if len(value) <= len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(value[len(prefix):])
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	if s.platform != nil && principal.canReadAllSessions() {
		if s.audit == nil || s.audit(r.Context(), store.AuditEvent{Action: actionAdminSessionRead, EventType: store.AuditEventSession, UserID: principal.User.ID, ResourceKind: "SessionList", Status: "success"}) != nil {
			writeError(w, 503, fmt.Errorf("audit unavailable"))
			return
		}
	}
	var (
		list []session.Session
		err  error
	)
	if principal.canReadAllSessions() {
		list, err = s.chat.ListSessions()
	} else {
		list, err = s.chat.ListSessionsForUser(principal.User.ID)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type item struct {
		ClusterID string `json:"clusterId,omitempty"`
		OwnerID   string `json:"ownerId,omitempty"`
		// OwnerName 仅管理员跨用户读列表时回填展示名；缺失时前端用 ownerId 兜底。
		OwnerName    string    `json:"ownerName,omitempty"`
		ID           string    `json:"id"`
		Name         string    `json:"name"`
		State        string    `json:"state"`
		Model        string    `json:"model,omitempty"`
		MessageCount int       `json:"messageCount"`
		LastModified time.Time `json:"lastModified"`
		// P7.5：置顶排序 + 标题自动提取（首条用户意图）。
		Pinned bool `json:"pinned,omitempty"`
		// Token 为会话级 token 用量合计（P2.13）；未装配四期 Store 时省略。
		Token *session.TokenUsage `json:"token,omitempty"`
	}
	// 置顶会话始终排最前；置顶集合内部与非置顶集合内部均按最近活跃降序。
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].Pinned != list[j].Pinned {
			return list[i].Pinned
		}
		return list[i].LastModified.After(list[j].LastModified)
	})
	ownerNames := map[string]string{}
	if s.ownerNames != nil && principal.canReadAllSessions() {
		// 展示名解析失败只影响标签，不能阻断列表；前端会用稳定 ownerId 兜底。
		if resolved, err := s.ownerNames(r.Context()); err == nil {
			ownerNames = resolved
		}
	}
	out := make([]item, 0, len(list))
	for _, sess := range list {
		if s.platform != nil && !principal.canReadAllSessions() {
			full, e := s.chat.GetSession(sess.ID)
			if e != nil || !s.covered(r.Context(), full, principal.User.ID) {
				out = append(out, item{ID: sess.ID, Name: "权限已变化", State: "restricted", LastModified: sess.LastModified})
				continue
			}
		}
		out = append(out, item{
			ID:   sess.ID,
			Name: sessionTitle(sess), ClusterID: sess.Cluster, OwnerID: sess.UserID,
			OwnerName:    ownerNames[sess.UserID],
			State:        sess.State,
			Model:        sess.Model,
			MessageCount: sess.TotalMessages(),
			LastModified: sess.LastModified,
			Pinned:       sess.Pinned,
			Token:        s.sessionToken(sess.ID),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ownedSession 允许会话 owner 或持有 chat.manage.all 的 admin 操作会话。
func (s *Server) ownedSession(r *http.Request, id string) (*session.Session, error) {
	principal, ok := principalFrom(r)
	if !ok {
		return nil, session.ErrForbidden
	}
	sess, err := s.chat.GetSession(id)
	if err != nil {
		return nil, err
	}
	if !principal.canManageAllSessions() && sess.UserID != principal.User.ID {
		return nil, session.ErrNotFound
	}
	return sess, nil
}

// createOwnedSession 创建或获取会话。admin 可通过已存在的 session ID
// 获取任意会话并继续操作。
func (s *Server) createOwnedSession(r *http.Request, id, cluster string) (*session.Session, error) {
	principal, ok := principalFrom(r)
	if !ok {
		return nil, session.ErrForbidden
	}
	sess, err := s.chat.GetSession(id)
	if err == nil {
		if !principal.canManageAllSessions() && sess.UserID != principal.User.ID {
			return nil, session.ErrNotFound
		}
		if cluster != "" && cluster != sess.Cluster {
			return nil, session.ErrForbidden
		}
		return s.readableSession(r, id)
	}
	if !errors.Is(err, session.ErrNotFound) {
		return nil, err
	}
	if s.platform != nil {
		a, e := s.platform.Grants.EffectiveAccess(r.Context(), principal.User.ID, cluster)
		if e != nil || len(a.Coverage().Targets) == 0 {
			return nil, session.ErrForbidden
		}
		creator, ok := s.chat.(interface {
			NewClusterSession(string, string, string, string, []byte) (*session.Session, error)
		})
		if !ok {
			return nil, session.ErrForbidden
		}
		data, e := json.Marshal(a.Coverage())
		if e != nil {
			return nil, e
		}
		return creator.NewClusterSession(id, principal.User.ID, cluster, s.model, data)
	}
	sess, err = s.chat.GetOrCreateSessionForUser(id, principal.User.ID, "", s.model)
	if errors.Is(err, session.ErrForbidden) {
		return nil, session.ErrNotFound
	}
	return sess, err
}

// readableSession 允许 owner 读取；持有会话读取或管理权限的 admin
// 可读取任意历史与事件流。
func (s *Server) readableSession(r *http.Request, id string) (*session.Session, error) {
	principal, ok := principalFrom(r)
	if !ok {
		return nil, session.ErrForbidden
	}
	sess, err := s.chat.GetSession(id)
	if err != nil {
		return nil, err
	}
	if !principal.canReadAllSessions() && s.platform != nil && !s.covered(r.Context(), sess, principal.User.ID) {
		return nil, session.ErrNotFound
	}
	if principal.canReadAllSessions() || principal.canManageAllSessions() || sess.UserID == principal.User.ID {
		return sess, nil
	}
	return nil, session.ErrNotFound
}

func (s *Server) covered(ctx context.Context, sess *session.Session, uid string) bool {
	var c authz.Coverage
	if json.Unmarshal(sess.Coverage, &c) != nil || c.ClusterID != sess.Cluster {
		return false
	}
	a, err := s.platform.Grants.EffectiveAccess(ctx, uid, c.ClusterID)
	return err == nil && c.CoveredBy(a)
}
func writeSessionAccessError(w http.ResponseWriter, err error) {
	if errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrForbidden) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "session not found"})
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}

// sessionTitle 返回会话列表展示标题（P7.5）。P8.6 起判定逻辑搬到
// session.Session.DisplayTitle：列表走数据库索引列时正文缺席，标题必须能在
// 「读不到 messages」的情况下仍给出同一个结果，因此放在会话类型上、由写入侧
// 固化快照，而不是留在 Web 层现算。
func sessionTitle(sess session.Session) string {
	return sess.DisplayTitle()
}

// sessionToken 读取会话级 token 用量合计（P2.13）；未装配 Store 或读取失败时返回 nil，
// 由前端退化为"不展示统计"，避免把读不到当成 0。
func (s *Server) sessionToken(sessionID string) *session.TokenUsage {
	provider, ok := s.chat.(service.SessionTokenizer)
	if !ok {
		return nil
	}
	usage, err := provider.SessionTokenUsage(context.Background(), sessionID)
	if err != nil {
		slog.Warn("sessionapi: 读取会话 token 用量失败", "session", sessionID, "error", err)
		return nil
	}
	if usage.IsZero() {
		return nil
	}
	return &usage
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClusterID string `json:"clusterId"`
		ID        string `json:"id"`
		Name      string `json:"name"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body)
	sess, err := s.createOwnedSession(r, body.ID, body.ClusterID)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	// admin 经 chat.manage.all 续用他人会话同样要留痕。
	if !s.recordCrossUserAction(r, sess, actionAdminSessionRead, store.AuditEventSession, nil) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	if body.Name != "" && body.Name != sess.Name {
		if err := s.chat.RenameSession(sess.ID, body.Name); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		sess.Name = body.Name
	}
	writeJSON(w, http.StatusOK, sess)
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("name must not be empty"))
		return
	}
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	if !s.recordCrossUserAction(r, sess, actionAdminSessionRename, store.AuditEventSession, map[string]any{"name": body.Name}) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	if err := s.chat.RenameSession(id, body.Name); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "name": body.Name})
}

// handlePinSession 置顶/取消置顶会话（P7.5）。
func (s *Server) handlePinSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	if !s.recordCrossUserAction(r, sess, actionAdminSessionPin, store.AuditEventSession, map[string]any{"pinned": body.Pinned}) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	if err := s.chat.PinSession(id, body.Pinned); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "pinned": body.Pinned})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	if !s.recordCrossUserAction(r, sess, actionAdminSessionDelete, store.AuditEventSession, nil) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	if err := s.chat.DeleteSession(id); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// 审计在内存态清理前写：删除后已无法从会话里取回 owner。
	s.mu.Lock()
	if cancel, ok := s.runs[id]; ok {
		cancel()
	}
	delete(s.broadcasters, id)
	delete(s.pending, id)
	delete(s.runs, id)
	delete(s.eventSeq, id)
	delete(s.eventLogs, id)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// handleStream 以 SSE 推送会话事件：先发历史快照，再转发实时事件。
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	sess, err := s.readableSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	// admin 读取他人会话的历史快照/事件流属于跨用户读取，必须留痕。
	if !s.recordCrossUserAction(r, sess, actionAdminSessionRead, store.AuditEventSession, nil) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// 反向代理（Nginx 等）默认缓冲响应体，会把 SSE 攒成一段一段；显式关闭本响应的
	// 缓冲，代理配置遗漏时也能正常推流（P6.11）。
	w.Header().Set("X-Accel-Buffering", "no")
	b := s.broadcaster(id)
	ch := b.add()
	defer b.remove(ch)
	snapshot, _ := json.Marshal(map[string]any{
		"type":      "history",
		"sessionId": sess.ID,
		"state":     sess.State,
		"messages":  sess.Messages,
		// Token 为会话级累计用量（P2.13）：刷新/重开会话后右下角统计仍可读。
		"token": s.sessionToken(sess.ID),
	})
	fmt.Fprintf(w, "data: %s\n\n", snapshot)
	flusher.Flush()

	// 运行中回合的事件日志先补齐，再消费实时通道。客户端按 seq 去重，
	// 因此这里与快照之后到达的实时帧重叠也不会产生重复卡片。
	if sess.State == "running" || sess.State == "waiting" {
		s.mu.Lock()
		logs := append([]bufferedEvent(nil), s.eventLogs[id]...)
		s.mu.Unlock()
		for _, item := range logs {
			if !s.canStream(r, sess) {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", item.raw)
			flusher.Flush()
		}
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if !s.canStream(r, sess) {
				return
			}
		case <-r.Context().Done():
			return
		case msg := <-ch:
			if !s.canStream(r, sess) {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// handleSendMessage 发起一轮 Chat 并把事件广播到会话的 SSE 客户端。
func (s *Server) canStream(r *http.Request, sess *session.Session) bool {
	principal, ok := principalFrom(r)
	if !ok {
		return false
	}
	if principal.recheck != nil {
		user, role, err := principal.recheck(r.Context())
		if err != nil || user == nil || role == nil {
			return false
		}
		principal.User = user
		principal.Role = role
	}
	return s.platform == nil || principal.canReadAllSessions() || sess.UserID == principal.User.ID && s.covered(r.Context(), sess, principal.User.ID)
}
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Query string `json:"query"`
		// Skills 用户显式 @ 指定的 Skill 名称（P3.8.4），由 service 层强校验。
		Skills []string `json:"skills"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if body.Query == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query must not be empty"))
		return
	}
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	caller, _ := principalFrom(r)
	if caller.User == nil || sess.UserID != caller.User.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the session owner can perform this action"})
		return
	}
	var bound *service.BoundRun
	if s.platform != nil {
		var c authz.Coverage
		if json.Unmarshal(sess.Coverage, &c) != nil || c.ClusterID != sess.Cluster {
			writeError(w, 403, session.ErrForbidden)
			return
		}
		bound, err = s.platform.Bind(r.Context(), caller.User.ID, c, func(ctx context.Context) error {
			u, role, e := caller.recheck(ctx)
			if e != nil || u == nil || role == nil || u.ID != caller.User.ID {
				return session.ErrForbidden
			}
			return nil
		})
		if err != nil {
			writeError(w, 403, session.ErrForbidden)
			return
		}
	}
	handedOff := false
	defer func() {
		if bound != nil && !handedOff {
			bound.Close()
		}
	}()
	s.mu.Lock()
	if _, running := s.pending[id]; running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, fmt.Errorf("a chat is already running for this session"))
		return
	}
	respond := make(chan string, 1)
	chatCtx, cancelChat := context.WithCancel(context.Background())
	if bound != nil {
		go bound.Watch(chatCtx, cancelChat)
	}
	s.pending[id] = respond
	s.runs[id] = cancelChat
	s.mu.Unlock()

	// Chat 生命周期独立于 HTTP 请求：请求返回后 SSE 仍需持续推送。
	principal, ok := principalFrom(r)
	if !ok {
		cancelChat()
		s.mu.Lock()
		delete(s.pending, id)
		delete(s.runs, id)
		s.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	var accessScope *tools.AccessScope
	if s.scope != nil {
		resolved, err := s.scope(r.Context(), principal.User, principal.Role)
		if err != nil {
			cancelChat()
			s.mu.Lock()
			delete(s.pending, id)
			delete(s.runs, id)
			s.mu.Unlock()
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if resolved == nil {
			cancelChat()
			s.mu.Lock()
			delete(s.pending, id)
			delete(s.runs, id)
			s.mu.Unlock()
			writeError(w, 403, session.ErrForbidden)
			return
		}
		accessScope = resolved
		accessScope.Refresh = func(ctx context.Context) (*tools.AccessScope, error) {
			u, role, err := principal.recheck(ctx)
			if err != nil || u == nil || role == nil {
				return nil, session.ErrForbidden
			}
			return s.scope(ctx, u, role)
		}
	}
	var kubePath string
	var check func(*http.Request) error
	var targetCheck func(context.Context) error
	if bound != nil {
		targetCheck = func(ctx context.Context) error {
			_, e := bound.Access(ctx)
			return e
		}
		refresh := func(ctx context.Context) (*tools.AccessScope, error) {
			a, e := bound.Access(ctx)
			if e != nil {
				return nil, e
			}
			return &tools.AccessScope{Policy: a, ClusterID: a.ClusterID}, nil
		}
		accessScope = &tools.AccessScope{Policy: &authz.EffectiveAccess{ClusterID: sess.Cluster}, ClusterID: sess.Cluster, Refresh: refresh}
		if fresh, e := refresh(r.Context()); e == nil {
			accessScope.Policy = fresh.Policy
		}
		kubePath = bound.Path
		check = bound.CheckRequest
	}
	var confirm service.Confirmation
	if s.platform != nil {
		confirm = func(ctx context.Context, data sse.ApprovalData) (string, <-chan string, func(), error) {
			if s.audit == nil {
				return "", nil, nil, fmt.Errorf("audit unavailable")
			}
			key, ch, done := s.newConfirmation(id)
			raw, _ := json.Marshal(data)
			payload, _ := json.Marshal(map[string]any{"planSHA256": fmt.Sprintf("%x", sha256.Sum256(raw)), "sessionVersion": principal.User.SessionVersion, "credentialVersion": bound.CredentialVersion, "expiresAt": time.Now().UTC().Add(5 * time.Minute), "target": data.Target})
			if e := s.audit(ctx, store.AuditEvent{Action: "approval.request", EventType: store.AuditEventApproval, SessionID: id, UserID: principal.User.ID, ClusterID: sess.Cluster, ApprovalID: key, Payload: string(payload), Operation: data.Operation, Risk: data.Risk, Status: "pending"}); e != nil {
				done()
				return "", nil, nil, e
			}
			return key, ch, done, nil
		}
	}
	var coverageSnapshot func() []byte
	if bound != nil {
		coverageSnapshot = bound.CoverageSnapshot
	}
	approvalContext := ""
	if bound != nil {
		approvalContext = fmt.Sprintf("%x", sha256.Sum256([]byte(bound.CredentialVersion+":"+bearerToken(r))))
	}
	events, err := s.chat.Chat(chatCtx, service.ChatRequest{ApprovalContext: approvalContext, CoverageSnapshot: coverageSnapshot, Confirmation: confirm, KubeconfigPath: kubePath, RequestCheck: check, TargetCheck: targetCheck,
		SessionID:       id,
		Query:           body.Query,
		UserID:          principal.User.ID,
		AllowAnySession: principal.canManageAllSessions(),
		AccessScope:     accessScope,
		Skills:          body.Skills,
	}, respond)
	if err != nil {
		cancelChat()
		s.mu.Lock()
		delete(s.pending, id)
		delete(s.runs, id)
		s.mu.Unlock()
		if errors.Is(err, settings.ErrModelNotConfigured) {
			// 模型未配置是配置态问题，不是服务端故障：409 + 原提示，前端据此引导去
			// 配置中心「模型」分组，而不是报 500 让用户以为后端坏了（P6.1）。
			writeError(w, http.StatusConflict, err)
			return
		}
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	// admin 代他人发起对话是平台管理员例外能力（PRD §18），以该 admin 为操作者留痕；
	// 只记身份，不记 query 内容。
	s.recordCrossUserAction(r, sess, actionAdminSessionMessage, store.AuditEventSession, nil)
	b := s.broadcaster(id)
	handedOff = true
	go func() {
		if bound != nil {
			defer bound.Close()
		}
		defer cancelChat()
		defer func() {
			s.mu.Lock()
			delete(s.pending, id)
			delete(s.runs, id)
			s.mu.Unlock()
		}()
		for ev := range events {
			raw, err := json.Marshal(ev)
			if err != nil {
				slog.Warn("sessionapi: 事件序列化失败", "error", err)
				continue
			}
			b.broadcast(raw)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"sessionId": id})
}

// handleCancel 取消当前会话正在执行的 Chat 运行。
func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	s.mu.Lock()
	cancel, ok := s.runs[id]
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("no active run for session %s", id))
		return
	}
	if !s.recordCrossUserAction(r, sess, actionAdminSessionCancel, store.AuditEventSession, nil) {
		writeError(w, 503, fmt.Errorf("audit unavailable"))
		return
	}
	cancel()
	writeJSON(w, http.StatusOK, map[string]string{"sessionId": id})
}

// handleApprove 把审批选择回传给当前 Chat 的 respond 通道。
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		ApprovalID string `json:"approvalId"`
		Choice     string `json:"choice"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sess, err := s.ownedSession(r, id)
	if err != nil {
		writeSessionAccessError(w, err)
		return
	}
	caller, _ := principalFrom(r)
	if caller.User == nil || sess.UserID != caller.User.ID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "only the session owner can perform this action"})
		return
	}
	s.mu.Lock()
	respond, ok := s.pending[id]
	if s.platform != nil {
		ticket, exists := s.tickets[id]
		ok = ok && exists && body.ApprovalID == ticket.id && time.Now().Before(ticket.expires)
		if ok {
			delete(s.tickets, id)
			respond = ticket.reply
		}
	}
	s.mu.Unlock()
	if !ok {
		writeError(w, http.StatusConflict, fmt.Errorf("no pending approval for session %s", id))
		return
	}
	if s.platform != nil {
		decision := "confirmed"
		if body.Choice == "no" {
			decision = "rejected"
		}
		if s.audit == nil || s.audit(r.Context(), store.AuditEvent{Action: "approval.answer", EventType: store.AuditEventApproval, SessionID: id, ClusterID: sess.Cluster, UserID: caller.User.ID, ApprovalID: body.ApprovalID, Decision: decision, Status: "success"}) != nil {
			writeError(w, 503, fmt.Errorf("audit unavailable"))
			return
		}
	}
	select {
	case respond <- body.Choice:
		// 不回显 choice：Secret 值经此通道提交，响应体必须原样返回会话 id。
		// 同理，跨用户审批审计只记身份，绝不记 choice 值。
		s.recordCrossUserAction(r, sess, actionAdminSessionApprove, store.AuditEventApproval, nil)
		writeJSON(w, http.StatusOK, map[string]string{"sessionId": id})
	default:
		writeError(w, http.StatusConflict, fmt.Errorf("approval already answered"))
	}
}

func (s *Server) newConfirmation(sessionID string) (string, <-chan string, func()) {
	ticket := confirmationTicket{rand.Text(), time.Now().Add(5 * time.Minute), make(chan string, 1)}
	s.mu.Lock()
	s.tickets[sessionID] = ticket
	s.mu.Unlock()
	return ticket.id, ticket.reply, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.tickets[sessionID].id == ticket.id {
			delete(s.tickets, sessionID)
		}
	}
}
func (s *Server) broadcaster(id string) *Broadcaster {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.broadcasters[id]; ok {
		return b
	}
	b := newBroadcaster()
	s.broadcasters[id] = b
	return b
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
