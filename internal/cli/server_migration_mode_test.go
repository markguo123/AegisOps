package cli

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestServerAutomaticMigrationInitializesEmptyStore(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := prepareServerStore(ctx, st, false); err != nil {
		t.Fatal(err)
	}
	if err := store.IsReady(ctx, st); err != nil {
		t.Fatalf("automatic initialization left store unready: %v", err)
	}
}

func TestServerCheckOnlyRejectsEmptyStoreWithoutCreatingSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "platform.db")
	st, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	err = prepareServerStore(context.Background(), st, true)
	if !errors.Is(err, store.ErrNotReady) || !strings.Contains(err.Error(), "aopsctl db migrate") {
		t.Fatalf("expected actionable not-ready error, got %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("check-only created %d tables", count)
	}
}

func TestServerCheckOnlyAcceptsMigratedStore(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "platform.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := prepareServerStore(ctx, st, true); err != nil {
		t.Fatalf("migrated store was rejected: %v", err)
	}
}
