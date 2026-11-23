package service

import (
	"context"
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
