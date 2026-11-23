package service

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/objectstore"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestChatResultStoreUsesObjectGatewayAndExistingGC(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "results.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	backend := gcDirBackend(t)
	gw := NewBlobGateway(backend, nil)
	if err := gw.EnsureReady(ctx); err != nil {
		t.Fatal(err)
	}
	results := chatResultStore{gateway: gw, store: st}
	ref, err := results.Put(ctx, objectstore.Spec{Category: objectstore.CategoryExports, Scope: []string{"run1"}, Ext: ".txt", CreatedBy: "owner1"}, strings.NewReader("redacted evidence"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := st.Objects().Get(ctx, ref.Key)
	if err != nil {
		t.Fatal(err)
	}
	if meta.RefCount != 0 || meta.State != store.ObjectStateOrphaned || meta.DeletedAt == nil || meta.CreatedBy != "owner1" {
		t.Fatalf("transient result not in GC lifecycle: %+v", meta)
	}
	r, _, err := results.Get(ctx, ref.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil || string(raw) != "redacted evidence" {
		t.Fatal("cannot read result during grace period")
	}
}
