package cli

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func runDBCheck(t *testing.T, ctx context.Context, cfg *config.Config, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd(testDeps(cfg, &fakeScan{}))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"db", "check"}, args...))
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestDBCheckEmptyStoreDoesNotCreateSchema(t *testing.T) {
	cfg := defaultCfg(t)
	cfg.Platform.Store.SQLitePath = filepath.Join(t.TempDir(), "platform.db")
	_, err := runDBCheck(t, context.Background(), cfg)
	if !errors.Is(err, store.ErrNotReady) || !strings.Contains(err.Error(), "aopsctl db migrate") {
		t.Fatalf("expected actionable not-ready error, got %v", err)
	}
	db, err := sql.Open("sqlite", cfg.Platform.Store.SQLitePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("schema check created %d tables", count)
	}
}

func TestDBCheckMigratedStore(t *testing.T) {
	cfg := defaultCfg(t)
	cfg.Platform.Store.SQLitePath = filepath.Join(t.TempDir(), "platform.db")
	st, _, err := openPlatformStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatal(err)
	}
	st.Close()
	out, err := runDBCheck(t, context.Background(), cfg)
	if err != nil || !strings.Contains(out, "database schema ready") {
		t.Fatalf("migrated store rejected: %v (%s)", err, out)
	}
}

func TestDBCheckWaitTimeout(t *testing.T) {
	cfg := defaultCfg(t)
	cfg.Platform.Store.SQLitePath = filepath.Join(t.TempDir(), "platform.db")
	start := time.Now()
	_, err := runDBCheck(t, context.Background(), cfg, "--wait=20ms")
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "aopsctl db migrate") {
		t.Fatalf("expected actionable timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait exceeded deadline by excessive margin: %v", elapsed)
	}
}

func TestDBCheckCanceledContext(t *testing.T) {
	cfg := defaultCfg(t)
	cfg.Platform.Store.SQLitePath = filepath.Join(t.TempDir(), "platform.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := runDBCheck(t, ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestDBCheckRejectsNegativeWait(t *testing.T) {
	_, err := runDBCheck(t, context.Background(), defaultCfg(t), "--wait=-1s")
	if err == nil || !strings.Contains(err.Error(), "--wait must not be negative") {
		t.Fatalf("expected negative wait rejection, got %v", err)
	}
}
