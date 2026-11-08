package service

import (
	"context"
	"errors"
	"github.com/k8s-ai/k8s-ai/internal/auth"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"net/http/httptest"
	"os"
	"testing"
)

func TestBoundRunRevocationAndCredentialRotation(t *testing.T) {
	ctx := context.Background()
	st := newArchiveStore(t)
	users := auth.New(st.Users(), st.Roles())
	if err := users.Bootstrap(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	roles, err := st.Roles().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var userRole string
	for _, role := range roles {
		if role.Name == auth.RoleUser {
			userRole = role.ID
		}
	}
	user, err := users.CreateUser(ctx, auth.CreateUserRequest{Username: "tester", Password: "password-123", RoleID: userRole})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("apiVersion: v1\nkind: Config\ncurrent-context: local\nclusters:\n- name: c\n  cluster:\n    server: https://unused.test\ncontexts:\n- name: local\n  context:\n    cluster: c\n    user: u\nusers:\n- name: u\n  user:\n    token: example\n")
	enc, err := cipher.Encrypt(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Clusters().Create(ctx, &store.Cluster{ID: "c", Name: "cluster"}); err != nil {
		t.Fatal(err)
	}
	cred := &store.Credential{ID: "cred", ClusterID: "c", KubeconfigSecret: enc}
	if err := st.Credentials().Create(ctx, cred); err != nil {
		t.Fatal(err)
	}
	p := PlatformAccess{authz.New(st), cluster.New(st.Clusters(), st.Credentials(), cipher)}
	req := authz.ResourceGrantRequest{ClusterID: "c", UserID: user.ID, ScopeType: "namespace", Namespaces: []string{"team"}, Actions: []string{"read"}}
	g, err := p.Grants.CreateResourceGrant(ctx, req, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.Grants.EffectiveAccess(ctx, user.ID, "c")
	if err != nil {
		t.Fatal(err)
	}
	active := true
	run, err := p.Bind(ctx, user.ID, a.Coverage(), func(context.Context) error {
		if !active {
			return errors.New("session revoked")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	check := func(path string, want bool) {
		t.Helper()
		err := run.CheckRequest(httptest.NewRequest("GET", path, nil))
		if (err == nil) != want {
			t.Fatalf("%s allowed=%t err=%v", path, want, err)
		}
	}
	const pods = "/api/v1/namespaces/team/pods"
	check(pods, true)
	check("/api/v1/namespaces/other/pods", false)
	check("/api/v1/nodes", false)
	check(pods+"/p/log", false)
	active = false
	check(pods, false)
	active = true
	if err := p.Grants.DeleteGrant(ctx, g.GrantID); err != nil {
		t.Fatal(err)
	}
	check(pods, false)
	if _, err := p.Grants.CreateResourceGrant(ctx, req, user.ID); err != nil {
		t.Fatal(err)
	}
	check(pods, true)
	cred.KubeconfigSecret, err = cipher.Encrypt(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Credentials().Update(ctx, cred); err != nil {
		t.Fatal(err)
	}
	check(pods, false)
	run.Close()
	if _, err := os.Stat(run.Path); !os.IsNotExist(err) {
		t.Fatal("credential file retained")
	}
}
