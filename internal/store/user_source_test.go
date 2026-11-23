package store

import (
	"context"
	"testing"
)

// runUserSource 锁用户列表的来源标识：本地口令账号留空（由 auth 层归一为 local），
// 只有外部身份的账号标 oidc，本地口令与外部身份并存的账号标 local+oidc。
func runUserSource(t *testing.T, s Store) {
	t.Helper()
	ctx := context.Background()

	local := &User{ID: "u-src-local", Username: "src-local", PasswordHash: "hash", RoleID: "r-src", Status: "active"}
	if err := s.Users().Create(ctx, local); err != nil {
		t.Fatalf("create local: %v", err)
	}
	bound := &User{ID: "u-src-bound", Username: "src-bound", PasswordHash: "hash", RoleID: "r-src", Status: "active"}
	if err := s.Users().Create(ctx, bound); err != nil {
		t.Fatalf("create bound: %v", err)
	}
	candidate := &User{ID: "u-src-oidc", Username: "oidc-SRCOIDC", DisplayName: "外部用户", Email: "ext@example.com", RoleID: "r-src"}
	if _, err := s.Users().ResolveIdentity(ctx, "https://idp.example.com/oidc", "subject-src", candidate); err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if err := s.Users().BindIdentity(ctx, "u-src-bound", bound.SessionVersion, "https://idp.example.com/oidc", "subject-bound"); err != nil {
		t.Fatalf("bind identity: %v", err)
	}

	list, err := s.Users().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := map[string]string{}
	for _, u := range list {
		got[u.ID] = u.Source
	}
	if got["u-src-local"] != "" {
		t.Fatalf("本地账号来源应留空待归一, got %q", got["u-src-local"])
	}
	if got["u-src-oidc"] != UserSourceOIDC {
		t.Fatalf("外部身份账号来源应为 %s, got %q", UserSourceOIDC, got["u-src-oidc"])
	}
	if got["u-src-bound"] != UserSourceBoth {
		t.Fatalf("已绑定外部身份的本地账号来源应为 %s, got %q", UserSourceBoth, got["u-src-bound"])
	}

	// 单条读取同样回填来源：/auth/me 靠它决定右上角显示登录名还是显示名。
	one, err := s.Users().Get(ctx, "u-src-oidc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if one.Source != UserSourceOIDC {
		t.Fatalf("Get 应回填来源 %s, got %q", UserSourceOIDC, one.Source)
	}
	if one.DisplayName != "外部用户" || one.Email != "ext@example.com" {
		t.Fatalf("外部身份建档资料未落库: %+v", one)
	}
	if boundUser, err := s.Users().Get(ctx, "u-src-bound"); err != nil {
		t.Fatalf("get bound: %v", err)
	} else if boundUser.Source != UserSourceBoth {
		t.Fatalf("Get 已绑定账号来源应为 %s, got %q", UserSourceBoth, boundUser.Source)
	}
	if localUser, err := s.Users().Get(ctx, "u-src-local"); err != nil {
		t.Fatalf("get local: %v", err)
	} else if localUser.Source != "" {
		t.Fatalf("本地账号来源应留空待归一, got %q", localUser.Source)
	}

	// 再次登录同一外部身份：已有资料不被 IdP 回传覆盖（管理员改过的显示名要留得住）。
	again, err := s.Users().ResolveIdentity(ctx, "https://idp.example.com/oidc", "subject-src",
		&User{ID: "u-src-ignored", Username: "oidc-IGNORED", DisplayName: "改名了", Email: "new@example.com"})
	if err != nil {
		t.Fatalf("re-resolve: %v", err)
	}
	if again.ID != "u-src-oidc" || again.DisplayName != "外部用户" || again.Email != "ext@example.com" {
		t.Fatalf("已有建档资料被覆盖: %+v", again)
	}

	// 建档时是空的字段，由下一次登录的 IdP claim 补齐。
	if _, err := s.Users().ResolveIdentity(ctx, "https://idp.example.com/oidc", "subject-empty",
		&User{ID: "u-src-empty", Username: "oidc-EMPTY", RoleID: "r-src"}); err != nil {
		t.Fatalf("resolve empty profile: %v", err)
	}
	filled, err := s.Users().ResolveIdentity(ctx, "https://idp.example.com/oidc", "subject-empty",
		&User{ID: "u-src-unused", Username: "oidc-UNUSED", DisplayName: "张三", Email: "zhangsan@example.com"})
	if err != nil {
		t.Fatalf("fill profile: %v", err)
	}
	if filled.DisplayName != "张三" || filled.Email != "zhangsan@example.com" {
		t.Fatalf("空字段应由 IdP claim 补齐: %+v", filled)
	}
}

func TestUserSourceSQLite(t *testing.T) {
	runUserSource(t, newTestStore(t))
}
