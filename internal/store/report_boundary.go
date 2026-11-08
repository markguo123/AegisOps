package store

import "strings"

// ReportBoundary is compiled from current grants. Level 1 covers reads,
// level 2 also covers logs; level 0 on a stored row means unknown legacy data.
type ReportBoundary struct {
	ClusterID, Namespace string
	Cluster              bool
	Level                int
}

func (b ReportBoundary) Matches(r *Report) bool {
	if b.ClusterID != r.ClusterID || r.CoverageLevel < 1 || r.CoverageLevel > b.Level {
		return false
	}
	if b.Cluster {
		return r.ScopeKind == "cluster"
	}
	ns, _, _ := strings.Cut(r.ScopeValue, "/")
	return (r.ScopeKind == "namespace" || r.ScopeKind == "pod") && ns != "" && (b.Namespace == "*" || b.Namespace == ns)
}
func (b ReportBoundary) predicate() (string, []any) {
	prefix := "cluster_id=? AND coverage_level BETWEEN 1 AND ? AND "
	args := []any{b.ClusterID, b.Level}
	if b.Cluster {
		return prefix + "scope_kind='cluster'", args
	}
	if b.Namespace == "*" {
		return prefix + "scope_kind IN ('namespace','pod') AND scope_value<>''", args
	}
	return prefix + "((scope_kind='namespace' AND scope_value=?) OR (scope_kind='pod' AND scope_value LIKE ?))", append(args, b.Namespace, b.Namespace+"/%")
}
