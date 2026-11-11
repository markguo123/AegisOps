package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/service"
)

func TestQuickstartCookieTransportBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, url, public       string
		quick, loopback, secure bool
	}{
		{"node-opt-in", "http://192.0.2.1:31080", "", true, false, false},
		{"production-node", "http://192.0.2.1:31080", "", false, false, true},
		{"loopback-does-not-enable-node", "http://192.0.2.1:31080", "", false, true, true},
		{"loopback", "http://127.0.0.1:18080", "", false, true, false},
		{"tls-stays-secure", "https://example.test", "", true, false, true},
		{"proxy-stays-secure", "http://backend:8080", "https://example.test", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &BrowserGateway{publicURL: tc.public, dev: tc.loopback, quickstart: tc.quick}
			r := httptest.NewRequest("GET", tc.url, nil)
			r.Header.Set("X-Forwarded-Proto", "https")
			w := httptest.NewRecorder()
			g.cookie(w, r, "session", "test", 60, true)
			c := w.Result().Cookies()[0]
			if c.Secure != tc.secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || strings.HasPrefix(c.Name, "__Host-") != tc.secure {
				t.Fatalf("wrong transport boundary: %+v", c)
			}
		})
	}
}

func TestQuickstartNodePortLoginAndCSRF(t *testing.T) {
	f := newPlatformServer(t)
	a := auth.New(f.store.Users(), f.store.Roles())
	e, err := auth.NewEnterpriseLogin(context.Background(), a, f.store.Settings(), nil, "", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewBrowserGateway(a, e, service.NewAuditService(f.store), "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler(NewWithPlatform(&fakeScan{}, a, nil, service.NewAuditService(f.store)).Handler())
	const origin = "http://192.0.2.1:31080"
	var cookies []*http.Cookie
	request := func(method, path, body, from, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, origin+"/api/v1/auth/"+path, strings.NewReader(body))
		r.Header.Set("Origin", from)
		r.Header.Set("X-CSRF-Token", csrf)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "login", `{"username":"admin","password":"admin-pw"}`, origin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	cookies = w.Result().Cookies()
	var csrf string
	for _, c := range cookies {
		if c.Secure {
			t.Fatal("HTTP cookie cannot be replayed by a browser")
		}
		if strings.HasSuffix(c.Name, "csrf") {
			csrf = c.Value
		}
	}
	if csrf == "" || request("GET", "me", "", "", "").Code != http.StatusOK {
		t.Fatal("cookie authentication failed")
	}
	if request("POST", "logout", "", "http://evil.test", csrf).Code != http.StatusForbidden || request("POST", "logout", "", origin, "").Code != http.StatusForbidden {
		t.Fatal("CSRF boundary bypassed")
	}
	if request("POST", "logout", "", origin, csrf).Code != http.StatusOK {
		t.Fatal("valid CSRF rejected")
	}
}
