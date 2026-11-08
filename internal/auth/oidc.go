package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"

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
}
type ProviderCipher interface {
	Encrypt([]byte) (string, error)
	Decrypt(string) ([]byte, error)
}
type loginTransaction struct {
	state, nonce, verifier string
	expires                time.Time
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
}

func NewEnterpriseLogin(ctx context.Context, s *Service, settings store.SettingsStore, cipher ProviderCipher, publicURL string) (*EnterpriseLogin, error) {
	e := &EnterpriseLogin{auth: s, settings: settings, cipher: cipher, transactions: map[string]loginTransaction{}}
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
		provider, err = oidc.NewProvider(ctx, c.Issuer)
		if err != nil {
			return errors.New("OIDC discovery failed")
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
func (e *EnterpriseLogin) oauth(p *oidc.Provider, c ProviderConfig) oauth2.Config {
	var scopes []string
	if len(c.AllowedGroups) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}
	scopes = GetScopesOrDefault(scopes)
	return oauth2.Config{ClientID: c.ClientID, ClientSecret: c.Secret, Endpoint: p.Endpoint(), RedirectURL: e.callback, Scopes: scopes}
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
		p, err := oidc.NewProvider(ctx, e.cfg.Issuer)
		if err != nil {
			return "", "", errors.New("enterprise login unavailable")
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
	tx := loginTransaction{state: state, nonce: nonce, verifier: oauth2.GenerateVerifier(), expires: now.Add(5 * time.Minute), revision: e.revision}
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
	opts := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(tx.verifier)}
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
	if !ok || state != tx.state || code == "" || !time.Now().Before(tx.expires) || p == nil {
		return nil, ErrInvalidCredentials
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Provider verification uses the discovery issuer and configured client audience.
	oauth := e.oauth(p, cfg)
	token, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(tx.verifier))
	if err != nil {
		return nil, ErrInvalidCredentials
	}
	raw, _ := token.Extra("id_token").(string)
	id, err := p.Verifier(&oidc.Config{ClientID: cfg.ClientID}).Verify(ctx, raw)
	if err != nil || id.Subject == "" || id.Nonce != tx.nonce || id.IssuedAt.IsZero() || id.IssuedAt.After(time.Now().Add(time.Minute)) {
		return nil, ErrInvalidCredentials
	}
	var claims map[string]json.RawMessage
	if id.Claims(&claims) != nil {
		return nil, ErrInvalidCredentials
	}
	claim := func(k string) string { var v string; _ = json.Unmarshal(claims[k], &v); return v }
	if azp := claim("azp"); (len(id.Audience) > 1 || azp != "") && azp != cfg.ClientID {
		return nil, ErrInvalidCredentials
	}
	if len(cfg.AllowedGroups) > 0 {
		var groups []string
		k := cfg.GroupsClaim
		if k == "" {
			k = "groups"
		}
		if json.Unmarshal(claims[k], &groups) != nil || !slices.ContainsFunc(groups, func(g string) bool { return slices.Contains(cfg.AllowedGroups, g) }) {
			return nil, ErrInvalidCredentials
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Enabled || tx.revision != e.revision {
		return nil, ErrInvalidCredentials
	}
	if tx.bindID != "" {
		u, _, checkErr := e.auth.RecheckToken(ctx, sessionToken)
		if checkErr != nil || tx.bindSession != tokenHash(sessionToken) || u.ID != tx.bindID || u.SessionVersion != tx.bindVersion {
			return nil, ErrInvalidCredentials
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
	u, err := e.auth.users.ResolveIdentity(ctx, id.Issuer, id.Subject, &store.User{ID: uid, Username: "oidc-" + uid, DisplayName: claim("name"), Email: claim("email"), RoleID: roleID})
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
