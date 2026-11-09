package server

import (
	"context"
	"errors"
	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBrowserCookieOriginAndLogout(t *testing.T) {
	fixture := newPlatformServer(t)
	a := auth.New(fixture.store.Users(), fixture.store.Roles())
	enterprise, e := auth.NewEnterpriseLogin(context.Background(), a, fixture.store.Settings(), nil, "https://platform.test", nil, false, false)
	if e != nil {
		t.Fatal(e)
	}
	gateway, e := NewBrowserGateway(a, enterprise, service.NewAuditService(fixture.store), "https://platform.test", false)
	if e != nil {
		t.Fatal(e)
	}
	h := gateway.Handler(NewWithPlatform(&fakeScan{}, a, nil, service.NewAuditService(fixture.store)).Handler())
	var cookies []*http.Cookie
	var csrf string
	request := func(method, path, body, origin, header string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "https://platform.test/api/v1/auth/"+path, strings.NewReader(body))
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", header)
		for _, c := range cookies {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const origin = "https://platform.test"
	login := request("POST", "login", `{"username":"admin","password":"admin-pw"}`, origin, "")
	if login.Code != 200 {
		t.Fatal(login.Code)
	}
	cookies = login.Result().Cookies()
	for _, c := range cookies {
		if c.Name == "__Host-aegisops_session" && (!c.HttpOnly || !c.Secure || strings.Contains(login.Body.String(), c.Value)) {
			t.Fatal("session exposed")
		}
		if c.Name == "__Host-aegisops_csrf" {
			csrf = c.Value
		}
	}
	if csrf == "" || request("GET", "me", "", "", "").Code != 200 {
		t.Fatal("missing authenticated cookie")
	}
	if request("POST", "logout", "", "https://other.test", csrf).Code != 403 || request("POST", "logout", "", origin, "").Code != 403 {
		t.Fatal("CSRF accepted")
	}
	if request("POST", "logout", "", origin, csrf).Code != 200 || request("GET", "me", "", "", "").Code != 401 {
		t.Fatal("logged-out cookie still valid")
	}
}

// TestEnterpriseLoginHintCoversEveryReason 保证以后新增失败分类时不会漏配提示：漏了就退化成
// 「平台侧失败 + 500」，配置问题会被当成服务故障来查。
func TestEnterpriseLoginHintCoversEveryReason(t *testing.T) {
	for _, e := range []error{auth.ErrLoginTxInvalid, auth.ErrLoginStateMismatch, auth.ErrLoginExchange, auth.ErrLoginIDToken, auth.ErrLoginGroupDenied, auth.ErrLoginConfigChanged, auth.ErrLoginNotReady} {
		reason, hint := enterpriseLoginHint(e)
		if reason == "failed" || hint == "" {
			t.Fatalf("%v has no browser hint", e)
		}
	}
	if reason, _ := enterpriseLoginHint(errors.New("store unavailable")); reason != "failed" {
		t.Fatalf("a non-login error must count as a platform-side failure, got %q", reason)
	}
}
