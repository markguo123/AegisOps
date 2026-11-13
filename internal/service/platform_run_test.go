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
	"strings"
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

// TestPlatformBindIsolatesClusters 是 P12.5 的多集群隔离回归：B/C 使用同一
// namespace 与资源名，授权与凭据必须按 ClusterID 精确隔离；临时 kubeconfig
// 只包含选定目标的 server，不串到另一个集群。
func TestPlatformBindIsolatesClusters(t *testing.T) {
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
	user, err := users.CreateUser(ctx, auth.CreateUserRequest{Username: "multi", Password: "password-123", RoleID: userRole})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	kubeconfig := func(server string) []byte {
		return []byte("apiVersion: v1\nkind: Config\ncurrent-context: ctx\nclusters:\n- name: c\n  cluster:\n    server: " +
			server + "\ncontexts:\n- name: ctx\n  context:\n    cluster: c\n    user: u\nusers:\n- name: u\n  user:\n    token: example\n")
	}
	for _, item := range []struct{ id, name, server string }{
		{"cluster-b", "B", "https://b.example:6443"},
		{"cluster-c", "C", "https://c.example:6443"},
	} {
		if err := st.Clusters().Create(ctx, &store.Cluster{ID: item.id, Name: item.name}); err != nil {
			t.Fatal(err)
		}
		enc, err := cipher.Encrypt(kubeconfig(item.server))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Credentials().Create(ctx, &store.Credential{ID: "cred-" + item.id, ClusterID: item.id, KubeconfigSecret: enc}); err != nil {
			t.Fatal(err)
		}
	}
	p := PlatformAccess{authz.New(st), cluster.New(st.Clusters(), st.Credentials(), cipher)}
	req := authz.ResourceGrantRequest{ClusterID: "cluster-b", UserID: user.ID, ScopeType: "namespace", Namespaces: []string{"team"}, Actions: []string{"read"}}
	if _, err := p.Grants.CreateResourceGrant(ctx, req, user.ID); err != nil {
		t.Fatal(err)
	}
	aB, err := p.Grants.EffectiveAccess(ctx, user.ID, "cluster-b")
	if err != nil {
		t.Fatal(err)
	}
	runB, err := p.Bind(ctx, user.ID, aB.Coverage(), func(context.Context) error { return nil })
	if err != nil {
		t.Fatalf("B 授权存在时应可绑定: %v", err)
	}
	defer runB.Close()
	if err := runB.CheckRequest(httptest.NewRequest("GET", "/api/v1/namespaces/team/pods", nil)); err != nil {
		t.Fatalf("B 授权不应被 C 影响: %v", err)
	}
	// B 的授权不能用于 C：同一用户、同一 namespace 也必须拒绝。
	aC, err := p.Grants.EffectiveAccess(ctx, user.ID, "cluster-c")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Bind(ctx, user.ID, aC.Coverage(), func(context.Context) error { return nil }); err == nil {
		t.Fatal("B 的授权不得用于 C")
	}
	// 给 C 独立授权后，两者临时凭据必须各自只包含目标 server。
	req.ClusterID = "cluster-c"
	if _, err := p.Grants.CreateResourceGrant(ctx, req, user.ID); err != nil {
		t.Fatal(err)
	}
	aC, err = p.Grants.EffectiveAccess(ctx, user.ID, "cluster-c")
	if err != nil {
		t.Fatal(err)
	}
	runC, err := p.Bind(ctx, user.ID, aC.Coverage(), func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer runC.Close()
	if runB.Path == runC.Path {
		t.Fatal("B/C 必须使用不同的临时 kubeconfig")
	}
	rawB, err := os.ReadFile(runB.Path)
	if err != nil {
		t.Fatal(err)
	}
	rawC, err := os.ReadFile(runC.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawB), "b.example") || strings.Contains(string(rawB), "c.example") {
		t.Fatalf("B 临时凭据串到了其它集群: %s", rawB)
	}
	if !strings.Contains(string(rawC), "c.example") || strings.Contains(string(rawC), "b.example") {
		t.Fatalf("C 临时凭据串到了其它集群: %s", rawC)
	}
}

// TestBoundRunCredentialRotationInvalidatesOnlyTarget 验证轮换 B 的凭据只让
// B 的旧运行失效，C 的并行运行不受影响。
func TestBoundRunCredentialRotationInvalidatesOnlyTarget(t *testing.T) {
	ctx := context.Background()
	st := newArchiveStore(t)
	users := auth.New(st.Users(), st.Roles())
	if err := users.Bootstrap(ctx, "admin-password"); err != nil {
		t.Fatal(err)
	}
	roles, _ := st.Roles().List(ctx)
	var userRole string
	for _, role := range roles {
		if role.Name == auth.RoleUser {
			userRole = role.ID
		}
	}
	user, err := users.CreateUser(ctx, auth.CreateUserRequest{Username: "rotate", Password: "password-123", RoleID: userRole})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := cluster.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	config := []byte("apiVersion: v1\nkind: Config\ncurrent-context: local\nclusters:\n- name: c\n  cluster:\n    server: https://unused.test\ncontexts:\n- name: local\n  context:\n    cluster: c\n    user: u\nusers:\n- name: u\n  user:\n    token: example\n")
	var creds []*store.Credential
	for _, id := range []string{"cluster-b", "cluster-c"} {
		if err := st.Clusters().Create(ctx, &store.Cluster{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
		enc, _ := cipher.Encrypt(config)
		cred := &store.Credential{ID: "cred-" + id, ClusterID: id, KubeconfigSecret: enc}
		if err := st.Credentials().Create(ctx, cred); err != nil {
			t.Fatal(err)
		}
		creds = append(creds, cred)
	}
	p := PlatformAccess{authz.New(st), cluster.New(st.Clusters(), st.Credentials(), cipher)}
	for _, id := range []string{"cluster-b", "cluster-c"} {
		req := authz.ResourceGrantRequest{ClusterID: id, UserID: user.ID, ScopeType: "namespace", Namespaces: []string{"team"}, Actions: []string{"read"}}
		if _, err := p.Grants.CreateResourceGrant(ctx, req, user.ID); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(id string) *BoundRun {
		a, err := p.Grants.EffectiveAccess(ctx, user.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		run, err := p.Bind(ctx, user.ID, a.Coverage(), func(context.Context) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	runB, runC := bind("cluster-b"), bind("cluster-c")
	defer runB.Close()
	defer runC.Close()
	// 只轮换 B。
	creds[0].KubeconfigSecret, _ = cipher.Encrypt([]byte("apiVersion: v1\nkind: Config\ncurrent-context: local\nclusters:\n- name: c\n  cluster:\n    server: https://rotated.test\ncontexts:\n- name: local\n  context:\n    cluster: c\n    user: u\nusers:\n- name: u\n  user:\n    token: rotated\n"))
	if err := st.Credentials().Update(ctx, creds[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := runB.Access(ctx); err == nil {
		t.Fatal("B 凭据轮换后旧运行必须失效")
	}
	if _, err := runC.Access(ctx); err != nil {
		t.Fatalf("C 的并行运行不应受 B 轮换影响: %v", err)
	}
}
