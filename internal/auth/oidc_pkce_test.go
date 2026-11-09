package auth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"golang.org/x/oauth2"
)

func (f *fakeOIDC) tokenRequests() []tokenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tokenRequest(nil), f.tokenReqs...)
}

func (f *fakeOIDC) resetTokenRequests() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenReqs = nil
}

// TestEnterpriseLoginPKCE 锁住 PKCE 兼容开关的两端，以及令牌请求的凭据口径：
// 默认必须发 code_challenge / code_verifier；platform.auth.oidc_disable_pkce 打开后两端
// 参数都要彻底消失（只发一半会被严格 IdP 拒掉）；有 client_secret 时凭据固定走 Basic，
// 且一次交换只发一个令牌请求，不允许 AutoDetect 的重复探测。
func TestEnterpriseLoginPKCE(t *testing.T) {
	ctx := context.Background()
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	if err := svc.Bootstrap(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	idp := newFakeOIDC(t)
	cfg := ProviderConfig{Enabled: true, Issuer: idp.issuer(), ClientID: "client", Secret: "super-secret-value"}

	run := func(t *testing.T, disablePKCE bool) (authorize, token url.Values) {
		t.Helper()
		login, err := NewEnterpriseLogin(idp.ctx(), svc, st.Settings(), cipher, "https://platform.test", nil, disablePKCE, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := login.Configure(idp.ctx(), cfg, false); err != nil {
			t.Fatal(err)
		}
		idp.resetTokenRequests()
		cookie, redirect, err := login.Begin(idp.ctx(), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(redirect)
		if err != nil {
			t.Fatal(err)
		}
		authorize = parsed.Query()
		idp.setAuthRequest(authorize.Get("nonce"), authorize.Get("code_challenge"))
		if _, err := login.Finish(idp.ctx(), cookie, authorize.Get("state"), "code", ""); err != nil {
			t.Fatalf("login should succeed: %v", err)
		}
		reqs := idp.tokenRequests()
		if len(reqs) != 1 {
			t.Fatalf("一次交换应当只发一个令牌请求，实际发了 %d 个：AutoDetect 重复探测会盖掉 IdP 的报错", len(reqs))
		}
		if !strings.HasPrefix(reqs[0].auth, "Basic ") {
			t.Errorf("客户端凭据应固定走 client_secret_basic，实际 Authorization=%q", reqs[0].auth)
		}
		if reqs[0].form.Get("client_secret") != "" {
			t.Error("client_secret 不应出现在请求体里")
		}
		return authorize, reqs[0].form
	}

	t.Run("默认发 PKCE", func(t *testing.T) {
		authQuery, tokenForm := run(t, false)
		if authQuery.Get("code_challenge") == "" || authQuery.Get("code_challenge_method") != "S256" {
			t.Errorf("授权请求缺少 S256 PKCE： %v", authQuery)
		}
		if tokenForm.Get("code_verifier") == "" {
			t.Error("令牌请求缺少 code_verifier")
		}
	})

	t.Run("开关关闭后两端都不带 PKCE", func(t *testing.T) {
		authQuery, tokenForm := run(t, true)
		for _, key := range []string{"code_challenge", "code_challenge_method"} {
			if authQuery.Has(key) {
				t.Errorf("关掉 PKCE 后授权请求仍带 %s", key)
			}
		}
		if tokenForm.Has("code_verifier") {
			t.Error("关掉 PKCE 后令牌请求仍带 code_verifier")
		}
	})
}

// TestExchangeDetailKeepsProviderWording 保证令牌交换失败时 IdP 原话进得了日志：
// 现场只看得到 "enterprise login failed"，如果连 IdP 的 error/error_description 都不转述，
// 就没人能分清是 secret 错、code 过期还是 IdP 自己的 PKCE 实现坏了。
func TestExchangeDetailKeepsProviderWording(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusBadRequest}
	err := &oauth2.RetrieveError{
		Response:         resp,
		Body:             []byte(`{"error":"invalid_request","error_description":"code challenge verification failed"}`),
		ErrorCode:        "invalid_request",
		ErrorDescription: "code challenge verification failed",
	}
	detail := exchangeDetail(err, "super-secret-value", "one-time-code", "one-time-verifier")
	for _, want := range []string{"HTTP 400", "invalid_request", "code challenge verification failed"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q 缺少 %q", detail, want)
		}
	}
	for _, leak := range []string{"super-secret-value", "one-time-code", "one-time-verifier"} {
		if strings.Contains(detail, leak) {
			t.Errorf("detail %q 泄露了 %q", detail, leak)
		}
	}
	if got := exchangeDetail(errors.New("dial tcp: no route to host"), "secret"); !strings.Contains(got, "no route to host") {
		t.Errorf("非 IdP 错误应原样压平，实际 %q", got)
	}
}
