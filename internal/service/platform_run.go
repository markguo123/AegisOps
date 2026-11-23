package service

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/k8sgateway"
)

type PlatformAccess struct {
	Grants   *authz.Service
	Clusters *cluster.Service
	Observe  func(context.Context, string, string, k8sgateway.RequestRecord) error
}
type BoundRun struct {
	GatewayAuthorizer k8sgateway.RequestAuthorizer
	CoverageSnapshot  func() []byte
	CredentialVersion string
	Path              string
	Close             func()
	Access            func(context.Context) (*authz.EffectiveAccess, error)
}

// Watch cancels idle runs, including runs waiting for approval or model output, on revocation.
func (b BoundRun) Watch(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		changed := authz.AuthorizationChanges()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := b.Access(ctx); err != nil {
				cancel()
				return
			}
		case <-changed:
			if _, err := b.Access(ctx); err != nil {
				cancel()
				return
			}
		}
	}
}
func (b BoundRun) CheckRequest(r *http.Request) error {
	if b.GatewayAuthorizer != nil {
		return b.GatewayAuthorizer.AuthorizeRequest(r)
	}
	a, err := b.Access(r.Context())
	if err != nil {
		return err
	}
	return a.CheckRequest(r)
}
func (p PlatformAccess) Bind(ctx context.Context, uid string, c authz.Coverage, active func(context.Context) error) (*BoundRun, error) {
	if active == nil || p.Grants == nil || p.Clusters == nil {
		return nil, errors.New("target unavailable")
	}
	recorder := authz.NewCoverageRecorder(c)
	var cacheMu sync.Mutex
	var cached *authz.EffectiveAccess
	var cachedRevision uint64
	var refreshAt time.Time
	current := func(ctx context.Context) (*authz.EffectiveAccess, error) {
		if err := active(ctx); err != nil {
			return nil, err
		}
		cacheMu.Lock()
		defer cacheMu.Unlock()
		revision := authz.AuthorizationRevision()
		if cached == nil || cachedRevision != revision || !time.Now().Before(refreshAt) {
			a, err := p.Grants.EffectiveAccess(ctx, uid, c.ClusterID)
			if err != nil {
				return nil, err
			}
			if authz.AuthorizationRevision() != revision {
				return nil, errors.New("SCOPE_DENIED: authorization changed during refresh")
			}
			cached = a
			cachedRevision = revision
			refreshAt = time.Now().Add(30 * time.Second)
		}
		a := cached
		if !c.CoveredBy(a) {
			return nil, errors.New("SCOPE_DENIED: access to this data has changed")
		}
		limited := c.Limit(a)
		limited.Observe = recorder.Observe
		return limited, nil
	}
	if _, err := current(ctx); err != nil {
		return nil, err
	}
	data, version, err := p.Clusters.ExecutionConfig(ctx, c.ClusterID)
	if err != nil {
		return nil, err
	}
	path, close, err := executor.TemporaryKubeconfig(data)
	clear(data)
	if err != nil {
		return nil, err
	}
	run := &BoundRun{CoverageSnapshot: recorder.JSON, CredentialVersion: version, Path: path, Close: close, Access: func(ctx context.Context) (*authz.EffectiveAccess, error) {
		v, err := p.Clusters.ExecutionVersion(ctx, c.ClusterID)
		if err != nil || v != version {
			return nil, errors.New("SCOPE_DENIED: target credentials changed")
		}
		return current(ctx)
	}}
	run.GatewayAuthorizer = &k8sgateway.AccessContext{Current: run.Access}
	if p.Observe != nil {
		run.GatewayAuthorizer = &k8sgateway.AccessContext{Current: run.Access, Observe: func(ctx context.Context, record k8sgateway.RequestRecord) error {
			return p.Observe(ctx, uid, c.ClusterID, record)
		}}
	}
	return run, nil
}
