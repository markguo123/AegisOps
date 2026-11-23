package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"golang.org/x/oauth2"
)

// fakeOIDC 是最小可用企业 IdP：只实现 discovery、jwks 和 token 三个端点，token 端点的
// 行为由每个用例逐次设定，用来验证「失败时能不能说清原因」。
type fakeOIDC struct {
	t           *testing.T
	server      *httptest.Server
	mu          sync.Mutex
	signer      jose.Signer
	key         *rsa.PrivateKey
	nonce       string
	challenge   string
	groupMode   string // team | other | missing
	tokenStatus int
	tokenBody   string
	noIDToken   bool
	// dropNonce 模拟「IdP 收到 nonce 却不回传」：签发的 id_token 里根本没有 nonce 声明，
	// 而不是回一个空串——现场那套 Dex 分支就是整个声明缺失。
	dropNonce bool
	// iatSkew 让 id_token 的签发时间偏移，用来验证「缺 nonce 时靠 iat 挡住旧令牌重放」。
	iatSkew   time.Duration
	// tokenReqs 记录每一次令牌请求的表单与客户端凭据头，用来断言 PKCE 参数有没有发、
	// 凭据走的是 Basic 还是 body，以及有没有出现 AutoDetect 的重复探测请求。
	tokenReqs []tokenRequest
}

type tokenRequest struct {
	form url.Values
	auth string
}

func newFakeOIDC(t *testing.T) *fakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOIDC{t: t, key: key, signer: signer, groupMode: "team"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.issuer(), "authorization_endpoint": f.issuer() + "/auth", "token_endpoint": f.issuer() + "/token", "jwks_uri": f.issuer() + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", f.handleToken)
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeOIDC) issuer() string { return f.server.URL }

// ctx 走测试服务器的 TLS 客户端：企业根 CA 那条路径另有 oidc_ca_test.go 覆盖。
func (f *fakeOIDC) ctx() context.Context {
	return context.WithValue(context.Background(), oauth2.HTTPClient, f.server.Client())
}

func (f *fakeOIDC) setAuthRequest(nonce, challenge string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nonce, f.challenge = nonce, challenge
}

