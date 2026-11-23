package authz

import (
	"encoding/json"
	"sync"
	"testing"
)

func TestCoverageRecorderAccumulatesWithoutExpandingBoundary(t *testing.T) {
	base := Coverage{Version: 1, ClusterID: "c", Targets: []Target{{ClusterID: "c", Namespace: "team", Actions: []string{"read"}}}}
	recorder := NewCoverageRecorder(base)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder.Observe(Target{ClusterID: "c", Namespace: "team", Resource: "/pods", Actions: []string{"read"}})
		}()
	}
	wg.Wait()
	var coverage Coverage
	if err := json.Unmarshal(recorder.JSON(), &coverage); err != nil {
		t.Fatal(err)
	}
	if len(coverage.Observed) != 1 || len(coverage.Targets) != 1 || coverage.Completeness != "conservative" {
		t.Fatalf("%+v", coverage)
	}
	next := NewCoverageRecorder(coverage)
	next.Observe(Target{ClusterID: "c", Namespace: "team", Resource: "/events", Actions: []string{"read"}})
	if err := json.Unmarshal(next.JSON(), &coverage); err != nil {
		t.Fatal(err)
	}
	if len(coverage.Observed) != 2 {
		t.Fatal("prior turn targets lost")
	}
}
