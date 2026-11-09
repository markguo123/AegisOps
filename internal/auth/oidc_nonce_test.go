package auth

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/cluster"
)

// TestEnterpriseLoginMissingNonce 锁住 platform.auth.oidc_allow_missing_nonce 的放宽边界。
// 现场诱因是企业 IdP（容器云那套 Dex 分支）在授权请求里收到 nonce 却不持久化，签发的
// id_token 因此永远没有 nonce 声明，严格校验必然把每次单点登录都拒在 id-token-rejected。
// 这里要同时钉住两件事：默认必须照旧拒绝（放宽不是常态），而开关打开后放宽面也只有
// 「完全没有 nonce 声明」这一种——IdP 回了 nonce 却对不上、或者 id_token 早于本次登录
// 事务签发，都仍然拒绝，否则一次性随机数防重放就彻底失效了。
func TestEnterpriseLoginMissingNonce(t *testing.T) {
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

	type loginCase struct {
		allowMissingNonce bool
		dropNonce         bool
		wrongNonce        bool
		iatSkew           time.Duration
	}

	run := func(t *testing.T, c loginCase) error {
		t.Helper()
		idp.mu.Lock()
		idp.dropNonce, idp.iatSkew = c.dropNonce, c.iatSkew
		idp.mu.Unlock()
		login, err := NewEnterpriseLogin(idp.ctx(), svc, st.Settings(), cipher, "https://platform.test", nil, false, c.allowMissingNonce)
		if err != nil {
			t.Fatal(err)
		}
		if err := login.Configure(idp.ctx(), cfg, false); err != nil {
			t.Fatal(err)
		}
		cookie, redirect, err := login.Begin(idp.ctx(), nil, "")
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(redirect)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		// 授权请求里始终带 nonce：放宽只发生在回传校验这一侧，我方不发是另一种缺陷。
		if query.Get("nonce") == "" {
			t.Fatal("授权请求必须带 nonce")
		}
		nonce := query.Get("nonce")
		if c.wrongNonce {
			nonce = nonce + "-forged"
		}
		idp.setAuthRequest(nonce, query.Get("code_challenge"))
		_, err = login.Finish(idp.ctx(), cookie, query.Get("state"), "code", "")
		return err
	}

	t.Run("默认严格：缺 nonce 声明照旧拒绝", func(t *testing.T) {
		err := run(t, loginCase{dropNonce: true})
		if !errors.Is(err, ErrLoginIDToken) {
			t.Fatalf("期望 ErrLoginIDToken，实际 %v", err)
		}
		if !strings.Contains(err.Error(), "nonce") {
			t.Errorf("提示要点明是 nonce，实际 %q", err)
		}
	})

	t.Run("开关打开后放行无 nonce 的新鲜令牌", func(t *testing.T) {
		if err := run(t, loginCase{allowMissingNonce: true, dropNonce: true}); err != nil {
			t.Fatalf("开关打开后应当登录成功，实际 %v", err)
		}
	})

	t.Run("开关打开也不放过对不上的 nonce", func(t *testing.T) {
		err := run(t, loginCase{allowMissingNonce: true, wrongNonce: true})
		if !errors.Is(err, ErrLoginIDToken) {
			t.Fatalf("nonce 不匹配必须拒绝，实际 %v", err)
		}
	})

	t.Run("开关打开也要挡住事务开始前的旧令牌", func(t *testing.T) {
		err := run(t, loginCase{allowMissingNonce: true, dropNonce: true, iatSkew: -10 * time.Minute})
		if !errors.Is(err, ErrLoginIDToken) {
			t.Fatalf("缺 nonce 时必须靠 iat 挡住重放，实际 %v", err)
		}
	})

	t.Run("正常 IdP 不受开关影响", func(t *testing.T) {
		if err := run(t, loginCase{allowMissingNonce: true}); err != nil {
			t.Fatalf("合规 IdP 登录应成功，实际 %v", err)
		}
	})
}
