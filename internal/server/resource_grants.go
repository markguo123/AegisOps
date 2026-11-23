package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

func (s *Server) auditPolicy(w http.ResponseWriter, r *http.Request, action string) bool {
	u := UserFrom(r.Context())
	if u == nil || s.auditlog == nil || s.auditlog.Record(r.Context(), store.AuditEvent{UserID: u.ID, Action: action, Timestamp: time.Now().UTC(), RequestID: RequestIDFrom(r.Context()), Status: "requested", EventType: store.EventTypeForAction(action), ResourceName: r.URL.Path, Operation: r.Method}) != nil {
		writeError(w, r, 503, codeUnavailable, "audit unavailable")
		return false
	}
	return true
}
func (s *Server) savePolicy(w http.ResponseWriter, r *http.Request, edit bool) {
	ctx := r.Context()
	var req authz.ResourceGrantRequest
	if edit {
		g, e := s.platform.Grants.GetGrant(ctx, r.PathValue("id"))
		if e != nil {
			writeAuthzError(w, r, e)
			return
		}
		req = authz.ResourceGrantRequest{UserID: g.UserID, ClusterID: g.ClusterID, ScopeType: g.ScopeType, Namespaces: g.Namespaces, Actions: g.Actions, ExpiresAt: g.ExpiresAt}
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAuthzBody))
	dec.DisallowUnknownFields()
	if dec.Decode(&req) != nil {
		writeError(w, r, 400, codeInvalidArgument, "invalid resource grant")
		return
	}
	payload, _ := json.Marshal(req)
	if s.auditlog == nil || s.auditlog.Record(ctx, store.AuditEvent{UserID: UserFrom(ctx).ID, ClusterID: req.ClusterID, Action: "grant.save", EventType: store.EventTypeForAction("grant.save"), ResourceKind: "AuthzGrant", ResourceName: r.PathValue("id"), Payload: string(payload), Status: "pending"}) != nil {
		writeError(w, r, 503, codeUnavailable, "audit unavailable")
		return
	}
	if edit {
		g, e := s.platform.Grants.ReplaceResourceGrant(ctx, r.PathValue("id"), req)
		if e != nil {
			writeAuthzError(w, r, e)
			return
		}
		writeOK(w, r, 200, g)
		return
	}
	rule, e := s.platform.Grants.CreateResourceGrant(ctx, req, UserFrom(ctx).ID)
	if e != nil {
		writeAuthzError(w, r, e)
		return
	}
	g, e := s.platform.Grants.GetGrant(ctx, rule.GrantID)
	if e != nil {
		writeAuthzError(w, r, e)
		return
	}
	writeOK(w, r, 201, g)
}
