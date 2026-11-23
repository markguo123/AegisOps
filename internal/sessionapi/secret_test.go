package sessionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecretRevealScopeAndAudit(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	users := auth.New(st.Users(), st.Roles())
	if err = users.Bootstrap(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	roles, _ := st.Roles().List(ctx)
	var role *store.Role
	for _, r := range roles {
		if r.Name == "user" {
			role = r
		}
	}
	if role == nil {
		t.Fatal("user role missing")
	}
	user, err := users.CreateUser(ctx, auth.CreateUserRequest{Username: "reader", Password: "password-123", RoleID: role.ID})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	kube := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/v1/namespaces/team/secrets/db" {
			t.Errorf("unexpected Kubernetes request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"team"},"data":{"password":"aHVudGVyMg=="}}`)
	}))
	defer kube.Close()
	cipher, _ := cluster.NewCipher(make([]byte, 32))
	config := []byte(fmt.Sprintf("apiVersion: v1\nkind: Config\ncurrent-context: test\nclusters:\n- name: c\n  cluster:\n    server: %s\ncontexts:\n- name: test\n  context:\n    cluster: c\n    user: u\nusers:\n- name: u\n  user:\n    token: fake\n", kube.URL))
	encrypted, err := cipher.Encrypt(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Clusters().Create(ctx, &store.Cluster{ID: "c", Name: "test"}); err != nil {
		t.Fatal(err)
	}
	if err = st.Credentials().Create(ctx, &store.Credential{ID: "credential", ClusterID: "c", KubeconfigSecret: encrypted}); err != nil {
		t.Fatal(err)
	}
	grants := authz.New(st)
	if _, err = grants.CreateResourceGrant(ctx, authz.ResourceGrantRequest{UserID: user.ID, ClusterID: "c", ScopeType: "namespace", Namespaces: []string{"team"}, Actions: []string{"read"}}, user.ID); err != nil {
		t.Fatal(err)
	}
	access, _ := grants.EffectiveAccess(ctx, user.ID, "c")
	coverage, _ := json.Marshal(access.Coverage())
	chat := newFakeChat()
	chat.sessions["s"] = &session.Session{ID: "s", UserID: user.ID, Cluster: "c", Coverage: coverage}
	s := New(chat, "model").WithPlatformAccess(&service.PlatformAccess{Grants: grants, Clusters: cluster.New(st.Clusters(), st.Credentials(), cipher)})
	capture := &auditCapture{}
	s.WithAudit(capture.sink)
	handler := s.AuthenticatedHandler(func(context.Context, string) (*store.User, *store.Role, error) { return user, role, nil })
	for _, test := range []struct {
		ns     string
		status int
	}{{"other", 403}, {"team", 200}} {
		req := httptest.NewRequest("POST", "/api/v1/sessions/s/secret/reveal", strings.NewReader(fmt.Sprintf(`{"namespace":%q,"name":"db","keys":["password"]}`, test.ns)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != test.status {
			t.Fatalf("status=%d want=%d body=%s", rec.Code, test.status, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store, private" {
			t.Fatal("missing no-store")
		}
		if test.status == 200 && !strings.Contains(rec.Body.String(), "hunter2") {
			t.Fatal("authorized plaintext missing")
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized request reached Kubernetes: %d", calls)
	}
	raw, _ := json.Marshal(capture.eventsSnapshot())
	if strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "aHVudGVyMg==") {
		t.Fatal("plaintext leaked into audit")
	}
}
