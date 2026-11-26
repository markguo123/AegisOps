package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/auth"
)

func TestBrowserPortalPolicy(t *testing.T) {
	const origin = "https://platform.test"
	ctx := context.Background()
	f := newPlatformServer(t)
	enterprise, err := auth.NewEnterpriseLogin(ctx, f.authSvc, f.store.Settings(), nil, origin, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewBrowserGateway(f.authSvc, enterprise, nil, origin, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.LoadPortalSettings(ctx, f.store.Settings()); err != nil {
		t.Fatal(err)
	}

	var cookies []*http.Cookie
	var csrf string
	request := func(method, path, body, requestOrigin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+path, strings.NewReader(body))
		r.Header.Set("Origin", requestOrigin)
		r.Header.Set("X-CSRF-Token", csrf)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		g.Handler(http.NotFoundHandler()).ServeHTTP(w, r)
		return w
	}
	if w := request("POST", parentTokenLogin, `{"token":"test"}`, origin); w.Code != 401 || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("default Portal is not disabled: %d %s", w.Code, w.Body.String())
	}
	if w := request("POST", parentTokenLogin, `{"token":"test"}`, "https://other.test"); w.Code != 403 {
		t.Fatalf("cross-origin exchange returned %d", w.Code)
	}
	const login = "/api/v1/auth/login"
	w := request("POST", login, `{"username":"admin","password":"admin-pw"}`, origin)
	if w.Code != 200 {
		t.Fatalf("local login returned %d", w.Code)
	}
	cookies = w.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "__Host-aegisops_csrf" {
			csrf = c.Value
		}
	}
	const path = "/api/v1/settings/auth/portal"
	const portal = `{"enabled":true,"homeUrl":"https://portal.example.test"}`
	if w = request("PATCH", path, portal, origin); w.Code != 200 {
		t.Fatalf("enable Portal: %d %s", w.Code, w.Body.String())
	}
	if w = request("GET", "/api/v1/auth/providers", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"local":true`) || !strings.Contains(w.Body.String(), `"portalHomeUrl":"https://portal.example.test"`) {
		t.Fatalf("provider list: %d %s", w.Code, w.Body.String())
	}
	if w = request("PATCH", path, `{"enabled":true,"localDisabled":true,"homeUrl":"https://portal.example.test"}`, origin); w.Code != 409 {
		t.Fatalf("local admin could disable own only login method: %d", w.Code)
	}
	reload, err := NewBrowserGateway(f.authSvc, enterprise, nil, origin, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := reload.LoadPortalSettings(ctx, f.store.Settings()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := reload.portalConfig()
	if !cfg.Enabled || cfg.LocalDisabled || cfg.CheckTokenURL != "https://portal.example.test/api/authcenter/auth/checktoken" {
		t.Fatalf("Portal configuration was not persisted: %+v", cfg)
	}
}
