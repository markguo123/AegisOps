package store

import (
	"context"
	"time"
)

// ReportRetentionPreview counts reports by the age and minimum-copy policy.
// Potential contains metadata-only candidates pending lifecycle verification.
type ReportRetentionPreview struct {
	Total     int64     `json:"total"`
	Protected int64     `json:"protected"`
	Potential int64     `json:"potential"`
	Cutoff    time.Time `json:"cutoff"`
}

func (st *reportStore) PreviewRetention(ctx context.Context, clusterID, kind string, cutoff time.Time, minimum int) (ReportRetentionPreview, error) {
	result := ReportRetentionPreview{Cutoff: cutoff}
	err := st.s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN created_at < ? AND position_in_group > ? THEN 1 ELSE 0 END), 0)
		FROM (
			SELECT created_at, ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS position_in_group
			FROM reports WHERE cluster_id = ? AND kind = ?
		) ranked_reports`, cutoff, minimum, clusterID, kind).Scan(&result.Total, &result.Potential)
	if err != nil {
		return ReportRetentionPreview{}, mapErr(err, "preview report retention")
	}
	result.Protected = result.Total - result.Potential
	return result, nil
}
