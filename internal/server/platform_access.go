package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/service"
	"github.com/k8s-ai/k8s-ai/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

func (s *Server) WithPlatformAccess(p *service.PlatformAccess) *Server { s.platform = p; return s }
func (s *Server) handleAccess(w http.ResponseWriter, r *http.Request) {
	u := UserFrom(r.Context())
	if u == nil || s.platform == nil {
		writeError(w, r, 403, codeForbidden, "access unavailable")
		return
	}
	list, e := s.platform.Grants.AccessibleClusters(r.Context(), u.ID)
	if e != nil {
		writeError(w, r, 500, codeInternal, "access unavailable")
		return
	}
	writeOK(w, r, 200, list)
}
func scanCoverage(id, kind, value string, level int) authz.Coverage {
	c := authz.Coverage{ClusterID: id, Version: 1}
	if id == "" || level < 1 || level > 2 {
		return c
	}
	ns := value
	switch kind {
	case model.ScopeCluster:
		if value != "" || level != 2 {
			return c
		}
		ns = "*"
		c.Targets = append(c.Targets, authz.Target{ClusterID: id, ClusterScoped: true, Actions: []string{authz.ActionRead}})
	case model.ScopePod:
		parts := strings.Split(value, "/")
		if len(parts) != 2 || len(validation.IsDNS1123Subdomain(parts[1])) != 0 {
			return c
		}
		ns = parts[0]
	case model.ScopeNamespace:
	default:
		return c
	}
	if kind != model.ScopeCluster && len(validation.IsDNS1123Label(ns)) != 0 {
		return c
	}
	c.Targets = append(c.Targets, authz.Target{ClusterID: id, Namespace: ns, Actions: []string{authz.ActionRead}})
	if level == 2 {
		c.Targets = append(c.Targets, authz.Target{ClusterID: id, Namespace: ns, Actions: []string{authz.ActionLogs}})
	}
	return c
}
func (s *Server) bindScan(r *http.Request, u *store.User, opts *model.ScanOptions) (*service.BoundRun, error) {
	attr := &opts.Attribution
	a, e := s.platform.Grants.EffectiveAccess(r.Context(), u.ID, attr.ClusterID)
	if e != nil {
		return nil, e
	}
	level := 1
	if attr.ScopeKind == model.ScopeCluster || opts.CollectLogs && a.Check(authz.Target{ClusterID: attr.ClusterID, Namespace: opts.Namespace, Actions: []string{authz.ActionLogs}}) == nil {
		level = 2
	}
	c := scanCoverage(attr.ClusterID, attr.ScopeKind, attr.ScopeValue, level)
	token := BearerTokenFrom(r.Context())
	auth, ok := s.auth.(interface {
		RecheckToken(context.Context, string) (*store.User, *store.Role, error)
	})
	if !ok {
		return nil, errors.New("authentication unavailable")
	}
	bound, e := s.platform.Bind(r.Context(), u.ID, c, func(ctx context.Context) error {
		user, role, e := auth.RecheckToken(ctx, token)
		if e != nil || role == nil || user == nil || user.ID != u.ID {
			return errors.New("authentication expired")
		}
		return nil
	})
	if e != nil {
		return nil, e
	}
	opts.RequestCheck = bound.CheckRequest
	opts.Context = ""
	opts.Kubeconfig = bound.Path
	opts.InCluster = false
	attr.CoverageLevel = level
	attr.Recheck = func(ctx context.Context) error { _, e := bound.Access(ctx); return e }
	return bound, nil
}
