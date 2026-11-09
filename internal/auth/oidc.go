package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"golang.org/x/oauth2"
)

type ProviderConfig struct {
	Enabled       bool     `json:"enabled"`
	DisplayName   string   `json:"displayName"`
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"clientId"`
	Secret        string   `json:"clientSecret,omitempty"`
	GroupsClaim   string   `json:"groupsClaim,omitempty"`
	AllowedGroups []string `json:"allowedGroups,omitempty"`
	// UsernameClaim 覆盖 id_token 里当作登录名的 claim，对应 Dex 的
	// claimMappings.preferred_username；留空即用 Dex 默认 preferred_username。
	UsernameClaim string `json:"usernameClaim,omitempty"`
	// UsernameClaimOverride 对应 Dex 的 overrideClaimMapping：true 时无条件使用
	// UsernameClaim，哪怕 id_token 里就有 preferred_username。默认 false，只在
	// preferred_username 缺失时才退到 UsernameClaim。
	UsernameClaimOverride bool `json:"usernameClaimOverride,omitempty"`
}

// 企业登录失败原因分类。浏览器侧统一是 401，但服务端日志和审计必须指出到底卡在哪一步：
// 登录事务失效、IdP 拒绝令牌交换，还是令牌/准入校验不通过。否则现场只能看到
// "enterprise login failed" 一个字都定位不了。消息本身不含 client_secret、授权码和令牌内容。
var (
	ErrLoginTxInvalid     = errors.New("auth: enterprise login transaction is expired, already used, or lost to a restart")
	ErrLoginStateMismatch = errors.New("auth: enterprise login state mismatch")
	ErrLoginExchange      = errors.New("auth: enterprise identity provider rejected the token exchange")
	ErrLoginIDToken       = errors.New("auth: enterprise identity token failed validation")
	ErrLoginGroupDenied   = errors.New("auth: enterprise account is not allowed by the group admission rule")
	ErrLoginConfigChanged = errors.New("auth: enterprise login configuration changed during the login")
	ErrLoginNotReady      = errors.New("auth: enterprise login is not configured")
)

// loginTransactionTTL 冻结为 5 分钟（方案 20 §5.2，与 Keycloak、Argo CD 同量级）。
// 现场最常见的坑是用户在企业 IdP 登录页停留超时，回到回调必然失败。
const loginTransactionTTL = 5 * time.Minute

// loginNonceClockSkew 是「IdP 不回传 nonce」这条兼容路径上，容忍平台与 IdP 时钟差的宽度。
// 只有签发时刻落在本次登录事务开始之前这么一点余量之内才接受，超了就算旧令牌。
const loginNonceClockSkew = time.Minute

// safeDetail 把第三方错误压成可入库、可入日志的单行短文本：折叠空白防日志注入，截断
// 防刷屏，并把 client_secret 与一次性授权码替换掉——这两样永远不该落到日志或审计里。
func safeDetail(err error, secrets ...string) string {
	msg := err.Error()
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
		}
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "..."
	}
	return msg
}

// exchangeDetail 说清楚令牌交换到底被谁拒了。企业登录排障最大的坑是浏览器只有
// "enterprise login failed" 一句，IdP 明明回了明确的原因却看不见，所以这里优先转述
// IdP 原话（HTTP 状态码 + OAuth error + error_description），拿不到结构化字段时才退回
// 整条错误串。两路都过 safeDetail：client_secret、一次性授权码和 code_verifier 一律不落
// 日志与审计，第三方响应也折叠截断，防止拿响应体往日志里塞内容。
func exchangeDetail(err error, secrets ...string) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) && re.Response != nil {
		msg := fmt.Sprintf("identity provider token endpoint returned HTTP %d", re.Response.StatusCode)
		switch {
		case re.ErrorCode != "" && re.ErrorDescription != "":
			msg += fmt.Sprintf(": %s (%s)", re.ErrorCode, re.ErrorDescription)
		case re.ErrorCode != "":
			msg += ": " + re.ErrorCode
		case re.ErrorDescription != "":
			msg += ": " + re.ErrorDescription
		case len(re.Body) > 0:
			msg += ": " + string(re.Body)
		}
		return safeDetail(errors.New(msg), secrets...)
	}
	return safeDetail(err, secrets...)
}

type ProviderCipher interface {
	Encrypt([]byte) (string, error)
	Decrypt(string) ([]byte, error)
}
type loginTransaction struct {
	state, nonce, verifier string
	expires                time.Time
	// started 是事务创建时刻，用于在 IdP 不回填 nonce 时靠 iat 判断令牌是不是这次登录刚签发的。
	started                time.Time
	revision               uint64
	bindID                 string
	bindVersion            int64
	bindSession            string
}
type EnterpriseLogin struct {
	auth         *Service
	settings     store.SettingsStore
	cipher       ProviderCipher
	callback     string
	mu           sync.Mutex
	cfg          ProviderConfig
	revision     uint64
	transactions map[string]loginTransaction
	provider     *oidc.Provider
	// httpClient 只在配置了企业 IdP 根 CA 时非空；为空走 Go 默认传输（系统信任链）。
	httpClient *http.Client
	// disablePKCE 是对 IdP 侧 PKCE 实现有缺陷时的兼容开关（platform.auth.oidc_disable_pkce）。
	// 打开后授权请求不再带 code_challenge，令牌交换也不再带 code_verifier。默认关闭：
	// PKCE 是授权码被截获时的主要防线，只有现场 IdP 自己把 PKCE 处理坏、导致单点登录
	// 无法完成时才关，且必须配合固定 HTTPS 回调与一次性授权码使用。
	disablePKCE bool
	// allowMissingNonce 是「IdP 根本不回传 nonce」时的兼容开关（platform.auth.oidc_allow_missing_nonce）。
	// 默认关闭，且它的放宽范围刻意窄到只有一种：id_token 完全没有 nonce 声明。只要 IdP 回了
	// nonce 却与本次登录对不上，无论开关如何都拒绝。放行时还要求 iat 落在本次登录事务开始
	// 之后（留 loginNonceClockSkew 时钟差余量），使旧令牌 replay 不成立。现场触发条件是 IdP
	// 在授权请求里收到 nonce 却不持久化（例：容器云那套 Dex 分支的 AuthRequest 里没有 nonce
	// 字段，其自定义登录页还会把 nonce 从 back 参数里丢掉），于是任何严格实现 OIDC 的客户端
	// 都无法通过 nonce 校验，而其自家控制台因为不校验才没暴露。
	allowMissingNonce bool
}

// oidcHTTPClient 在系统信任链之上追加企业 IdP 的根 CA：只增不减，因此内部自建 CA
// 签发的 IdP 可校验，公网 IdP 仍走系统根。企业 IdP 用集群自签 CA 时，镜像里的系统
// 信任库不认，OIDC discovery 会直接失败，这是内部环境接企业登录最常见的坎。
// 刻意不提供 InsecureSkipVerify 分支：跳过校验等于接受任意来源签发的身份令牌。
func oidcHTTPClient(caPEM []byte) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		// 没有系统信任库的镜像变体（distroless）退化为「只信现场显式提供的根」。
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("oidc CA file contains no valid PEM certificate")
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        16,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}}, nil
}

// NewEnterpriseLogin 装配企业 OIDC 登录。caPEM 是追加信任的根 CA（PEM，可为空），
// 非法 PEM 在这里就报错：宁起不来，也不要「配了 CA 却静默没生效」。
// disablePKCE 是对 PKCE 实现有缺陷的企业 IdP 的兼容开关，默认必须为 false。
// allowMissingNonce 同样默认必须为 false，只容忍 id_token 不带 nonce 声明这一种情形。
func NewEnterpriseLogin(ctx context.Context, s *Service, settings store.SettingsStore, cipher ProviderCipher, publicURL string, caPEM []byte, disablePKCE, allowMissingNonce bool) (*EnterpriseLogin, error) {
	e := &EnterpriseLogin{auth: s, settings: settings, cipher: cipher, transactions: map[string]loginTransaction{}, disablePKCE: disablePKCE, allowMissingNonce: allowMissingNonce}
	if len(bytes.TrimSpace(caPEM)) > 0 {
		client, err := oidcHTTPClient(caPEM)
		if err != nil {
			return nil, err
		}
		e.httpClient = client
	}
	if publicURL != "" {
		u, err := url.Parse(publicURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
			return nil, errors.New("public_url must be a fixed HTTPS origin")
		}
		e.callback = strings.TrimSuffix(publicURL, "/") + "/api/v1/auth/oidc/callback"
	}
	v, err := settings.Get(ctx, "auth", "provider")
	if errors.Is(err, store.ErrNotFound) {
		return e, nil
	}
	if err != nil {
		return nil, err
	}
	if cipher == nil {
		return e, nil // local emergency login remains available
	}
	plain, err := cipher.Decrypt(v.Value)
	if err != nil {
		return e, nil
	}
	if err = json.Unmarshal(plain, &e.cfg); err != nil {
		e.cfg = ProviderConfig{}
		return e, nil
	}
	return e, nil
}
func (e *EnterpriseLogin) Config() (ProviderConfig, bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c := e.cfg
	has := c.Secret != ""
	c.Secret = ""
	return c, has, e.callback
}
func (e *EnterpriseLogin) Configure(ctx context.Context, c ProviderConfig, testOnly bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c.Secret == "" {
		c.Secret = e.cfg.Secret
	}
	var provider *oidc.Provider
	if c.Enabled || testOnly {
		u, err := url.Parse(c.Issuer)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || c.ClientID == "" || c.Secret == "" || e.callback == "" {
			return errors.New("HTTPS issuer, public_url, client ID and client secret are required")
		}
		ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		provider, err = oidc.NewProvider(e.discoveryContext(ctx), c.Issuer)
		if err != nil {
			// 原文一并回显：这里只有 issuer 地址与传输层错误（x509 不受信、DNS、超时），
			// 不含凭据，而 "certificate signed by unknown authority" 正是现场最需要的线索。
			return fmt.Errorf("OIDC discovery failed: %w", err)
		}
	}
	if testOnly {
		return nil
	}
	if e.cipher == nil {
		return errors.New("OIDC settings require platform encryption")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	encrypted, err := e.cipher.Encrypt(raw)
	if err != nil {
		return err
	}
	if err = e.settings.Set(ctx, &store.Settings{Group: "auth", Key: "provider", Value: encrypted, Sensitive: true}); err != nil {
		return err
	}
	if !c.Enabled || c.Issuer != e.cfg.Issuer || c.ClientID != e.cfg.ClientID || c.GroupsClaim != e.cfg.GroupsClaim || !slices.Equal(c.AllowedGroups, e.cfg.AllowedGroups) {
		e.auth.mu.Lock()
		for key, entry := range e.auth.tokens {
			if entry.source == "oidc" {
				delete(e.auth.tokens, key)
			}
		}
		e.auth.mu.Unlock()
	}
	e.cfg = c
	e.provider = provider
	e.revision++
	clear(e.transactions)
	return nil
}
func randomChallenge() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// discoveryContext 给 OIDC 发现流程挂上带企业根 CA 的客户端。未配置 CA 时原样返回，
// 保留调用方通过 ctx 注入自定义客户端的能力（测试即用这条路径）。
func (e *EnterpriseLogin) discoveryContext(ctx context.Context) context.Context {
	if e.httpClient == nil {
		return ctx
	}
	return oidc.ClientContext(ctx, e.httpClient)
}
func (e *EnterpriseLogin) oauth(p *oidc.Provider, c ProviderConfig) oauth2.Config {
	var scopes []string
	if len(c.AllowedGroups) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	scopes = GetScopesOrDefault(scopes)
	endpoint := p.Endpoint()
	endpoint.AuthStyle = tokenAuthStyle(c.Secret)
	return oauth2.Config{ClientID: c.ClientID, ClientSecret: c.Secret, Endpoint: endpoint, RedirectURL: e.callback, Scopes: scopes}
}

// tokenAuthStyle 明确令牌交换怎么带客户端凭据。Endpoint.AuthStyle 留空（AutoDetect）时
// golang.org/x/oauth2 会先按 Basic 发一次，失败后（不限 401，任何错误都算）换成进 body
// 再发一次，见 internal/token.go 里的 needsAuthStyleProbe：现场因此看到两次 POST，而 IdP
// 的原话被第二次尝试盖掉，排查时只剩一个状态码。有 secret 就固定用 RFC 首选的
// client_secret_basic；公开客户端没有 secret，只能进 body。
func tokenAuthStyle(secret string) oauth2.AuthStyle {
	if secret == "" {
		return oauth2.AuthStyleInParams
	}
	return oauth2.AuthStyleInHeader
}
func (e *EnterpriseLogin) Begin(ctx context.Context, bind *store.User, sessionToken string) (string, string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Enabled || e.callback == "" {
		return "", "", errors.New("enterprise login disabled")
	}
	if e.provider == nil {
		ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		p, err := oidc.NewProvider(e.discoveryContext(ctx), e.cfg.Issuer)
		if err != nil {
			return "", "", fmt.Errorf("enterprise login unavailable: %w", err)
		}
		e.provider = p
	}
	now := time.Now()
	for k, v := range e.transactions {
		if !now.Before(v.expires) {
			delete(e.transactions, k)
		}
	}
	if len(e.transactions) >= 4096 {
		return "", "", errors.New("too many pending logins")
	}
	key, err := randomChallenge()
	if err != nil {
		return "", "", err
	}
	state, err := randomChallenge()
	if err != nil {
		return "", "", err
	}
	nonce, err := randomChallenge()
	if err != nil {
		return "", "", err
	}
	tx := loginTransaction{
		state:    state,
		nonce:    nonce,
		started:  now,
		expires:  now.Add(loginTransactionTTL),
		revision: e.revision,
	}
	// 关掉 PKCE 时整条链路都不出现 PKCE 参数：只发 challenge 不发 verifier，或反过来，
	// 都会让实现严格的 IdP 直接拒掉交换。
	if !e.disablePKCE {
		tx.verifier = oauth2.GenerateVerifier()
	}
	if bind != nil {
		if sessionToken == "" {
			return "", "", ErrInvalidCredentials
		}
		tx.bindSession = tokenHash(sessionToken)
		tx.bindID = bind.ID
		tx.bindVersion = bind.SessionVersion
	}
	e.transactions[key] = tx
	oauth := e.oauth(e.provider, e.cfg)
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce)}
	if tx.verifier != "" {
		opts = append(opts, oauth2.S256ChallengeOption(tx.verifier))
	}
	if len(e.cfg.AllowedGroups) > 0 {
		name := e.cfg.GroupsClaim
		if name == "" {
			name = "groups"
		}
		opt, err := createClaimsAuthenticationRequestParameter(map[string]essentialClaim{name: {Essential: true}})
		if err != nil {
			delete(e.transactions, key)
			return "", "", err
		}
		opts = append(opts, opt)
	}
	return key, oauth.AuthCodeURL(state, opts...), nil
}
func (e *EnterpriseLogin) Finish(ctx context.Context, key, state, code, sessionToken string) (*Session, error) {
	e.mu.Lock()
	tx, ok := e.transactions[key]
	delete(e.transactions, key)
	cfg, p := e.cfg, e.provider
	e.mu.Unlock()
	// 事务一次性：取出即删除，重复回调（刷新页面、二次点击）必然落空。
	if !ok {
		return nil, fmt.Errorf("%w: no pending login matches this browser callback cookie", ErrLoginTxInvalid)
	}
	if !time.Now().Before(tx.expires) {
		return nil, fmt.Errorf("%w: the browser spent more than %s on the identity provider page", ErrLoginTxInvalid, loginTransactionTTL)
	}
	if state != tx.state {
		return nil, fmt.Errorf("%w: state parameter does not match the one this browser started with", ErrLoginStateMismatch)
	}
	if code == "" {
		return nil, fmt.Errorf("%w: the identity provider sent back no authorization code", ErrLoginStateMismatch)
	}
	if p == nil {
		return nil, fmt.Errorf("%w: the provider was removed before the callback arrived", ErrLoginNotReady)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 令牌交换走同一个客户端：配置了企业根 CA 时，Exchange 也必须认那把 CA。
	ctx = e.discoveryContext(ctx)
	// Provider verification uses the discovery issuer and configured client audience.
	oauth := e.oauth(p, cfg)
	var exchangeOpts []oauth2.AuthCodeOption
	if tx.verifier != "" {
		exchangeOpts = append(exchangeOpts, oauth2.VerifierOption(tx.verifier))
	}
	token, err := oauth.Exchange(ctx, code, exchangeOpts...)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrLoginExchange, exchangeDetail(err, cfg.Secret, code, tx.verifier))
	}
	raw, _ := token.Extra("id_token").(string)
	if raw == "" {
		return nil, fmt.Errorf("%w: the token response carried no id_token", ErrLoginIDToken)
	}
	id, err := p.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrLoginIDToken, safeDetail(err, cfg.Secret))
	}
	switch {
	case id.Subject == "":
		return nil, fmt.Errorf("%w: the id token carries an empty subject", ErrLoginIDToken)
	case id.IssuedAt.IsZero():
		return nil, fmt.Errorf("%w: the id token carries no issued-at claim", ErrLoginIDToken)
	case id.IssuedAt.After(time.Now().Add(time.Minute)):
		return nil, fmt.Errorf("%w: the id token is issued in the future, check the clock difference between platform and provider", ErrLoginIDToken)
	case id.Nonce != tx.nonce:
		// 放宽范围只有一种：id_token 完全没有 nonce 声明，且开关显式打开。此时改用「iat 必须
		// 不早于本次登录事务开始」兜住 replay——事务本身只活 loginTransactionTTL，签名与受众
		// 已由上面的 Verify 校过，旧令牌在这个窗口外会被拒。只要 IdP 回了 nonce 却对不上，
		// 无论开关如何都拒绝。
		if id.Nonce != "" || !e.allowMissingNonce || tx.started.IsZero() ||
			id.IssuedAt.Before(tx.started.Add(-loginNonceClockSkew)) {
			return nil, fmt.Errorf("%w: the id token nonce does not match this login", ErrLoginIDToken)
		}
	}
	var claims map[string]json.RawMessage
	if err := id.Claims(&claims); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrLoginIDToken, safeDetail(err, cfg.Secret))
	}
	claim := func(k string) string { var v string; _ = json.Unmarshal(claims[k], &v); return v }
	if azp := claim("azp"); (len(id.Audience) > 1 || azp != "") && azp != cfg.ClientID {
		return nil, fmt.Errorf("%w: the id token azp %q and audience %v do not match the configured client id %q", ErrLoginIDToken, azp, id.Audience, cfg.ClientID)
	}
	if len(cfg.AllowedGroups) > 0 {
		var groups []string
		k := cfg.GroupsClaim
		if k == "" {
			k = "groups"
		}
		// 准入拒绝是「IdP 登录成功但平台不让进」的唯一来源，必须说清是缺 claim 还是集合不交。
		var rawClaim json.RawMessage
		if rawClaim, _ = claims[k]; len(rawClaim) == 0 {
			return nil, fmt.Errorf("%w: the id token carries no %q claim; the provider has to release group membership in the id token", ErrLoginGroupDenied, k)
		}
		if err := json.Unmarshal(rawClaim, &groups); err != nil {
			return nil, fmt.Errorf("%w: the id token claim %q is not a list of strings", ErrLoginGroupDenied, k)
		}
		if !slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(cfg.AllowedGroups, g) }) {
			return nil, fmt.Errorf("%w: the id token groups %v are outside the allowed set %v", ErrLoginGroupDenied, groups, cfg.AllowedGroups)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Enabled {
		return nil, fmt.Errorf("%w: the provider was turned off while this login was in flight", ErrLoginConfigChanged)
	}
	if tx.revision != e.revision {
		return nil, fmt.Errorf("%w: the provider configuration was saved while this login was in flight", ErrLoginConfigChanged)
	}
	if tx.bindID != "" {
		u, _, checkErr := e.auth.RecheckToken(ctx, sessionToken)
		if checkErr != nil || tx.bindSession != tokenHash(sessionToken) || u.ID != tx.bindID || u.SessionVersion != tx.bindVersion {
			return nil, fmt.Errorf("%w: the platform session that started the binding is no longer valid", ErrLoginStateMismatch)
		}
		if err := e.auth.users.BindIdentity(ctx, tx.bindID, tx.bindVersion, id.Issuer, id.Subject); err != nil {
			return nil, err
		}
		e.auth.revokeUser(tx.bindID)
	}
	roles, err := e.auth.roles.List(ctx)
	if err != nil {
		return nil, err
	}
	var roleID string
	for _, r := range roles {
		if r.Name == RoleUser {
			roleID = r.ID
			break
		}
	}
	if roleID == "" {
		return nil, errors.New("user role unavailable")
	}
	uid := newSecretToken()
	// 登录名按 Dex 的 claim 口径取（preferred_username → 覆盖键 → name → email），
	// 这样 Keycloak 一类 name 为空的 IdP 也能拿到人看得懂的名字。
	u, err := e.auth.users.ResolveIdentity(ctx, id.Issuer, id.Subject, &store.User{ID: uid, Username: "oidc-" + uid, DisplayName: preferredLoginName(claim, cfg.UsernameClaim, cfg.UsernameClaimOverride), Email: claim(defaultEmailClaim), RoleID: roleID})
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(SessionTTL)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	return e.auth.issue(u, "oidc", expires)
}
func (s *Service) HasExternalIdentity(ctx context.Context, id string) (bool, error) {
	return s.users.HasExternalIdentity(ctx, id)
}
func (s *Service) Unlink(ctx context.Context, u *store.User) error {
	if err := s.users.UnbindIdentity(ctx, u.ID, u.SessionVersion); err != nil {
		return err
	}
	s.revokeUser(u.ID)
	return nil
}
