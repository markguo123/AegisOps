package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestPortalLoginUsesVerifiedIdentityAndLocalRole(t *testing.T) {
	const valid = "employee.a+b&c"
	portal := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/authcenter/auth/checktoken" {
			t.Errorf("unexpected Portal request method or path")
		}
		switch r.URL.Query().Get("token") {
		case valid:
			io.WriteString(w, `{"code":"000000","data":{"username":"admin","nickName":"员工"}}`)
		case "missing-user":
			io.WriteString(w, `{"code":"000000","data":{}}`)
		default:
			io.WriteString(w, `{"code":"100000"}`)
		}
	}))
	defer portal.Close()

	ctx := context.Background()
	st := newStore(t)
	svc := New(st.Users(), st.Roles())
	if err := svc.Bootstrap(ctx, "test-password"); err != nil {
		t.Fatal(err)
	}
	cfg := PortalConfig{Enabled: true, HomeURL: portal.URL}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"invalid", "missing-user", ""} {
		if _, err := svc.verifyPortalLogin(ctx, portal.Client(), cfg, token); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("rejected token returned %v", err)
		}
	}
	cfg.Enabled = false
	if _, err := svc.verifyPortalLogin(ctx, portal.Client(), cfg, valid); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("disabled Portal returned %v", err)
	}
	cfg.Enabled = true
	id := ""
	for range 2 {
		issue, err := svc.verifyPortalLogin(ctx, portal.Client(), cfg, valid)
		if err != nil {
			t.Fatal(err)
		}
		session, err := issue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		u, role, err := svc.UserByToken(ctx, session.Token)
		if err != nil {
			t.Fatal(err)
		}
		if u.Source != store.UserSourcePortal || u.Username == "admin" || u.DisplayName != "员工" || role.Name != RoleUser || svc.SessionSource(session.Token) != "portal" {
			t.Fatalf("unexpected user or role: %s %s %s", u.Username, u.Source, role.Name)
		}
		if id != "" && id != u.ID {
			t.Fatal("repeat login created a duplicate user")
		}
		id = u.ID
	}
	if _, _, err := svc.UserByToken(ctx, valid); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("Portal token accepted as a local session: %v", err)
	}
}
