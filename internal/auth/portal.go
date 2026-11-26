package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

const PortalSettingsKey = "portal"
const defaultPortalHome = "https://portal.gientech.com"

var ErrPortalUnavailable = errors.New("portal authentication unavailable")

type PortalConfig struct {
	Enabled       bool   `json:"enabled"`
	LocalDisabled bool   `json:"localDisabled"`
	HomeURL       string `json:"homeUrl"`
	CheckTokenURL string `json:"checkTokenUrl"`
}

func DefaultPortalConfig() PortalConfig {
	return PortalConfig{HomeURL: defaultPortalHome, CheckTokenURL: defaultPortalHome + "/api/authcenter/auth/checktoken"}
}

func portalURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("Portal URLs must use HTTPS without credentials, query parameters or fragments")
	}
	return u, nil
}

func (c *PortalConfig) Validate() error {
	c.HomeURL = strings.TrimRight(strings.TrimSpace(c.HomeURL), "/")
	c.CheckTokenURL = strings.TrimSpace(c.CheckTokenURL)
	home, err := portalURL(c.HomeURL)
	if err != nil {
		return err
	}
	if c.CheckTokenURL == "" {
		c.CheckTokenURL = "https://" + home.Host + "/api/authcenter/auth/checktoken"
	}
	check, err := portalURL(c.CheckTokenURL)
	if err != nil {
		return err
	}
	if !strings.EqualFold(home.Host, check.Host) {
		return errors.New("Portal homepage and token validator must use the same host")
	}
	return nil
}

var portalClient = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type portalIdentity struct {
	Username string `json:"username"`
	NickName string `json:"nickName"`
	Email    string `json:"email"`
}

func verifyPortalToken(ctx context.Context, client *http.Client, endpoint, token string) (*portalIdentity, error) {
	u, err := portalURL(endpoint)
	if err != nil {
		return nil, ErrPortalUnavailable
	}
	q := u.Query()
	q.Set("token", token)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, ErrPortalUnavailable
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrPortalUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, ErrPortalUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ErrInvalidCredentials
	}
	var result struct {
		Code string         `json:"code"`
		Data portalIdentity `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return nil, ErrPortalUnavailable
	}
	result.Data.Username = strings.TrimSpace(result.Data.Username)
	if result.Code != "000000" || result.Data.Username == "" || len(result.Data.Username) > 255 {
		return nil, ErrInvalidCredentials
	}
	return &result.Data, nil
}

// VerifyPortalLogin validates the external token before the caller commits a
// local session under the current login policy.
func (s *Service) VerifyPortalLogin(ctx context.Context, c PortalConfig, token string) (func(context.Context) (*Session, error), error) {
	return s.verifyPortalLogin(ctx, portalClient, c, token)
}

func (s *Service) verifyPortalLogin(ctx context.Context, client *http.Client, c PortalConfig, token string) (func(context.Context) (*Session, error), error) {
	if !c.Enabled || strings.TrimSpace(token) == "" || len(token) > 4096 {
		return nil, ErrInvalidCredentials
	}
	identity, err := verifyPortalToken(ctx, client, c.CheckTokenURL, token)
	if err != nil {
		return nil, err
	}
	home, err := portalURL(c.HomeURL)
	if err != nil {
		return nil, ErrPortalUnavailable
	}
	issuer := store.PortalIdentityIssuerPrefix + "https://" + strings.ToLower(home.Host)
	return func(ctx context.Context) (*Session, error) {
		roleID, err := s.defaultUserRoleID(ctx)
		if err != nil {
			return nil, ErrPortalUnavailable
		}
		uid := newSecretToken()
		display := strings.TrimSpace(identity.NickName)
		if display == "" {
			display = identity.Username
		}
		u, err := s.users.ResolveIdentity(ctx, issuer, identity.Username, &store.User{
			ID: uid, Username: "portal-" + uid, DisplayName: display, Email: identity.Email, RoleID: roleID,
		})
		if err != nil {
			return nil, ErrPortalUnavailable
		}
		return s.issue(u, "portal", s.now().Add(SessionTTL))
	}, nil
}

func (s *Service) SessionSource(token string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokens[tokenHash(token)].source
}
