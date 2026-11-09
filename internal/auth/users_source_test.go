package auth

import (
	"context"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

// TestListUsersMarksSource 锁用户管理列表的来源口径：本地口令账号 local，
// 企业 OIDC 首次登录建档账号 oidc，已绑定外部身份的本地账号 local+oidc。
// 界面靠这个字段区分「本地账号」与「企业 SSO 账号」，避免只能看到 oidc-随机串。
func TestListUsersMarksSource(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	svc := New(st.Users(), st.Roles())
	if err := svc.Bootstrap(ctx, "pw"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	userRole := sourceRoleID(t, st, RoleUser)

	if _, err := svc.CreateUser(ctx, CreateUserRequest{Username: "ops-a", Password: "password123", RoleID: userRole}); err != nil {
		t.Fatalf("create local user: %v", err)
	}
	bound, err := svc.CreateUser(ctx, CreateUserRequest{Username: "ops-b", Password: "password123", RoleID: userRole})
	if err != nil {
		t.Fatalf("create bindable user: %v", err)
	}
	candidate := &store.User{ID: "u-src-oidc", Username: "oidc-SRCOIDC", DisplayName: "外部用户", Email: "ext@example.com", RoleID: userRole}
	if _, err := st.Users().ResolveIdentity(ctx, "https://idp.example.com/oidc", "subject-a", candidate); err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if err := st.Users().BindIdentity(ctx, bound.ID, bound.SessionVersion, "https://idp.example.com/oidc", "subject-b"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	list, err := svc.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	got := map[string]string{}
	for _, u := range list {
		got[u.Username] = u.Source
	}
	for username, want := range map[string]string{
		DefaultAdminUsername: store.UserSourceLocal,
		"ops-a":              store.UserSourceLocal,
		"ops-b":              store.UserSourceBoth,
		"oidc-SRCOIDC":       store.UserSourceOIDC,
	} {
		if got[username] != want {
			t.Fatalf("用户 %s 来源应为 %s, got %q（全量 %v）", username, want, got[username], got)
		}
	}
}

func sourceRoleID(t *testing.T, st store.Store, name string) string {
	t.Helper()
	roles, err := st.Roles().List(context.Background())
	if err != nil {
		t.Fatalf("list roles: %v", err)
	}
	for _, r := range roles {
		if r.Name == name {
			return r.ID
		}
	}
	t.Fatalf("角色 %q 不存在", name)
	return ""
}
