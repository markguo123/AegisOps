// Adapted from Argo CD TestIDTokenClaims, Apache-2.0.
package auth

import (
	"golang.org/x/oauth2"
	"net/url"
	"testing"
)

func TestIDTokenClaims(t *testing.T) {
	opt, err := createClaimsAuthenticationRequestParameter(map[string]essentialClaim{"teams": {Essential: true}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://issuer.test/auth"}}
	u, err := url.Parse(cfg.AuthCodeURL("state", opt))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("claims"); got != `{"id_token":{"teams":{"essential":true}}}` {
		t.Fatal(got)
	}
}
