package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"golang.org/x/oauth2"
)

func TestEnterpriseLoginCodeFlow(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var issuer, nonce, challenge, group, invalid string
	var mu sync.Mutex
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	defer srv.Close()
	issuer = srv.URL
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.FormValue("code") != "code" || oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != challenge {
			http.Error(w, "bad verifier", 400)
			return
		}
		now := time.Now()
		values := map[string]any{"iss": issuer, "aud": "client", "sub": "stable", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "nonce": nonce, "groups": []string{group}}
		switch invalid {
		case "issuer":
			values["iss"] = "https://other.test"
		case "audience":
			values["aud"] = "other"
		case "nonce":
			values["nonce"] = "other"
		case "expiry":
			values["exp"] = now.Add(-time.Hour).Unix()
		case "future":
			values["iat"] = now.Add(time.Hour).Unix()
		case "azp":
			values["azp"] = "other"
		case "subject":
			values["sub"] = ""
		}
		claims, _ := json.Marshal(values)
		signed, e := signer.Sign(claims)
		if e != nil {
			t.Error(e)
			return
		}
		raw, e := signed.CompactSerialize()
		if e != nil {
			t.Error(e)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id_token": raw, "access_token": "temporary", "token_type": "Bearer"})
	})
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, srv.Client())
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	if err := svc.Bootstrap(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	login, err := NewEnterpriseLogin(ctx, svc, st.Settings(), cipher, "https://platform.test", nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := ProviderConfig{Enabled: true, Issuer: issuer, ClientID: "client", Secret: "secret", AllowedGroups: []string{"team"}}
	if err := login.Configure(ctx, cfg, false); err != nil {
		t.Fatal(err)
	}
	run := func(g string) (*Session, error) {
		cookie, redirect, e := login.Begin(ctx, nil, "")
		if e != nil {
			t.Fatal(e)
		}
		u, e := url.Parse(redirect)
		if e != nil {
			t.Fatal(e)
		}
		q := u.Query()
		mu.Lock()
		group = g
		nonce = q.Get("nonce")
		challenge = q.Get("code_challenge")
		mu.Unlock()
		result, e := login.Finish(ctx, cookie, q.Get("state"), "code", "")
		if _, replay := login.Finish(ctx, cookie, q.Get("state"), "code", ""); replay == nil {
			t.Fatal("callback replay")
		}
		return result, e
	}
	if _, err := run("other"); err == nil {
		t.Fatal("unauthorized group")
	}
	for _, mode := range []string{"issuer", "audience", "nonce", "expiry", "future", "azp", "subject"} {
		mu.Lock()
		invalid = mode
		mu.Unlock()
		if _, err := run("team"); err == nil {
			t.Fatalf("accepted invalid %s", mode)
		}
	}
	mu.Lock()
	invalid = ""
	mu.Unlock()
	cookie, redirect, err := login.Begin(ctx, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse(redirect)
	if _, err := login.Finish(ctx, "other-browser", uri.Query().Get("state"), "code", ""); err == nil {
		t.Fatal("wrong browser")
	}
	if _, err := login.Finish(ctx, cookie, "wrong-state", "code", ""); err == nil {
		t.Fatal("wrong state")
	}
	if _, err := login.Finish(ctx, cookie, uri.Query().Get("state"), "code", ""); err == nil {
		t.Fatal("failed transaction reused")
	}
	first, err := run("team")
	if err != nil {
		t.Fatal(err)
	}
	again, err := run("team")
	if err != nil || first.UserID != again.UserID {
		t.Fatal("identity changed", err)
	}
	_, role, err := svc.UserByToken(ctx, first.Token)
	if err != nil || role.Name != RoleUser {
		t.Fatal("incorrect role", err)
	}
	cfg.Enabled = false
	if err := login.Configure(ctx, cfg, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RecheckToken(ctx, first.Token); err == nil {
		t.Fatal("disabled provider session survived")
	}
}
