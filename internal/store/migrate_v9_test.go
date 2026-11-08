package store

import (
	"context"
	"testing"
)

func TestMigrationV9ReplayPreservesLegacyGrantAndUser(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := &User{ID: "legacy-user", Username: "legacy", Status: "active", PasswordHash: "old-hash"}
	if err := st.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	g := &AuthzGrant{ID: "legacy-grant", PrincipalType: "user", PrincipalID: u.ID, Namespaces: `["orders"]`, Permissions: `["*"]`}
	if err := st.AuthzGrants().Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB(t, st).Exec(`DELETE FROM schema_migrations WHERE version=9`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	u2, err := st.Users().Get(ctx, u.ID)
	if err != nil || u2.SessionVersion != 0 || u2.PasswordHash != "old-hash" {
		t.Fatalf("user changed during migration: %+v %v", u2, err)
	}
	g2, err := st.AuthzGrants().Get(ctx, g.ID)
	if err != nil || g2.ScopeType != "" || g2.ResourceProfile != "" || g2.Permissions != `["*"]` {
		t.Fatalf("legacy grant implicitly converted: %+v %v", g2, err)
	}
}

func TestSecurityVersionIsPersistedAndProfileUpdatesDoNotRevoke(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	u := &User{ID: "version-user", Username: "version-user", Status: "active", PasswordHash: "before"}
	if err := st.Users().Create(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.DisplayName = "display edit"
	if err := st.Users().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	if u.SessionVersion != 0 {
		t.Fatal("display-only edit changed security version")
	}
	u.PasswordHash = "after"
	if err := st.Users().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	read, err := st.Users().Get(ctx, u.ID)
	if err != nil || read.SessionVersion != 1 {
		t.Fatalf("security version was not committed: %+v %v", read, err)
	}
}
