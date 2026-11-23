package store

import (
	"context"
	"fmt"
	"time"
)

// ReportRetentionPreview counts reports by the age and minimum-copy policy.
// Potential contains metadata-only candidates pending lifecycle verification.
type ReportRetentionPreview struct {
	Total     int64     `json:"total"`
	Protected int64     `json:"protected"`
	Eligible  int64     `json:"eligible"`
	Excluded  int64     `json:"excluded"`
	Potential int64     `json:"potential"`
	Cutoff    time.Time `json:"cutoff"`
}

func (st *reportStore) PreviewRetention(ctx context.Context, clusterID, kind string, cutoff time.Time, minimum int) (ReportRetentionPreview, error) {
	result := ReportRetentionPreview{Cutoff: cutoff}
	err := st.s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN created_at < ? AND position_in_group > ? THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN created_at < ? AND position_in_group > ? AND retention_finalized = 1 THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT created_at, retention_finalized, ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS position_in_group
			FROM reports WHERE cluster_id = ? AND kind = ?
		) ranked_reports`, cutoff, minimum, cutoff, minimum, clusterID, kind).Scan(&result.Total, &result.Potential, &result.Eligible)
	if err != nil {
		return ReportRetentionPreview{}, mapErr(err, "preview report retention")
	}
	result.Protected = result.Total - result.Potential
	result.Excluded = result.Potential - result.Eligible
	return result, nil
}

func (st *reportStore) FinalizeRetentionScan(ctx context.Context, scanID, clusterID, ownerID string) error {
	if scanID == "" || clusterID == "" || ownerID == "" {
		return nil
	}
	_, err := st.s.db.ExecContext(ctx, `
		UPDATE reports SET retention_finalized = 1
		WHERE scan_id = ? AND cluster_id = ? AND created_by = ?`, scanID, clusterID, ownerID)
	return mapErr(err, "finalize report retention scan")
}

type ReportRetentionPolicy struct {
	ClusterID   string     `json:"clusterId"`
	Kind        string     `json:"kind"`
	Days        int        `json:"days"`
	Minimum     int        `json:"minimum"`
	Enabled     bool       `json:"enabled"`
	NextRun     time.Time  `json:"nextRun"`
	LastRun     *time.Time `json:"lastRun,omitempty"`
	LastDeleted int        `json:"lastDeleted"`
}

func (st *reportStore) ReportRetentionPolicies(ctx context.Context) ([]ReportRetentionPolicy, error) {
	rows, err := st.s.db.QueryContext(ctx, `
		SELECT cluster_id, kind, days, minimum, enabled, next_run, last_run, last_deleted
		FROM report_retention_policies ORDER BY cluster_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]ReportRetentionPolicy, 0)
	for rows.Next() {
		var policy ReportRetentionPolicy
		if err := rows.Scan(&policy.ClusterID, &policy.Kind, &policy.Days, &policy.Minimum,
			&policy.Enabled, &policy.NextRun, &policy.LastRun, &policy.LastDeleted); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (st *reportStore) SetReportRetentionPolicy(ctx context.Context, p ReportRetentionPolicy, actor string) error {
	return st.s.WithTx(ctx, func(tx *Tx) error {
		columns := []string{"cluster_id", "kind", "days", "minimum", "enabled", "next_run"}
		query := st.s.dialect.upsert("report_retention_policies", columns, columns[:2], columns[2:])
		if _, err := tx.tx.ExecContext(ctx, query, p.ClusterID, p.Kind, p.Days, p.Minimum, boolInt(p.Enabled), p.NextRun); err != nil {
			return err
		}
		action := "report.retention.disable"
		if p.Enabled {
			action = "report.retention.enable"
		}
		return writeAuditEvent(ctx, tx.tx, &AuditEvent{UserID: actor, ClusterID: p.ClusterID,
			Action: action, EventType: AuditEventReport, ResourceKind: "ReportRetentionPolicy", ResourceName: p.Kind, Status: "success"})
	})
}

// RunReportRetention claims one due batch by updating the policy row inside the
// deletion transaction. If the process exits before commit another replica can retry it.
func (st *reportStore) RunReportRetention(ctx context.Context, p ReportRetentionPolicy, now time.Time) (int, error) {
	deleted := 0
	err := st.s.WithTx(ctx, func(tx *Tx) error {
		result, err := tx.tx.ExecContext(ctx, `
			UPDATE report_retention_policies SET next_run = ?
			WHERE cluster_id = ? AND kind = ? AND enabled = 1 AND days = ? AND minimum = ?
			AND next_run = ? AND next_run <= ? AND EXISTS (SELECT 1 FROM clusters WHERE id = ?)`,
			now.Add(time.Hour).Truncate(time.Millisecond), p.ClusterID, p.Kind, p.Days, p.Minimum, p.NextRun, now, p.ClusterID)
		if err != nil {
			return err
		}
		claimed, err := result.RowsAffected()
		if err != nil || claimed == 0 {
			return err
		}
		rows, err := tx.tx.QueryContext(ctx, `
			SELECT id, blob_key FROM (
				SELECT id, blob_key, created_at, retention_finalized,
				       ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS position_in_group
				FROM reports WHERE cluster_id = ? AND kind = ?
			) ranked_reports
			WHERE retention_finalized = 1 AND created_at < ? AND position_in_group > ?
			ORDER BY created_at, id LIMIT 200`,
			p.ClusterID, p.Kind, now.AddDate(0, 0, -p.Days), p.Minimum)
		if err != nil {
			return err
		}
		var candidates [][2]string
		for rows.Next() {
			var candidate [2]string
			if err := rows.Scan(&candidate[0], &candidate[1]); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		batchID := fmt.Sprintf("retention:%s:%s:%d", p.ClusterID, p.Kind, p.NextRun.UnixMilli())
		for _, candidate := range candidates {
			ok, missing, err := tx.DeleteReportReference(ctx, candidate[0], candidate[1])
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			summary := "report reference released"
			if missing {
				summary = "object reference already missing"
			}
			if err := writeAuditEvent(ctx, tx.tx, &AuditEvent{RequestID: batchID, ClusterID: p.ClusterID, Action: "report.retention.delete",
				EventType: AuditEventReport, ResourceKind: "Report", ResourceName: candidate[0], Status: "success",
				ResultSummary: summary}); err != nil {
				return err
			}
			deleted++
		}
		_, err = tx.tx.ExecContext(ctx, `
			UPDATE report_retention_policies SET last_run = ?, last_deleted = ? WHERE cluster_id = ? AND kind = ?`,
			now, deleted, p.ClusterID, p.Kind)
		if err != nil {
			return err
		}
		return writeAuditEvent(ctx, tx.tx, &AuditEvent{RequestID: batchID, ClusterID: p.ClusterID,
			Action: "report.retention.run", EventType: AuditEventReport, ResourceKind: "ReportRetentionPolicy",
			ResourceName: p.Kind, Status: "success", ResultSummary: fmt.Sprintf("deleted=%d", deleted)})
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

func (t *Tx) LockReportRetentionPolicy(ctx context.Context, clusterID, kind string) error {
	_, err := t.tx.ExecContext(ctx, `
		UPDATE report_retention_policies SET enabled = enabled WHERE cluster_id = ? AND kind = ?`, clusterID, kind)
	return err
}
