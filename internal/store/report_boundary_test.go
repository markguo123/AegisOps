package store

import (
	"context"
	"fmt"
	"github.com/k8s-ai/k8s-ai/internal/objectstore"
	"testing"
	"time"
)

func TestReportCoverageFiltersSQLTotalsAndRows(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i, tc := range []struct {
		owner, cluster, kind, value string
		level                       int
	}{
		{"viewer", "c", "namespace", "team", 1},
		{objectstore.SystemActor, "c", "namespace", "team", 1},
		{"other", "c", "namespace", "team", 1},
		{"viewer", "other", "namespace", "team", 1},
		{"viewer", "c", "namespace", "team", 2},
		{"viewer", "c", "namespace", "team", 0},
		{"viewer", "c", "cluster", "", 2},
		{"viewer", "c", "pod", "team/pod", 1},
		{"viewer", "c", "namespace", "team-other", 1},
	} {
		id := fmt.Sprint(i)
		ref := objTestRef(t, objectstore.CategoryReports, []string{tc.cluster, id}, ".md", id)
		row := &Report{ID: id, ClusterID: tc.cluster, Kind: "latest-md", BlobKey: ref.Key, MediaType: ref.MediaType, SHA256: ref.SHA256, Size: ref.Size, CreatedBy: tc.owner, ScopeKind: tc.kind, ScopeValue: tc.value, CoverageLevel: tc.level, CreatedAt: time.Now()}
		if err := st.WithTx(ctx, func(tx *Tx) error { return tx.InsertReport(ctx, row) }); err != nil {
			t.Fatal(err)
		}
	}
	q := ReportQuery{ViewerID: "viewer", Boundaries: []ReportBoundary{{ClusterID: "c", Namespace: "team", Level: 1}}, Limit: 1}
	rows, total, err := st.Reports().List(ctx, q)
	if err != nil || total != 3 || len(rows) != 1 {
		t.Fatalf("rows=%d total=%d err=%v", len(rows), total, err)
	}
	stats, err := st.Reports().Stats(ctx, q)
	if err != nil || stats.Total != total {
		t.Fatal("summary bypass", stats, err)
	}
	q.Boundaries = []ReportBoundary{}
	rows, total, err = st.Reports().List(ctx, q)
	if err != nil || total != 0 || len(rows) != 0 {
		t.Fatal("revoked reports visible", err)
	}
}
