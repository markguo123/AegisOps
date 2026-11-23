package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type retentionRun struct {
	id         string
	generation int64
	position   int
	candidates [][2]string
	cutoff     time.Time
}

type retentionRunPayload struct {
	Candidates [][2]string
	Cutoff     time.Time
}

func retentionRunID(p ReportRetentionPolicy) string {
	return retentionTokenHash(fmt.Sprintf("%s:%s:%d:%d", p.ClusterID, p.Kind, p.Version, p.NextRun.UnixMilli()))
}

// RunReportRetention resumes an abandoned batch or starts the due batch.
// Each item and its progress commit together while the policy and lease are valid.
func (st *reportStore) RunReportRetention(ctx context.Context, p ReportRetentionPolicy, _ time.Time) (int, error) {
	run, err := st.claimRetentionRun(ctx, p)
	if err != nil || run == nil {
		return 0, err
	}
	deleted := 0
	for run.position < len(run.candidates) {
		removed, advanced, err := st.runRetentionItem(ctx, p, run, false)
		if err != nil && ctx.Err() == nil {
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return deleted, ctx.Err()
			case <-timer.C:
			}
			removed, advanced, err = st.runRetentionItem(ctx, p, run, false)
			if err != nil && ctx.Err() == nil {
				_, advanced, err = st.runRetentionItem(ctx, p, run, true)
			}
		}
		if err != nil || !advanced {
			return deleted, err
		}
		if removed {
			deleted++
		}
		run.position++
	}
	return deleted, st.finishRetentionRun(ctx, p, run)
}

func (st *reportStore) claimRetentionRun(ctx context.Context, p ReportRetentionPolicy) (*retentionRun, error) {
	run := &retentionRun{id: retentionRunID(p)}
	claimed := false
	err := st.s.WithTx(ctx, func(tx *Tx) error {
		valid, err := lockCurrentRetentionPolicy(ctx, tx, p)
		if err != nil || !valid {
			return err
		}
		now, err := retentionDatabaseTime(ctx, tx)
		if err != nil || p.NextRun.After(now) {
			return err
		}
		var lease time.Time
		var status string
		var payload []byte
		err = tx.tx.QueryRowContext(ctx, `
			SELECT generation, lease_until, payload, position, status
			FROM report_retention_runs WHERE id = ?`, run.id).
			Scan(&run.generation, &lease, &payload, &run.position, &status)
		leaseUntil := now.Add(time.Minute)
		switch {
		case err == nil:
			if status != "running" || lease.After(now) {
				return nil
			}
			var batch retentionRunPayload
			if err := json.Unmarshal(payload, &batch); err != nil {
				return err
			}
			run.candidates, run.cutoff = batch.Candidates, batch.Cutoff
			run.generation++
			_, err = tx.tx.ExecContext(ctx, `UPDATE report_retention_runs SET generation = ?, lease_until = ? WHERE id = ?`,
				run.generation, leaseUntil, run.id)
		case errors.Is(err, sql.ErrNoRows):
			run.cutoff = now.AddDate(0, 0, -p.Days)
			run.candidates, err = selectRetentionCandidates(ctx, tx, p, run.cutoff)
			if err != nil {
				return err
			}
			run.generation = 1
			payload, err = json.Marshal(retentionRunPayload{Candidates: run.candidates, Cutoff: run.cutoff})
			if err != nil {
				return err
			}
			_, err = tx.tx.ExecContext(ctx, `
				INSERT INTO report_retention_runs (id, generation, lease_until, payload) VALUES (?, ?, ?, ?)`,
				run.id, run.generation, leaseUntil, string(payload))
		default:
			return err
		}
		claimed = err == nil
		return err
	})
	if err != nil || !claimed {
		return nil, err
	}
	return run, nil
}

