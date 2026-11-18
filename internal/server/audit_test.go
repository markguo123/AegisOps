package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/permission"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// auditSentinel 是写进 payload 的敏感值哨兵：审计查询接口只回字段白名单，
// 任何响应体里出现它都意味着直接透传了存储层 Payload。
const auditSentinel = "SECRET-AUDIT-XYZ-9f2b"

type auditQueryEnv struct {
	ts      *httptest.Server
	st      store.Store
	authSvc *auth.Service
}

func newAuditQueryEnv(t *testing.T) *auditQueryEnv {
	t.Helper()
	st, err := store.OpenSQLite("file:server-audit-" + time.Now().Format("150405.000000000") + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	authSvc := auth.New(st.Users(), st.Roles())
	if err := authSvc.Bootstrap(context.Background(), "admin-pw"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	handler := NewWithAuth(&fakeScan{}, authSvc).WithAuditQuery(service.NewAuditQueryService(st)).Handler()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return &auditQueryEnv{ts: ts, st: st, authSvc: authSvc}
}

func (e *auditQueryEnv) loginToken(t *testing.T, username, password string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, env, payload := login(t, e.ts, string(body))
	if resp.StatusCode != http.StatusOK || env.Error != nil {
		t.Fatalf("%s 登录失败: %d %+v", username, resp.StatusCode, env.Error)
	}
	token, _ := payload["token"].(string)
	if token == "" {
		t.Fatalf("%s 登录响应缺少 token", username)
	}
	return token
}

func (e *auditQueryEnv) roleByName(t *testing.T, name string) *store.Role {
	t.Helper()
	roles, err := e.st.Roles().List(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, r := range roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("角色 %s 不存在", name)
	return nil
}

// createUser 直接写入用户（不走 /users 端点，避免用例被用户管理权限绑定）。
func (e *auditQueryEnv) createUser(t *testing.T, username, password, roleName string) *store.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	role := e.roleByName(t, roleName)
	u := &store.User{
		ID:           "u-" + username,
		Username:     username,
		PasswordHash: hash,
		RoleID:       role.ID,
		Status:       "active",
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := e.st.Users().Create(context.Background(), u); err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	return u
}

// setRolePermissions 改内置角色权限，用于验证网关按"当前权限"判定而不是按角色名。
func (e *auditQueryEnv) setRolePermissions(t *testing.T, roleName string, perms ...string) {
	t.Helper()
	role := e.roleByName(t, roleName)
	role.Permissions = perms
	if err := e.st.Roles().Update(context.Background(), role); err != nil {
		t.Fatalf("update role %s: %v", roleName, err)
	}
}

func (e *auditQueryEnv) adminUserID(t *testing.T) string {
	t.Helper()
	users, err := e.st.Users().List(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	for _, u := range users {
		if u.Username == auth.DefaultAdminUsername {
			return u.ID
		}
	}
	t.Fatal("内置 admin 用户不存在")
	return ""
}

type auditSeed struct {
	events []*store.AuditEvent
	base   time.Time
}

// seedAuditEvents 写一组时间戳受控的事件：审批 a1 把工具执行与人工确认串起来，
// payload 里带哨兵值用于验证查询接口不透传存储层字段。
func seedAuditEvents(t *testing.T, st store.Store, adminID string) auditSeed {
	t.Helper()
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	events := []*store.AuditEvent{
		{
			Timestamp: base, Action: "auth.login", EventType: store.AuditEventLogin,
			UserID: adminID, ClusterID: "c1", Status: "success",
			Payload: "{\"username\":\"admin\"}",
		},
		{
			Timestamp: base.Add(1 * time.Second), Action: "tool-call.request", EventType: store.AuditEventToolCall,
			UserID: adminID, ClusterID: "c1", RequestID: "r1", SessionID: "s1", TurnID: "t1", ApprovalID: "a1",
			ResourceKind: "Pod", ResourceName: "default/api-0", Risk: "high", Status: "pending",
			ParameterSummary: "{\"command\":\"kubectl delete pod api-0\"}",
		},
		{
			Timestamp: base.Add(2 * time.Second), Action: "approval.decision", EventType: store.AuditEventApproval,
			UserID: adminID, ClusterID: "c1", RequestID: "r1", SessionID: "s1", TurnID: "t1", ApprovalID: "a1",
			Decision: "approved", Risk: "high", Status: "success",
			ResultSummary: "{\"decision\":\"approved\"}",
		},
		{
			Timestamp: base.Add(3 * time.Second), Action: "execution.failed", EventType: store.AuditEventResourceMutation,
			UserID: adminID, ClusterID: "c2", SessionID: "s1", TurnID: "t1", ApprovalID: "a1",
			ResourceKind: "Pod", ResourceName: "default/api-0", Status: "error",
			ResultSummary: "{\"error\":\"forbidden\"}",
		},
		{
			Timestamp: base.Add(4 * time.Second), Action: store.ActionConfigChange, EventType: store.AuditEventConfigChange,
			UserID: adminID, ClusterID: "c2", Status: "success",
			Payload: "{\"api_key\":\"" + auditSentinel + "\"}",
		},
	}
	for _, e := range events {
		if err := st.Audit().Write(context.Background(), e); err != nil {
			t.Fatalf("seed audit event %q: %v", e.Action, err)
		}
	}
	return auditSeed{events: events, base: base}
}

func (e *auditQueryEnv) get(t *testing.T, path, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.ts.URL+path, nil)
	if token != "" {
		req.Header.Set(HeaderAuthorization, "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp, string(raw)
}

func TestAuditEventsRequireAuditReadPermission(t *testing.T) {
	env := newAuditQueryEnv(t)
	seedAuditEvents(t, env.st, env.adminUserID(t))

	// 未登录：401，而不是空列表。
	resp, _ := env.get(t, "/api/v1/audit/events", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未登录访问审计列表应 401, got %d", resp.StatusCode)
	}

	// 普通 user 角色默认没有 audit.read：403。
	env.createUser(t, "plain", "plain-pw-123", auth.RoleUser)
	plainToken := env.loginToken(t, "plain", "plain-pw-123")
	resp, _ = env.get(t, "/api/v1/audit/events", plainToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 audit.read 的用户应 403, got %d", resp.StatusCode)
	}

	// 同一 Token 在授予 audit.read 后立即生效：网关按当前权限判定，不缓存角色名。
	env.setRolePermissions(t, auth.RoleUser, permission.AuditRead)
	resp, raw := env.get(t, "/api/v1/audit/events", plainToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("授予 audit.read 后应 200, got %d body=%s", resp.StatusCode, raw)
	}

	// admin 的 "*" 天然满足。
	adminToken := env.loginToken(t, auth.DefaultAdminUsername, "admin-pw")
	resp, _ = env.get(t, "/api/v1/audit/events", adminToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin 访问审计列表应 200, got %d", resp.StatusCode)
	}
}

func TestAuditEventsListFiltersAndCursor(t *testing.T) {
	env := newAuditQueryEnv(t)
	seed := seedAuditEvents(t, env.st, env.adminUserID(t))
	token := env.loginToken(t, auth.DefaultAdminUsername, "admin-pw")

	// 第一页：2 条，还有下一页。
	resp, raw := env.get(t, "/api/v1/audit/events?limit=2", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("列表应 200, got %d body=%s", resp.StatusCode, raw)
	}
	first := decodeAuditList(t, raw)
	if len(first.Items) != 2 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("第一页契约不符: %+v", first)
	}
	// 默认 timestamp DESC, id DESC：最新的两条是 config.change 与 execution.failed。
	if first.Items[0].EventType != store.AuditEventConfigChange || first.Items[1].EventType != store.AuditEventResourceMutation {
		t.Fatalf("第一页排序不符: %+v", first.Items)
	}
	if first.Items[0].UserName != "Administrator" {
		t.Errorf("操作者名称未解析: %q", first.Items[0].UserName)
	}

	// 第二页：游标翻页，不重复上一页末行。
	second := decodeAuditList(t, mustAuditGet(t, env, "/api/v1/audit/events?limit=2&cursor="+url.QueryEscape(first.NextCursor), token))
	if len(second.Items) != 2 {
		t.Fatalf("第二页应有 2 条, got %d", len(second.Items))
	}
	for _, a := range first.Items {
		for _, b := range second.Items {
			if a.ID == b.ID {
				t.Fatalf("游标翻页重复返回 id=%d", a.ID)
			}
		}
	}
	if !second.HasMore {
		t.Fatal("第二页之后还有一条，hasMore 应为 true")
	}
	third := decodeAuditList(t, mustAuditGet(t, env, "/api/v1/audit/events?limit=2&cursor="+url.QueryEscape(second.NextCursor), token))
	if len(third.Items) != 1 || third.HasMore || third.NextCursor != "" {
		t.Fatalf("最后一页契约不符: %+v", third)
	}

	filters := []struct {
		name  string
		query string
		want  int
	}{
		{"按事件类型", "eventType=" + store.AuditEventApproval, 1},
		{"按结果状态", "status=error", 1},
		{"按集群", "clusterId=c2", 2},
		{"按会话", "sessionId=s1", 3},
		{"按动作", "action=approval.decision", 1},
		{
			"按时间范围",
			"from=" + url.QueryEscape(seed.base.Add(2*time.Second).Format(time.RFC3339)) +
				"&to=" + url.QueryEscape(seed.base.Add(3*time.Second).Format(time.RFC3339)),
			2,
		},
	}
	for _, tc := range filters {
		got := decodeAuditList(t, mustAuditGet(t, env, "/api/v1/audit/events?limit=100&"+tc.query, token))
		if len(got.Items) != tc.want {
			t.Errorf("%s: 事件数 = %d, want %d", tc.name, len(got.Items), tc.want)
		}
	}

	for _, bad := range []string{
		"/api/v1/audit/events?limit=0",
		"/api/v1/audit/events?cursor=not-base64",
		"/api/v1/audit/events?from=yesterday",
		"/api/v1/audit/events?from=" + url.QueryEscape("2026-11-19T00:00:00Z") + "&to=" + url.QueryEscape("2026-11-18T00:00:00Z"),
	} {
		resp, body := env.get(t, bad, token)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s 应 400, got %d body=%s", bad, resp.StatusCode, body)
		}
	}
}

func TestAuditEventDetailTimelineAndRedaction(t *testing.T) {
	env := newAuditQueryEnv(t)
	seed := seedAuditEvents(t, env.st, env.adminUserID(t))
	token := env.loginToken(t, auth.DefaultAdminUsername, "admin-pw")

	// 工具请求事件（第二条）的详情：审批 a1 关联出提议/确认/执行三条，按时间正序。
	detailPath := "/api/v1/audit/events/" + strconv.FormatInt(seed.events[1].ID, 10)
	raw := mustAuditGet(t, env, detailPath, token)
	var detail service.AuditDetailResult
	if err := json.Unmarshal(auditEnvelopeData(t, raw), &detail); err != nil {
		t.Fatalf("解详情失败: %v", err)
	}
	if detail.Event.ID != seed.events[1].ID || detail.Event.EventType != store.AuditEventToolCall {
		t.Fatalf("详情事件不符: %+v", detail.Event)
	}
	if detail.Event.ParameterSummary == "" {
		t.Fatal("详情应带已脱敏的参数摘要")
	}
	if len(detail.Timeline) != 3 {
		t.Fatalf("审批关联时间线应有 3 条, got %d: %+v", len(detail.Timeline), detail.Timeline)
	}
	if detail.Timeline[0].ID != seed.events[1].ID || detail.Timeline[2].ID != seed.events[3].ID {
		t.Fatalf("时间线应按时间正序: %+v", detail.Timeline)
	}

	// 红线：查询接口只回字段白名单，存储层 Payload 与其中的敏感值一律不出现在响应里。
	for _, path := range []string{
		"/api/v1/audit/events?limit=100",
		"/api/v1/audit/events/" + strconv.FormatInt(seed.events[4].ID, 10),
	} {
		body := mustAuditGet(t, env, path, token)
		if strings.Contains(body, auditSentinel) {
			t.Fatalf("%s 泄漏 payload 敏感值", path)
		}
		if strings.Contains(body, "\"payload\"") {
			t.Fatalf("%s 直接透传了存储层 Payload 字段", path)
		}
	}

	// 不存在的事件：404；非法 id：400。
	if resp, _ := env.get(t, "/api/v1/audit/events/999999", token); resp.StatusCode != http.StatusNotFound {
		t.Errorf("不存在的事件应 404, got %d", resp.StatusCode)
	}
	if resp, _ := env.get(t, "/api/v1/audit/events/abc", token); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("非法事件 id 应 400, got %d", resp.StatusCode)
	}
}

// ---- 用例内小工具 ----

func mustAuditGet(t *testing.T, env *auditQueryEnv, path, token string) string {
	t.Helper()
	resp, raw := env.get(t, path, token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s 应 200, got %d body=%s", path, resp.StatusCode, raw)
	}
	return raw
}

func decodeAuditList(t *testing.T, raw string) service.AuditListResult {
	t.Helper()
	var out service.AuditListResult
	if err := json.Unmarshal(auditEnvelopeData(t, raw), &out); err != nil {
		t.Fatalf("解列表失败: %v", err)
	}
	return out
}

// auditEnvelopeData 从统一信封里取出 data 原文（保持字段类型不变）。
func auditEnvelopeData(t *testing.T, raw string) []byte {
	t.Helper()
	var env apiResponse
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("解析信封失败: %v raw=%s", err, raw)
	}
	if env.Error != nil {
		t.Fatalf("响应携带错误: %+v", env.Error)
	}
	out, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("data 序列化失败: %v", err)
	}
	return out
}
