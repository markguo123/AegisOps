package authz

import (
	"context"
	"encoding/json"
	"sync"
)

// Coverage is the conservative upper bound of data that can enter one artifact.
// Execution never expands these boundaries when a later grant adds new access.
type Coverage struct {
	Completeness string   `json:"completeness,omitempty"`
	Observed     []Target `json:"observed,omitempty"`
	Version      int      `json:"version"`
	ClusterID    string   `json:"clusterId"`
	Targets      []Target `json:"targets"`
}

func (a EffectiveAccess) Coverage() Coverage {
	c := Coverage{Version: 1, ClusterID: a.ClusterID}
	seen := map[[2]string]bool{}
	for _, rule := range a.Rules {
		ns := rule.Namespaces
		cluster := rule.ScopeType == ScopeCluster
		if cluster {
			ns = []string{""}
		}
		for _, n := range ns {
			for _, act := range []string{ActionRead, ActionLogs, ActionSecretRead, ActionExec, ActionShell} {
				t := Target{ClusterID: a.ClusterID, Namespace: n, ClusterScoped: cluster, Actions: []string{act}}
				key := [2]string{n, act}
				if !seen[key] && a.Check(t) == nil {
					seen[key] = true
					c.Targets = append(c.Targets, t)
				}
			}
		}
	}
	return c
}
func (c Coverage) CoveredBy(a *EffectiveAccess) bool {
	if c.Version != 1 || a == nil || c.ClusterID != a.ClusterID || len(c.Targets) == 0 {
		return false
	}
	for _, t := range c.Targets {
		if a.Check(t) != nil {
			return false
		}
	}
	return true
}

// Limit retains writes from current grants, but only within the frozen data boundary.
func (c Coverage) Limit(a *EffectiveAccess) *EffectiveAccess {
	out := &EffectiveAccess{ClusterID: c.ClusterID}
	for _, t := range c.Targets {
		if len(t.Actions) != 1 {
			continue
		}
		r := AccessRule{ScopeType: ScopeNamespace, Namespaces: []string{t.Namespace}, ResourceProfile: "builtin_namespaced", Actions: []string{t.Actions[0]}}
		if t.ClusterScoped {
			r.ScopeType = ScopeCluster
			r.ResourceProfile = "builtin_cluster"
			r.Namespaces = nil
		}
		if t.Actions[0] == ActionRead {
			for _, act := range []string{ActionOperate, ActionDestroy} {
				x := t
				x.Actions = []string{act}
				if a.Check(x) == nil {
					r.Actions = append(r.Actions, act)
				}
			}
		}
		out.Rules = append(out.Rules, r)
	}
	return out
}

type AccessibleCluster struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Status string           `json:"status"`
	Access *EffectiveAccess `json:"access"`
}

func (s *Service) AccessibleClusters(ctx context.Context, uid string) ([]AccessibleCluster, error) {
	rows, err := s.st.Clusters().List(ctx)
	if err != nil {
		return nil, err
	}
	out := []AccessibleCluster{}
	for _, row := range rows {
		a, err := s.EffectiveAccess(ctx, uid, row.ID)
		if err != nil {
			return nil, err
		}
		if len(a.Coverage().Targets) > 0 {
			out = append(out, AccessibleCluster{row.ID, row.Name, row.Status, a})
		}
	}
	return out, nil
}

// CoverageRecorder accumulates the observed targets under the conservative
// execution boundary. Unknown tool internals retain that wider boundary.
type CoverageRecorder struct {
	mu       sync.Mutex
	coverage Coverage
}

func NewCoverageRecorder(initial Coverage) *CoverageRecorder {
	initial.Completeness = "conservative"
	return &CoverageRecorder{coverage: initial}
}
func (r *CoverageRecorder) Observe(t Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	encoded, _ := json.Marshal(t)
	for _, old := range r.coverage.Observed {
		raw, _ := json.Marshal(old)
		if string(encoded) == string(raw) {
			return
		}
	}
	t.Actions = append([]string(nil), t.Actions...)
	r.coverage.Observed = append(r.coverage.Observed, t)
}
func (r *CoverageRecorder) JSON() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, _ := json.Marshal(r.coverage)
	return data
}