func (f *fakeOIDC) handleToken(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	if err := r.ParseForm(); err != nil {
		http.Error(w, "unreadable form", http.StatusBadRequest)
		return
	}
	form := r.PostForm
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenReqs = append(f.tokenReqs, tokenRequest{form: form, auth: auth})
	if f.tokenStatus != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.tokenStatus)
		w.Write([]byte(f.tokenBody))
		return
	}
	// 与合规 IdP 一致：客户端没带 PKCE（challenge 为空）就不做 PKCE 校验；带了就必须对得上。
	if r.FormValue("code") != "code" || (f.challenge != "" && oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != f.challenge) {
		http.Error(w, "invalid_grant", http.StatusBadRequest)
		return
	}
	if f.noIDToken {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "temporary", "token_type": "Bearer"})
		return
	}
	now := time.Now().Add(f.iatSkew)
	values := map[string]any{"iss": f.issuer(), "aud": "client", "sub": "stable", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}
	if !f.dropNonce {
		values["nonce"] = f.nonce
	}
	switch f.groupMode {
	case "team":
		values["groups"] = []string{"team"}
	case "other":
		values["groups"] = []string{"helm-app", "default-reader"}
	}
	claims, _ := json.Marshal(values)
	signed, err := f.signer.Sign(claims)
	if err != nil {
		http.Error(w, "signing failed", http.StatusInternalServerError)
		return
	}
	raw, err := signed.CompactSerialize()
	if err != nil {
		http.Error(w, "serializing failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"id_token": raw, "access_token": "temporary", "token_type": "Bearer"})
}

// loginTweak 伪造回调异常：改登录事务、改掉 IdP 带回的 state，或干脆不带 code。
type loginTweak struct {
	tx    func(login *EnterpriseLogin, cookie string)
	state *string
	code  *string
}

// TestEnterpriseLoginFailureReasons 锁住的是可运维性：每种失败都要落到一个可判别的哨兵上，
// 并且提示里带上现场真正需要的线索，同时绝不把 client_secret 写进任何回显。
func TestEnterpriseLoginFailureReasons(t *testing.T) {
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
	cfg := ProviderConfig{Enabled: true, Issuer: idp.issuer(), ClientID: "client", Secret: "super-secret-value", AllowedGroups: []string{"team"}}

	run := func(t *testing.T, tw loginTweak) error {
		t.Helper()
		login, err := NewEnterpriseLogin(idp.ctx(), svc, st.Settings(), cipher, "https://platform.test", nil, false, false)
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
		u, err := url.Parse(redirect)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		idp.setAuthRequest(q.Get("nonce"), q.Get("code_challenge"))
		state, code := q.Get("state"), "code"
		if tw.state != nil {
			state = *tw.state
		}
		if tw.code != nil {
			code = *tw.code
		}
		if tw.tx != nil {
			tw.tx(login, cookie)
		}
		_, err = login.Finish(idp.ctx(), cookie, state, code, "")
		if err == nil {
			t.Fatal("expected a failure")
		}
		return err
	}

	expired := func(login *EnterpriseLogin, cookie string) {
		login.mu.Lock()
		defer login.mu.Unlock()
		tx := login.transactions[cookie]
		tx.expires = time.Now().Add(-time.Second)
		login.transactions[cookie] = tx
	}
	empty := ""

	cases := []struct {
		name        string
		prepare     func()
		want        error
		contains    []string
		notContains []string
		tweak       loginTweak
	}{
		{
			name: "expired transaction", want: ErrLoginTxInvalid, contains: []string{"5m0s"},
			tweak: loginTweak{tx: expired},
		},
		{
			name: "state mismatch", want: ErrLoginStateMismatch, tweak: loginTweak{state: &empty},
		},
		{
			name: "provider sends no code", want: ErrLoginStateMismatch, tweak: loginTweak{code: &empty},
		},
		{
			name: "unknown browser callback", want: ErrLoginTxInvalid,
			tweak: loginTweak{tx: func(login *EnterpriseLogin, cookie string) {
				login.mu.Lock()
				defer login.mu.Unlock()
				delete(login.transactions, cookie)
			}},
		},
		{
			name: "idp rejects the exchange", want: ErrLoginExchange,
			contains:    []string{"invalid_client", "invalid client credentials"},
			notContains: []string{"super-secret-value"},
			prepare: func() {
				idp.mu.Lock()
				defer idp.mu.Unlock()
				idp.tokenStatus, idp.tokenBody = http.StatusUnauthorized, `{"error":"invalid_client","error_description":"invalid client credentials"}`
			},
		},
		{
			name: "missing id token", want: ErrLoginIDToken, contains: []string{"no id_token"},
			prepare: func() {
				idp.mu.Lock()
				defer idp.mu.Unlock()
				idp.noIDToken, idp.tokenStatus = true, 0
			},
		},
		{
			name: "group claim absent", want: ErrLoginGroupDenied, contains: []string{`"groups"`},
			prepare: func() {
				idp.mu.Lock()
				defer idp.mu.Unlock()
				idp.groupMode, idp.tokenStatus = "missing", 0
			},
		},
		{
			name: "group not allowed", want: ErrLoginGroupDenied, contains: []string{"helm-app", "[team]"},
			prepare: func() {
				idp.mu.Lock()
				defer idp.mu.Unlock()
				idp.groupMode, idp.tokenStatus = "other", 0
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp.mu.Lock()
			idp.groupMode, idp.tokenStatus, idp.tokenBody, idp.noIDToken = "team", 0, "", false
			idp.mu.Unlock()
			if tc.prepare != nil {
				tc.prepare()
			}
			err := run(t, tc.tweak)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			msg := err.Error()
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q missing %q", msg, want)
				}
			}
			for _, bad := range tc.notContains {
				if strings.Contains(msg, bad) {
					t.Errorf("message %q leaks %q", msg, bad)
				}
			}
		})
	}
}
