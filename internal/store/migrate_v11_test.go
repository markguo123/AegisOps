package store

import (
	"context"
	"testing"
)

func TestMigrationV11RejectsLegacyGrantWithoutChangingIt(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	g := &AuthzGrant{ID: "legacy", PrincipalType: "user", PrincipalID: "old", Namespaces: `["team"]`, Permissions: `["*"]`}
	if err := st.AuthzGrants().Create(ctx, g); err != nil {
		t.Fatal(err)
	}
	db := rawDB(t, st)
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version=11"); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err == nil {
		t.Fatal("unsafe legacy migration accepted")
	}
	persisted, err := st.AuthzGrants().Get(ctx, g.ID)
	if err != nil || persisted.Permissions != g.Permissions || persisted.ScopeType != "" {
		t.Fatal("failed upgrade changed policy", err)
	}
	if err := st.AuthzGrants().Delete(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
}
