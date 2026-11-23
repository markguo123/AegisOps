package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ReportRetentionPreview counts reports by the age and minimum-copy policy.
// Potential contains metadata-only candidates pending lifecycle verification.
type ReportRetentionPreview struct {
	Token     string    `json:"token,omitempty"`
	Total     int64     `json:"total"`
	Protected int64     `json:"protected"`
	Eligible  int64     `json:"eligible"`
	Excluded  int64     `json:"excluded"`
	Potential int64     `json:"potential"`
	Cutoff    time.Time `json:"cutoff"`
}

func (st *reportStore) PreviewRetention(ctx context.Context, clusterID, kind string, cutoff time.Time, minimum int) (ReportRetentionPreview, error) {
	return previewReportRetention(ctx, st.s.db, clusterID, kind, cutoff, minimum)
}

func previewReportRetention(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, clusterID, kind string, cutoff time.Time, minimum int) (ReportRetentionPreview, error) {
	result := ReportRetentionPreview{Cutoff: cutoff}
	err := db.QueryRowContext(ctx, `
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
	IntervalMinutes int        `json:"intervalMinutes"`
	Version         int64      `json:"version"`
	ClusterID       string     `json:"clusterId"`
	Kind            string     `json:"kind"`
	Days            int        `json:"days"`
	Minimum         int        `json:"minimum"`
	Enabled         bool       `json:"enabled"`
	NextRun         time.Time  `json:"nextRun"`
	LastRun         *time.Time `json:"lastRun,omitempty"`
	LastDeleted     int        `json:"lastDeleted"`
}

func (st *reportStore) ReportRetentionPolicies(ctx context.Context) ([]ReportRetentionPolicy, error) {
	rows, err := st.s.db.QueryContext(ctx, `
		SELECT cluster_id, kind, days, minimum, interval_minutes, version, enabled, next_run, last_run, last_deleted
		FROM report_retention_policies ORDER BY cluster_id, kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	policies := make([]ReportRetentionPolicy, 0)
	for rows.Next() {
		var policy ReportRetentionPolicy
		if err := rows.Scan(&policy.ClusterID, &policy.Kind, &policy.Days, &policy.Minimum, &policy.IntervalMinutes, &policy.Version,
			&policy.Enabled, &policy.NextRun, &policy.LastRun, &policy.LastDeleted); err != nil {
			return nil, err
		}
		policies = append(policies, policy)
	}
	return policies, rows.Err()
}

func (st *reportStore) SetReportRetentionPolicy(ctx context.Context, p ReportRetentionPolicy, actor string) error {
	return st.s.WithTx(ctx, func(tx *Tx) error {
		if !p.Enabled {
			if err := stopRetentionRun(ctx, tx, p, actor); err != nil {
				return err
			}
			result, err := tx.tx.ExecContext(ctx, `
				UPDATE report_retention_policies SET enabled = 0, version = version + 1
				WHERE cluster_id = ? AND kind = ? AND enabled = 1`, p.ClusterID, p.Kind)
			if err != nil {
				return err
			}
			changed, err := result.RowsAffected()
			if err != nil || changed == 0 {
				return err
			}
			return writeAuditEvent(ctx, tx.tx, &AuditEvent{UserID: actor, ClusterID: p.ClusterID,
				Action: "report.retention.disable", EventType: AuditEventReport, ResourceKind: "ReportRetentionPolicy",
				ResourceName: p.Kind, Status: "success"})
		}
		return st.setReportRetentionPolicy(ctx, tx, p, actor)
	})
}

func (st *reportStore) setReportRetentionPolicy(ctx context.Context, tx *Tx, p ReportRetentionPolicy, actor string) error {
	if err := stopRetentionRun(ctx, tx, p, actor); err != nil {
		return err
	}
	if p.IntervalMinutes == 0 {
		p.IntervalMinutes = 60
	}
	columns := []string{"cluster_id", "kind", "days", "minimum", "interval_minutes", "enabled", "next_run"}
	query := st.s.dialect.upsert("report_retention_policies", columns, columns[:2], columns[2:])
	if _, err := tx.tx.ExecContext(ctx, query, p.ClusterID, p.Kind, p.Days, p.Minimum, p.IntervalMinutes, boolInt(p.Enabled), p.NextRun); err != nil {
		return err
	}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE report_retention_policies SET version = version + 1 WHERE cluster_id = ? AND kind = ?`, p.ClusterID, p.Kind); err != nil {
		return err
	}
	action := "report.retention.enable"
	return writeAuditEvent(ctx, tx.tx, &AuditEvent{UserID: actor, ClusterID: p.ClusterID,
		Action: action, EventType: AuditEventReport, ResourceKind: "ReportRetentionPolicy", ResourceName: p.Kind, Status: "success"})
}

func (t *Tx) LockReportRetentionPolicy(ctx context.Context, clusterID, kind string) error {
	_, err := t.tx.ExecContext(ctx, `
		UPDATE report_retention_policies SET enabled = enabled WHERE cluster_id = ? AND kind = ?`, clusterID, kind)
	return err
}

var ErrRetentionPreviewRequired = errors.New("report retention preview expired or changed")

type retentionPreviewClaim struct {
	Actor  string
	Policy ReportRetentionPolicy
	Cutoff time.Time
	Digest string
}

func retentionTokenHash(token string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(token)))
}

func (st *reportStore) IssueRetentionPreview(ctx context.Context, p ReportRetentionPolicy, actor string, cutoff time.Time) (ReportRetentionPreview, error) {
	var preview ReportRetentionPreview
	err := st.s.WithTx(ctx, func(tx *Tx) error {
		now := time.Now().UTC()
		if _, err := tx.tx.ExecContext(ctx, `DELETE FROM report_retention_previews WHERE expires_at <= ?`, now); err != nil {
			return err
		}
		version, err := retentionPolicyVersion(ctx, tx.tx, p)
		if err != nil {
			return err
		}
		p.Version = version
		preview, err = previewReportRetention(ctx, tx.tx, p.ClusterID, p.Kind, cutoff, p.Minimum)
		if err != nil {
			return err
		}
		digest, err := retentionCandidateDigest(ctx, tx.tx, p, cutoff)
		if err != nil {
			return err
		}
		preview.Token = rand.Text()
		payload, err := json.Marshal(retentionPreviewClaim{Actor: actor, Policy: p, Cutoff: cutoff, Digest: digest})
		if err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, `INSERT INTO report_retention_previews (token_hash, expires_at, payload) VALUES (?, ?, ?)`,
			retentionTokenHash(preview.Token), now.Add(10*time.Minute), string(payload))
		return err
	})
	return preview, err
}

func (st *reportStore) ActivateReportRetention(ctx context.Context, p ReportRetentionPolicy, token, actor string) error {
	if token == "" {
		return ErrRetentionPreviewRequired
	}
	return st.s.WithTx(ctx, func(tx *Tx) error {
		if err := tx.LockReportRetentionPolicy(ctx, p.ClusterID, p.Kind); err != nil {
			return err
		}
		var payload []byte
		now := time.Now().UTC()
		hash := retentionTokenHash(token)
		err := tx.tx.QueryRowContext(ctx, `SELECT payload FROM report_retention_previews WHERE token_hash = ? AND expires_at > ?`, hash, now).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrRetentionPreviewRequired
		}
		if err != nil {
			return err
		}
		var claim retentionPreviewClaim
		if err := json.Unmarshal(payload, &claim); err != nil {
			return err
		}
		if claim.Actor != actor || claim.Policy.ClusterID != p.ClusterID || claim.Policy.Kind != p.Kind ||
			claim.Policy.Days != p.Days || claim.Policy.Minimum != p.Minimum || claim.Policy.IntervalMinutes != p.IntervalMinutes {
			return ErrRetentionPreviewRequired
		}
		version, err := retentionPolicyVersion(ctx, tx.tx, p)
		if err != nil {
			return err
		}
		if version != claim.Policy.Version {
			return ErrRetentionPreviewRequired
		}
		digest, err := retentionCandidateDigest(ctx, tx.tx, p, claim.Cutoff)
		if err != nil {
			return err
		}
		if digest != claim.Digest {
			return ErrRetentionPreviewRequired
		}
		result, err := tx.tx.ExecContext(ctx, `DELETE FROM report_retention_previews WHERE token_hash = ? AND expires_at > ?`, hash, now)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrRetentionPreviewRequired
		}
		return st.setReportRetentionPolicy(ctx, tx, p, actor)
	})
}

func retentionPolicyVersion(ctx context.Context, tx *sql.Tx, p ReportRetentionPolicy) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx, `SELECT version FROM report_retention_policies WHERE cluster_id = ? AND kind = ?`,
		p.ClusterID, p.Kind).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

func retentionCandidateDigest(ctx context.Context, tx *sql.Tx, p ReportRetentionPolicy, cutoff time.Time) (string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM (
			SELECT id, created_at, retention_finalized, ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS position_in_group
			FROM reports WHERE cluster_id = ? AND kind = ?
		) ranked_reports
		WHERE retention_finalized = 1 AND created_at < ? AND position_in_group > ?
		ORDER BY id`, p.ClusterID, p.Kind, cutoff, p.Minimum)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	digest := sha256.New()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		fmt.Fprintf(digest, "%d:%s", len(id), id)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), rows.Err()
}
