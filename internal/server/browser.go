package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

type failedLogin struct {
	count int
	until time.Time
}
type BrowserGateway struct {
	auth       *auth.Service
	enterprise *auth.EnterpriseLogin
	audit      auditWriter
	publicURL  string
	dev        bool
	quickstart bool
	key        [32]byte
	mu         sync.Mutex
	fails      map[string]failedLogin
}

func NewBrowserGateway(a *auth.Service, e *auth.EnterpriseLogin, audit auditWriter, origin string, dev bool, quickstart ...bool) (*BrowserGateway, error) {
	g := &BrowserGateway{auth: a, enterprise: e, audit: audit, publicURL: strings.TrimSuffix(origin, "/"), dev: dev, fails: map[string]failedLogin{}}
	g.quickstart = len(quickstart) > 0 && quickstart[0]
	_, err := rand.Read(g.key[:])
	return g, err
}
func (g *BrowserGateway) csrf(token string) string {
	mac := hmac.New(sha256.New, g.key[:])
	mac.Write([]byte(token))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func (g *BrowserGateway) secure(r *http.Request) bool {
	host := r.URL.Hostname()
	if host == "" {
		u := url.URL{Host: r.Host}
		host = u.Hostname()
	}
	// Never trust a caller-supplied X-Forwarded-Proto. A configured public URL
	// retains Secure cookies behind a TLS-terminating production proxy.
	return !(r.TLS == nil && g.publicURL == "" && (g.quickstart ||
		(g.dev && (host == "localhost" || net.ParseIP(host).IsLoopback()))))
}
func (g *BrowserGateway) name(r *http.Request, key string) string {
	if g.secure(r) {
		return "__Host-aegisops_" + key
	}
	return "aegisops_dev_" + key
}
func (g *BrowserGateway) cookie(w http.ResponseWriter, r *http.Request, key, value string, age int, private bool) {
	http.SetCookie(w, &http.Cookie{Name: g.name(r, key), Value: value, Path: "/", Secure: g.secure(r), HttpOnly: private, SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (g *BrowserGateway) signin(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	age := int(time.Until(sess.ExpiresAt).Seconds())
	if age < 1 {
		age = 1
	}
	g.cookie(w, r, "session", sess.Token, age, true)
	g.cookie(w, r, "csrf", g.csrf(sess.Token), age, false)
}
func (g *BrowserGateway) originOK(r *http.Request) bool {
	origin := g.publicURL
	if origin == "" {
		scheme := "https"
		if !g.secure(r) {
			scheme = "http"
		}
		origin = scheme + "://" + r.Host
	}
	return r.Header.Get("Origin") == origin
}
func (g *BrowserGateway) rateLimited(keys []string, limits []int, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, v := range g.fails {
		if !now.Before(v.until) {
			delete(g.fails, k)
		}
	}
	if len(g.fails) >= 10000 {
		return true
	}
	for i, k := range keys {
		if g.fails[k].count >= limits[i] {
			return true
		}
	}
	for i, k := range keys {
		v := g.fails[k]
		if v.count == 0 {
			period := time.Minute
			if i == 1 {
				period = 5 * time.Minute
			}
			v.until = now.Add(period)
		}
		v.count++
		g.fails[k] = v
	}
	return false
}
func (g *BrowserGateway) record(r *http.Request, user, action string) error {
	if g.audit == nil {
		return nil
	}
	return g.audit.Record(r.Context(), store.AuditEvent{UserID: user, Timestamp: time.Now().UTC(), Action: action, EventType: store.EventTypeForAction(action), Status: "success", RequestID: RequestIDFrom(r.Context())})
}
func (g *BrowserGateway) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		// Reject ambiguous API paths before the inner ServeMux canonicalizes them.
		if strings.Contains(r.URL.EscapedPath(), "%") || strings.Contains(r.URL.Path, "//") || strings.Contains(r.URL.Path, "/../") || strings.Contains(r.URL.Path, "/./") {
			http.Error(w, "invalid API path", 400)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		token := ""
		if c, err := r.Cookie(g.name(r, "session")); err == nil {
			token = c.Value
		}
		mutation := r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS"
		if mutation && (!g.originOK(r) || (r.URL.Path != "/api/v1/auth/login" && (token == "" || !hmac.Equal([]byte(g.csrf(token)), []byte(r.Header.Get("X-CSRF-Token")))))) {
			writeError(w, r, 403, codeForbidden, "request origin or CSRF token is invalid")
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/auth/providers":
			c, _, _ := g.enterprise.Config()
			writeOK(w, r, 200, map[string]any{"local": true, "oidc": c.Enabled, "displayName": c.DisplayName})
			return
		case "POST /api/v1/auth/login":
			var body struct{ Username, Password string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body) != nil {
				writeError(w, r, 400, codeInvalidArgument, "invalid credentials")
				return
			}
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			keys := []string{"ip/" + ip, "account/" + strings.ToLower(body.Username)}
			if g.rateLimited(keys, []int{30, 5}, time.Now()) {
				writeError(w, r, 429, "RATE_LIMITED", "too many login attempts")
				return
			}
			sess, err := g.auth.Login(r.Context(), body.Username, body.Password)
			if err != nil {
				if g.audit != nil {
					_ = g.audit.Record(r.Context(), store.AuditEvent{Action: store.ActionAuthLogin, EventType: store.EventTypeForAction(store.ActionAuthLogin), Status: "error", RequestID: RequestIDFrom(r.Context()), ResourceKind: "auth", ResultSummary: "local authentication failed"})
				}
				writeError(w, r, 401, codeUnauthorized, "invalid credentials")
				return
			}
			g.mu.Lock()
			delete(g.fails, keys[1])
			g.mu.Unlock()
			if err = g.record(r, sess.UserID, store.ActionAuthLogin); err != nil {
				g.auth.Logout(sess.Token)
				writeError(w, r, 500, codeInternal, "audit unavailable")
				return
			}
			g.signin(w, r, sess)
			writeOK(w, r, 200, map[string]any{"expiresAt": sess.ExpiresAt, "user": sess.User})
			return
		case "GET /api/v1/auth/oidc/start":
			g.begin(w, r, nil)
			return
		case "GET /api/v1/auth/oidc/callback":
			c, err := r.Cookie(g.name(r, "login"))
			g.cookie(w, r, "login", "", -1, true)
			if err != nil {
				writeError(w, r, 401, codeUnauthorized, "login expired")
				return
			}
			sess, err := g.enterprise.Finish(r.Context(), c.Value, r.URL.Query().Get("state"), r.URL.Query().Get("code"), token)
			if err != nil {
				reason, hint := enterpriseLoginHint(err)
				// 根因只进日志（含 IdP 返回的协议错误，已去凭据），审计里只留粗粒度原因。
				slog.Warn("server: 企业登录失败", "request_id", RequestIDFrom(r.Context()), "reason", reason, "error", err)
				if g.audit != nil {
					_ = g.audit.Record(r.Context(), store.AuditEvent{Action: store.ActionAuthLogin, EventType: store.EventTypeForAction(store.ActionAuthLogin), Status: "error", RequestID: RequestIDFrom(r.Context()), ResourceKind: "auth", ResultSummary: "enterprise authentication failed: " + reason})
				}
				status, code := http.StatusUnauthorized, codeUnauthorized
				if reason == "failed" {
					// 不是登录流程本身的失败（存储、角色解析等），别用 401 误导排查。
					status, code = http.StatusInternalServerError, codeInternal
				}
				writeError(w, r, status, code, hint)
				return
			}
			if err = g.record(r, sess.UserID, store.ActionAuthLogin); err != nil {
				g.auth.Logout(sess.Token)
				writeError(w, r, 500, codeInternal, "audit unavailable")
				return
			}
			g.signin(w, r, sess)
			http.Redirect(w, r, "/app/chat", 303)
			return
		}
		u, role, err := g.auth.UserByToken(r.Context(), token)
		if err != nil || role == nil {
			writeError(w, r, 401, codeUnauthorized, "authentication required")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/users/") && slices.Contains(strings.Split(r.URL.Path, "/"), "tokens") {
			writeError(w, r, 501, "FEATURE_NOT_ENABLED", "machine credentials are disabled")
			return
		}
		if r.URL.Path == "/api/v1/settings/auth" || r.URL.Path == "/api/v1/settings/auth/test" {
			if role.Name != auth.RoleAdmin {
				writeError(w, r, 403, codeForbidden, "administrator required")
				return
			}
			if r.Method == "PATCH" && r.URL.Path == "/api/v1/settings/auth" || r.Method == "POST" && r.URL.Path == "/api/v1/settings/auth/test" {
				var c auth.ProviderConfig
				if json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&c) != nil {
					writeError(w, r, 400, codeInvalidArgument, "invalid provider configuration")
					return
				}
				if err := g.record(r, u.ID, "auth.provider.change.request"); err != nil {
					writeError(w, r, 500, codeInternal, "audit unavailable")
					return
				}
				if err := g.enterprise.Configure(r.Context(), c, r.Method == "POST"); err != nil {
					writeError(w, r, 400, codeInvalidArgument, err.Error())
					return
				}
			} else if r.Method != "GET" {
				w.WriteHeader(405)
				return
			}
			c, has, callback := g.enterprise.Config()
			writeOK(w, r, 200, map[string]any{"provider": c, "hasSecret": has, "callback": callback})
			return
		}
		if r.URL.Path == "/api/v1/auth/oidc/bind" && r.Method == "GET" {
			linked, err := g.auth.HasExternalIdentity(r.Context(), u.ID)
			if err != nil {
				writeError(w, r, 500, codeInternal, "identity unavailable")
				return
			}
			cfg, _, _ := g.enterprise.Config()
			writeOK(w, r, 200, map[string]bool{"linked": linked, "local": u.PasswordHash != "", "enabled": cfg.Enabled})
			return
		}
		if r.URL.Path == "/api/v1/auth/oidc/bind" && (r.Method == "POST" || r.Method == "DELETE") {
			var body struct{ Password string }
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body) != nil {
				writeError(w, r, 400, codeInvalidArgument, "password required")
				return
			}
			verified, err := g.auth.Authenticate(r.Context(), u.Username, body.Password)
			if err != nil || verified.SessionVersion != u.SessionVersion {
				writeError(w, r, 401, codeUnauthorized, "password verification failed")
				return
			}
			if err := g.record(r, u.ID, "auth.identity."+strings.ToLower(r.Method)+".request"); err != nil {
				writeError(w, r, 503, codeUnavailable, "audit unavailable")
				return
			}
			if r.Method == "DELETE" {
				if err := g.auth.Unlink(r.Context(), u); err != nil {
					writeError(w, r, 409, codeConflict, "unlink failed")
					return
				}
				writeOK(w, r, 200, true)
				return
			}
			g.begin(w, r, u)
			return
		}
		if r.Method == "POST" && r.URL.Path == "/api/v1/auth/logout" {
			g.cookie(w, r, "session", "", -1, true)
			g.cookie(w, r, "csrf", "", -1, false)
		}
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		next.ServeHTTP(w, r)
	})
}
func (g *BrowserGateway) begin(w http.ResponseWriter, r *http.Request, bind *store.User) {
	var token string
	if c, e := r.Cookie(g.name(r, "session")); e == nil {
		token = c.Value
	}
	key, redirect, err := g.enterprise.Begin(r.Context(), bind, token)
	if err != nil {
		slog.Warn("server: 发起企业登录失败", "request_id", RequestIDFrom(r.Context()), "error", err)
		writeError(w, r, 503, codeInternal, "enterprise login unavailable")
		return
	}
	// 回调 cookie 的存活时间要盖住服务端登录事务的 5 分钟，否则浏览器先丢 key。
	g.cookie(w, r, "login", key, 300, true)
	if bind != nil {
		writeOK(w, r, 200, map[string]string{"redirect": redirect})
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// enterpriseLoginHint 把企业登录失败分成「原因 + 可执行提示」。原因进服务端日志和审计，
// 提示回到浏览器：企业 IdP 对接的失败大多不是密码错，而是回调地址、客户端凭据、根 CA、
// groups 准入或登录超时，光一句 enterprise login failed 用户只能瞎猜。
func enterpriseLoginHint(err error) (reason, hint string) {
	switch {
	case errors.Is(err, auth.ErrLoginTxInvalid):
		return "login-expired", "enterprise login expired or was already used; start the login again and finish the enterprise page within 5 minutes"
	case errors.Is(err, auth.ErrLoginStateMismatch):
		return "state-mismatch", "enterprise login state mismatch; start the login again"
	case errors.Is(err, auth.ErrLoginExchange):
		return "token-exchange-rejected", "the enterprise identity provider rejected the token exchange; check client id, client secret and the registered callback URL"
	case errors.Is(err, auth.ErrLoginIDToken):
		return "id-token-rejected", "the enterprise identity token failed validation; check issuer, audience and the clock difference with the provider"
	case errors.Is(err, auth.ErrLoginGroupDenied):
		return "group-denied", "your enterprise account groups are not allowed to sign in; ask an administrator to adjust the admission rule"
	case errors.Is(err, auth.ErrLoginConfigChanged):
		return "config-changed", "enterprise login configuration changed during the login; start the login again"
	case errors.Is(err, auth.ErrLoginNotReady):
		return "not-configured", "enterprise login is not configured on this platform"
	default:
		return "failed", "enterprise login could not be completed on the platform side; check the platform log with this request id"
	}
}
