package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func (p *ReportPlatform) PreviewReportRetention(ctx context.Context, policy store.ReportRetentionPolicy, actor string) (store.ReportRetentionPreview, error) {
	if _, err := p.st.Clusters().Get(ctx, policy.ClusterID); err != nil {
		return store.ReportRetentionPreview{}, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -policy.Days)
	return p.st.Reports().IssueRetentionPreview(ctx, policy, actor, cutoff)
}

func (p *ReportPlatform) FinalizeReportRetentionScan(ctx context.Context, scanID, clusterID, ownerID string) error {
	return p.st.Reports().FinalizeRetentionScan(ctx, scanID, clusterID, ownerID)
}

func (p *ReportPlatform) ReportRetentionPolicies(ctx context.Context) ([]store.ReportRetentionPolicy, error) {
	return p.st.Reports().ReportRetentionPolicies(ctx)
}

func (p *ReportPlatform) SetReportRetentionPolicy(ctx context.Context, policy store.ReportRetentionPolicy, token, actor string) error {
	policy.NextRun = time.Now().UTC().Truncate(time.Millisecond)
	if policy.Enabled {
		if _, err := p.st.Clusters().Get(ctx, policy.ClusterID); err != nil {
			return err
		}
		return p.st.Reports().ActivateReportRetention(ctx, policy, token, actor)
	}
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
				auditCtx, finish := context.WithTimeout(ctx, 3*time.Second)
				_ = p.st.Audit().Write(auditCtx, &store.AuditEvent{
					ClusterID: policy.ClusterID, Action: "report.retention.run", EventType: store.AuditEventReport,
					ResourceKind: "ReportRetentionPolicy", ResourceName: policy.Kind,
					Status: "error", ResultSummary: "Cleanup transaction failed and will be retried",
				})
				finish()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
