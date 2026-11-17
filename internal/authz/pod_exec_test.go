package authz

import (
	"net/http/httptest"
	"testing"
)

func TestPodExecHTTPGuard(t *testing.T) {
	for _, exec := range []bool{false, true} {
		actions := []string{ActionRead, ActionOperate}
		if exec {
			actions = append(actions, ActionExec)
		}
		a := EffectiveAccess{ClusterID: "c", Rules: []AccessRule{{ScopeType: ScopeNamespace, Namespaces: []string{"team"}, ResourceProfile: "builtin_namespaced", Actions: actions}}}
		for _, tc := range []struct {
			method, path string
			want         bool
		}{{"POST", "/api/v1/namespaces/team/pods/api/exec", exec}, {"GET", "/api/v1/namespaces/team/pods/api/exec", false}, {"POST", "/api/v1/namespaces/other/pods/api/exec", false}, {"GET", "/version", true}, {"GET", "/api/v1/nodes", false}} {
			if err := a.CheckRequest(httptest.NewRequest(tc.method, tc.path, nil)); (err == nil) != tc.want {
				t.Fatalf("exec=%v %s %s: %v", exec, tc.method, tc.path, err)
			}
		}
	}
	if err := (EffectiveAccess{}).CheckRequest(httptest.NewRequest("GET", "/version", nil)); err == nil {
		t.Fatal("report-only user received discovery access")
	}
}
