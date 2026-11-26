package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

const parentTokenLogin = "/api/v1/login/parentToken"

func (g *BrowserGateway) LoadPortalSettings(ctx context.Context, settings store.SettingsStore) error {
	g.portalStore = settings
	saved, err := settings.Get(ctx, "auth", auth.PortalSettingsKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(saved.Value), &g.portal); err != nil {
		return err
	}
	return g.portal.Validate()
}

func (g *BrowserGateway) portalConfig() (auth.PortalConfig, uint64) {
	g.loginPolicy.RLock()
	defer g.loginPolicy.RUnlock()
	return g.portal, g.portalRevision
}

func (g *BrowserGateway) handlePortalSettings(w http.ResponseWriter, r *http.Request, u *store.User, role *store.Role, token string) {
	if role.Name != auth.RoleAdmin {
		writeError(w, r, 403, codeForbidden, "administrator required")
		return
	}
	g.loginPolicy.Lock()
	defer g.loginPolicy.Unlock()
	switch r.Method {
	case http.MethodGet:
	case http.MethodPatch:
		var c auth.PortalConfig
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&c); err != nil {
			writeError(w, r, 400, codeInvalidArgument, "invalid Portal configuration")
			return
		}
		if err := c.Validate(); err != nil {
			writeError(w, r, 400, codeInvalidArgument, err.Error())
			return
		}
		public, publicErr := url.Parse(g.publicURL)
		if c.Enabled && (publicErr != nil || public.Scheme != "https" || public.Host == "") {
			writeError(w, r, 400, codeInvalidArgument, "an HTTPS platform.server.public_url is required for Portal login")
			return
		}
		if c.LocalDisabled && c != g.portal {
			oidc, _, _ := g.enterprise.Config()
			source := g.auth.SessionSource(token)
			portalVerified := source == "portal" && c.Enabled && g.portal.Enabled &&
				c.HomeURL == g.portal.HomeURL && c.CheckTokenURL == g.portal.CheckTokenURL
			if !portalVerified && !(source == "oidc" && oidc.Enabled) {
				writeError(w, r, 409, codeConflict, "sign in as an administrator using an unchanged, enabled external login method, or keep local login enabled")
				return
			}
		}
		if g.portalStore == nil || g.record(r, u.ID, "auth.portal.change.request") != nil {
			writeError(w, r, 503, codeUnavailable, "cannot save Portal configuration")
			return
		}
		raw, err := json.Marshal(c)
		if err != nil || g.portalStore.Set(r.Context(), &store.Settings{Group: "auth", Key: auth.PortalSettingsKey, Value: string(raw)}) != nil {
			writeError(w, r, 503, codeUnavailable, "cannot save Portal configuration")
			return
		}
		g.portal = c
		g.portalRevision++
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	entry := ""
	if g.publicURL != "" {
		entry = g.publicURL + "/"
	}
	writeOK(w, r, 200, map[string]any{"portal": g.portal, "entry": entry})
}

func (g *BrowserGateway) finishPortal(w http.ResponseWriter, r *http.Request, previousToken string) {
	failure := func(reason string) {
		if g.audit != nil {
			_ = g.audit.Record(r.Context(), store.AuditEvent{Action: store.ActionAuthLogin, EventType: store.AuditEventLogin, Status: "error", RequestID: RequestIDFrom(r.Context()), ResourceKind: "auth", ResultSummary: "Portal login: " + reason})
		}
		status, code := http.StatusUnauthorized, codeUnauthorized
		switch reason {
		case "unavailable":
			status, code = http.StatusServiceUnavailable, codeUnavailable
		case "rate-limit":
			status, code = http.StatusTooManyRequests, "RATE_LIMITED"
		}
		writeError(w, r, status, code, reason)
	}
	cfg, revision := g.portalConfig()
	if !cfg.Enabled {
		failure("disabled")
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&body) != nil {
		failure("rejected")
		return
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if g.rateLimited([]string{"portal/ip/" + ip}, []int{120}, time.Now()) {
		failure("rate-limit")
		return
	}
	issue, err := g.auth.VerifyPortalLogin(r.Context(), cfg, body.Token)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			failure("rejected")
		} else {
			failure("unavailable")
		}
		return
	}
	g.loginPolicy.RLock()
	defer g.loginPolicy.RUnlock()
	if revision != g.portalRevision {
		failure("changed")
		return
	}
	sess, err := issue(r.Context())
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			failure("rejected")
		} else {
			failure("unavailable")
		}
		return
	}
	if err := g.record(r, sess.UserID, store.ActionAuthLogin); err != nil {
		_ = g.auth.Logout(sess.Token)
		failure("unavailable")
		return
	}
	if previousToken != "" {
		_ = g.auth.Logout(previousToken)
	}
	g.signin(w, r, sess)
	writeOK(w, r, 200, map[string]any{"user": sess.User, "expiresAt": sess.ExpiresAt})
}