func lockCurrentRetentionPolicy(ctx context.Context, tx *Tx, p ReportRetentionPolicy) (bool, error) {
	if err := tx.LockReportRetentionPolicy(ctx, p.ClusterID, p.Kind); err != nil {
		return false, err
	}
	var current int
	err := tx.tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM report_retention_policies
		WHERE cluster_id = ? AND kind = ? AND enabled = 1 AND version = ? AND next_run = ?
			AND EXISTS (SELECT 1 FROM clusters WHERE id = ?)`,
		p.ClusterID, p.Kind, p.Version, p.NextRun, p.ClusterID).Scan(&current)
	return current == 1, err
}

func selectRetentionCandidates(ctx context.Context, tx *Tx, p ReportRetentionPolicy, cutoff time.Time) ([][2]string, error) {
	rows, err := tx.tx.QueryContext(ctx, `
		SELECT id, blob_key FROM (
			SELECT id, blob_key, created_at, retention_finalized,
			       ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS position_in_group
			FROM reports WHERE cluster_id = ? AND kind = ?
		) ranked_reports
		WHERE retention_finalized = 1 AND created_at < ? AND position_in_group > ?
		ORDER BY created_at, id LIMIT 200`,
		p.ClusterID, p.Kind, cutoff, p.Minimum)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var candidates [][2]string
	for rows.Next() {
		var candidate [2]string
		if err := rows.Scan(&candidate[0], &candidate[1]); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

func lockRetentionRun(ctx context.Context, tx *Tx, p ReportRetentionPolicy, run *retentionRun) (bool, error) {
	valid, err := lockCurrentRetentionPolicy(ctx, tx, p)
	if err != nil || !valid {
		return false, err
	}
	now, err := retentionDatabaseTime(ctx, tx)
	if err != nil {
		return false, err
	}
	_, err = tx.tx.ExecContext(ctx, `
		UPDATE report_retention_runs SET lease_until = ?
		WHERE id = ? AND generation = ? AND lease_until >= ? AND position = ? AND status = 'running'`,
		now.Add(time.Minute).Truncate(time.Millisecond), run.id, run.generation, now, run.position)
	if err != nil {
		return false, err
	}
	// Verify the fence even when a lease renewal shares a millisecond.
	var count int
	err = tx.tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM report_retention_runs
		WHERE id = ? AND generation = ? AND lease_until >= ? AND position = ? AND status = 'running'`,
		run.id, run.generation, now, run.position).Scan(&count)
	return count == 1, err
}

