package kubernetes

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/k8s-ai/k8s-ai/internal/model"
)

// GetPodLogs fetches capped log bytes for one container (FR-022).
func (c *Client) GetPodLogs(ctx context.Context, namespace, pod, container string, opts model.LogOptions) ([]byte, error) {
	po := &corev1.PodLogOptions{
		Container:    opts.Container,
		TailLines:    opts.TailLines,
		SinceSeconds: opts.SinceSeconds,
		Previous:     opts.Previous,
	}
	raw, err := c.clientset.CoreV1().Pods(namespace).GetLogs(pod, po).Do(ctx).Raw()
	if err != nil {
		return nil, fmt.Errorf("get logs %s/%s/%s: %w", namespace, pod, container, err)
	}
	return model.TruncateLogs(raw, opts.MaxBytes, opts.MaxLineBytes), nil
}

// GetPodLogSample requests one byte beyond the presentation budget to detect a
// truncated sample while bounding the response at the source.
func (c *Client) GetPodLogSample(ctx context.Context, namespace, pod string, opts model.LogOptions) ([]byte, bool, error) {
	limit := int64(opts.MaxBytes) + 1
	query := &corev1.PodLogOptions{
		Container: opts.Container, TailLines: opts.TailLines, SinceSeconds: opts.SinceSeconds,
		Previous: opts.Previous, LimitBytes: &limit,
	}
	raw, err := c.clientset.CoreV1().Pods(namespace).GetLogs(pod, query).Do(ctx).Raw()
	if err != nil {
		return nil, false, err
	}
	capped := len(raw) > opts.MaxBytes
	sample := model.TruncateLogs(raw, opts.MaxBytes, opts.MaxLineBytes)
	if len(sample) > opts.MaxBytes {
		sample = sample[:opts.MaxBytes]
	}
	return sample, capped, nil
}
