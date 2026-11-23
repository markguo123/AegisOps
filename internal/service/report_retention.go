package service

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func (p *ReportPlatform) PreviewReportRetention(ctx context.Context, clusterID, kind string, days, minimum int) (store.ReportRetentionPreview, error) {
	if _, err := p.st.Clusters().Get(ctx, clusterID); err != nil {
		return store.ReportRetentionPreview{}, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	return p.st.Reports().PreviewRetention(ctx, clusterID, kind, cutoff, minimum)
}

func (p *ReportPlatform) FinalizeReportRetentionScan(ctx context.Context, scanID, clusterID, ownerID string) error {
	return p.st.Reports().FinalizeRetentionScan(ctx, scanID, clusterID, ownerID)
}

var ErrRetentionPreviewRequired = errors.New("report retention preview has expired or changed")

func (p *ReportPlatform) ReportRetentionPolicies(ctx context.Context) ([]store.ReportRetentionPolicy, error) {
	return p.st.Reports().ReportRetentionPolicies(ctx)
}

func (p *ReportPlatform) SetReportRetentionPolicy(ctx context.Context, policy store.ReportRetentionPolicy, cutoff time.Time, eligible int64, actor string) error {
	now := time.Now().UTC()
	if policy.Enabled {
		if _, err := p.st.Clusters().Get(ctx, policy.ClusterID); err != nil {
			return err
		}
		expected := now.AddDate(0, 0, -policy.Days)
		if cutoff.Before(expected.Add(-10*time.Minute)) || cutoff.After(expected) {
			return ErrRetentionPreviewRequired
		}
		actual, err := p.st.Reports().PreviewRetention(ctx, policy.ClusterID, policy.Kind, cutoff, policy.Minimum)
		if err != nil {
			return err
		}
		if actual.Eligible != eligible {
			return ErrRetentionPreviewRequired
		}
	}
	policy.NextRun = now.Truncate(time.Millisecond)
	return p.st.Reports().SetReportRetentionPolicy(ctx, policy, actor)
}

func (p *ReportPlatform) RunReportRetentionScheduler(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		policies, err := p.ReportRetentionPolicies(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Warn("load report retention policies failed", "error", err)
		}
		for _, policy := range policies {
			now := time.Now().UTC()
			if !policy.Enabled || policy.NextRun.After(now) || ctx.Err() != nil {
				continue
			}
			runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_, err := p.st.Reports().RunReportRetention(runCtx, policy, now)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Warn("report retention batch failed", "cluster_id", policy.ClusterID, "kind", policy.Kind, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