func (st *reportStore) runRetentionItem(ctx context.Context, p ReportRetentionPolicy, run *retentionRun, failed bool) (bool, bool, error) {
	var deleted, advanced bool
	err := st.s.WithTx(ctx, func(tx *Tx) error {
		valid, err := lockRetentionRun(ctx, tx, p, run)
		if err != nil || !valid {
			return err
		}
		candidate := run.candidates[run.position]
		status, result := "success", "report reference released"
		if failed {
			status, result = "error", "item deletion failed and will be reconsidered in the next batch"
		} else {
			var eligible int
			err := tx.tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM reports AS candidate
				WHERE candidate.id = ? AND candidate.blob_key = ? AND candidate.cluster_id = ? AND candidate.kind = ?
				AND candidate.retention_finalized = 1 AND candidate.created_at < ? AND (
					SELECT COUNT(*) FROM reports AS newer
					WHERE newer.cluster_id = candidate.cluster_id AND newer.kind = candidate.kind
					AND (newer.created_at > candidate.created_at OR
					     (newer.created_at = candidate.created_at AND newer.id > candidate.id))
				) >= ?`, candidate[0], candidate[1], p.ClusterID, p.Kind, run.cutoff, p.Minimum).Scan(&eligible)
			if err != nil {
				return err
			}
			if eligible != 0 {
				removed, missing, err := tx.DeleteReportReference(ctx, candidate[0], candidate[1])
				if err != nil {
					return err
				}
				deleted = removed
				if missing {
					result = "object reference already missing"
				}
			}
			if !deleted {
				status, result = "not_executed", "report no longer eligible"
			}
		}
		if err := writeAuditEvent(ctx, tx.tx, &AuditEvent{
			RequestID: run.id, ClusterID: p.ClusterID, Action: "report.retention.delete", EventType: AuditEventReport,
			ResourceKind: "Report", ResourceName: candidate[0], Status: status, ResultSummary: result,
		}); err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, `
			UPDATE report_retention_runs SET position = position + 1, deleted = deleted + ?, failed = failed + ? WHERE id = ?`,
			boolInt(deleted), boolInt(failed), run.id)
		advanced = err == nil
		return err
	})
	return deleted && err == nil, advanced && err == nil, err
}

func (st *reportStore) finishRetentionRun(ctx context.Context, p ReportRetentionPolicy, run *retentionRun) error {
	return st.s.WithTx(ctx, func(tx *Tx) error {
		valid, err := lockRetentionRun(ctx, tx, p, run)
		if err != nil || !valid {
			return err
		}
		var deleted, failed int
		if err := tx.tx.QueryRowContext(ctx, `SELECT deleted, failed FROM report_retention_runs WHERE id = ?`, run.id).Scan(&deleted, &failed); err != nil {
			return err
		}
		now, err := retentionDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		next := now.Add(time.Duration(p.IntervalMinutes) * time.Minute)
		status := "success"
		if failed > 0 {
			status = "error"
		}
		if _, err := tx.tx.ExecContext(ctx, `UPDATE report_retention_runs SET status = ? WHERE id = ?`, status, run.id); err != nil {
			return err
		}
		_, err = tx.tx.ExecContext(ctx, `
			UPDATE report_retention_policies SET last_run = ?, last_deleted = ?, next_run = ?
			WHERE cluster_id = ? AND kind = ?`, now, deleted, next, p.ClusterID, p.Kind)
		if err != nil {
			return err
		}
		return writeAuditEvent(ctx, tx.tx, &AuditEvent{
			RequestID: run.id, ClusterID: p.ClusterID, Action: "report.retention.run", EventType: AuditEventReport,
			ResourceKind: "ReportRetentionPolicy", ResourceName: p.Kind, Status: status,
			ResultSummary: fmt.Sprintf("deleted=%d skipped=%d failed=%d", deleted, len(run.candidates)-deleted-failed, failed),
		})
	})
}

func retentionDatabaseTime(ctx context.Context, tx *Tx) (time.Time, error) {
	query := `SELECT CAST(strftime('%s', 'now') AS INTEGER) * 1000 + CAST(substr(strftime('%f', 'now'), 4, 3) AS INTEGER)`
	if _, mysql := tx.dialect.(mysqlDialect); mysql {
		query = `SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000 AS SIGNED)`
	}
	var milliseconds int64
	err := tx.tx.QueryRowContext(ctx, query).Scan(&milliseconds)
	return time.UnixMilli(milliseconds).UTC(), err
}

func stopRetentionRun(ctx context.Context, tx *Tx, p ReportRetentionPolicy, actor string) error {
	if err := tx.LockReportRetentionPolicy(ctx, p.ClusterID, p.Kind); err != nil {
		return err
	}
	err := tx.tx.QueryRowContext(ctx, `
		SELECT version, next_run FROM report_retention_policies WHERE cluster_id = ? AND kind = ? AND enabled = 1`,
		p.ClusterID, p.Kind).Scan(&p.Version, &p.NextRun)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	id := retentionRunID(p)
	result, err := tx.tx.ExecContext(ctx, `
		UPDATE report_retention_runs SET status = 'stopped' WHERE id = ? AND status = 'running'`, id)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	return writeAuditEvent(ctx, tx.tx, &AuditEvent{
		RequestID: id, UserID: actor, ClusterID: p.ClusterID, Action: "report.retention.run", EventType: AuditEventReport,
		ResourceKind: "ReportRetentionPolicy", ResourceName: p.Kind, Status: "not_executed",
		ResultSummary: "remaining items stopped by a policy change",
	})
}
