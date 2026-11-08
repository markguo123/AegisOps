package authz

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/util/validation"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

const (
	ScopeNamespace   = "namespace"
	ScopeCluster     = "cluster"
	ActionRead       = "read"
	ActionLogs       = "logs"
	ActionOperate    = "operate"
	ActionDestroy    = "destroy"
	ActionExec       = "exec"
	ActionSecretRead = "secret.read"
	ActionShell      = "shell"
)

// AccessRule keeps actions attached to their resource boundary. Never flatten
// actions from different namespaces into a global write/destroy flag.
type AccessRule struct {
	ScopeType       string   `json:"scopeType"`
	Namespaces      []string `json:"namespaces"`
	ResourceProfile string   `json:"resourceProfile"`
	Actions         []string `json:"actions"`
	GrantID         string   `json:"grantId"`
}

type Target struct {
	Name          string `json:"name,omitempty"`
	Resource      string `json:"resource,omitempty"`
	Subresource   string `json:"subresource,omitempty"`
	ClusterID     string
	Namespace     string
	ClusterScoped bool
	Actions       []string
}

// EffectiveAccess is an immutable snapshot for presentation. Execution must
// call Authorize again, which reads the authoritative user and grants.
type EffectiveAccess struct {
	Observe   func(Target) `json:"-"`
	ClusterID string       `json:"clusterId"`
	Rules     []AccessRule `json:"rules"`
}

func (a EffectiveAccess) Check(t Target) error {
	if len(t.Actions) == 0 {
		return fmt.Errorf("ACTION_DENIED: action is required")
	}
	if a.ClusterID == "" || t.ClusterID == "" || a.ClusterID != t.ClusterID {
		return fmt.Errorf("SCOPE_DENIED: explicit matching cluster is required")
	}
	actions := map[string]bool{}
	for _, r := range a.Rules {
		if !validAccessRule(r) {
			continue
		}
		if t.ClusterScoped != (r.ScopeType == ScopeCluster) {
			continue
		}
		if !t.ClusterScoped && !containsString(r.Namespaces, t.Namespace) && !containsString(r.Namespaces, "*") {
			continue
		}
		if !t.ClusterScoped && t.Namespace == "" {
			continue
		}
		for _, action := range r.Actions {
			actions[action] = true
		}
	}
	for _, action := range requiredActions(t.Actions) {
		if !actions[action] {
			return fmt.Errorf("ACTION_DENIED: missing %s in requested resource scope", action)
		}
	}
	if containsString(t.Actions, ActionShell) {
		if !t.ClusterScoped {
			return fmt.Errorf("ACTION_DENIED: shell requires cluster scope")
		}
		all := map[string]bool{}
		for _, r := range a.Rules {
			if validAccessRule(r) && r.ScopeType == ScopeNamespace && containsString(r.Namespaces, "*") {
				for _, action := range r.Actions {
					all[action] = true
				}
			}
		}
		for _, action := range []string{ActionRead, ActionLogs, ActionOperate, ActionDestroy} {
			if !all[action] {
				return fmt.Errorf("ACTION_DENIED: shell requires all-namespace %s", action)
			}
		}
	}
	if a.Observe != nil {
		a.Observe(t)
	}
	return nil
}

func requiredActions(actions []string) []string {
	out := append([]string{}, actions...)
	for _, action := range actions {
		switch action {
		case ActionLogs, ActionSecretRead:
			out = append(out, ActionRead)
		case ActionOperate:
			out = append(out, ActionRead)
		case ActionDestroy, ActionExec:
			out = append(out, ActionRead, ActionOperate)
		case ActionShell:
			out = append(out, ActionRead, ActionOperate, ActionDestroy)
		}
	}
	return out
}

func (s *Service) EffectiveAccess(ctx context.Context, userID, clusterID string) (*EffectiveAccess, error) {
	if strings.TrimSpace(clusterID) == "" {
		return nil, fmt.Errorf("%w: clusterId is required", ErrInvalid)
	}
	u, err := s.st.Users().Get(ctx, userID)
	if err != nil {
		return nil, err
	}
	if u.Status != "active" {
		return nil, fmt.Errorf("%w: user is not active", ErrInvalid)
	}
	if _, err := s.st.Clusters().Get(ctx, clusterID); err != nil {
		return nil, fmt.Errorf("%w: cluster not found", ErrInvalid)
	}
	role, err := s.st.Roles().Get(ctx, u.RoleID)
	if err != nil {
		return nil, err
	}
	if role.Name == "admin" {
		return &EffectiveAccess{ClusterID: clusterID, Rules: []AccessRule{
			{ScopeType: ScopeNamespace, Namespaces: []string{"*"}, ResourceProfile: "builtin_namespaced", Actions: []string{ActionRead, ActionLogs, ActionOperate, ActionDestroy, ActionExec, ActionSecretRead}},
			{ScopeType: ScopeCluster, ResourceProfile: "builtin_cluster", Actions: []string{ActionRead, ActionOperate, ActionDestroy, ActionShell}},
		}}, nil
	}
	grants, err := s.st.AuthzGrants().ListByPrincipal(ctx, "user", userID)
	if err != nil {
		return nil, err
	}
	a := &EffectiveAccess{ClusterID: clusterID, Rules: []AccessRule{}}
	for _, g := range grants {
		if g.ClusterID != clusterID || (g.ExpiresAt != nil && !g.ExpiresAt.After(time.Now())) {
			continue
		}
		var ns, actions []string
		if json.Unmarshal([]byte(g.Namespaces), &ns) != nil || json.Unmarshal([]byte(g.Permissions), &actions) != nil {
			return nil, fmt.Errorf("invalid grant data")
		}
		// Pre-refactor grants are intentionally inert; never interpret legacy '*'.
		if g.ScopeType != ScopeNamespace && g.ScopeType != ScopeCluster {
			continue
		}
		if containsString(actions, "*") {
			continue
		}
		rule := AccessRule{ScopeType: g.ScopeType, Namespaces: ns, ResourceProfile: g.ResourceProfile, Actions: actions, GrantID: g.ID}
		if !validAccessRule(rule) {
			return nil, fmt.Errorf("invalid resource grant %s", g.ID)
		}
		a.Rules = append(a.Rules, rule)
	}
	return a, nil
}

func validAccessRule(r AccessRule) bool {
	if len(r.Actions) == 0 {
		return false
	}
	switch r.ScopeType {
	case ScopeNamespace:
		if r.ResourceProfile != "builtin_namespaced" || len(r.Namespaces) == 0 {
			return false
		}
	case ScopeCluster:
		if r.ResourceProfile != "builtin_cluster" || len(r.Namespaces) != 0 {
			return false
		}
	default:
		return false
	}
	for _, ns := range r.Namespaces {
		if ns == "*" {
			if len(r.Namespaces) != 1 {
				return false
			}
			continue
		}
		if len(validation.IsDNS1123Label(ns)) > 0 {
			return false
		}
	}
	for _, action := range r.Actions {
		switch action {
		case ActionRead, ActionOperate, ActionDestroy:
		case ActionLogs, ActionExec, ActionSecretRead:
			if r.ScopeType != ScopeNamespace {
				return false
			}
		case ActionShell:
			if r.ScopeType != ScopeCluster {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// ResourceGrantRequest is the new explicit resource-policy contract. The
// legacy Grant API is not silently translated into this stronger model.
type ResourceGrantRequest struct {
	UserID     string     `json:"userId"`
	ClusterID  string     `json:"clusterId"`
	ScopeType  string     `json:"scopeType"`
	Namespaces []string   `json:"namespaces"`
	Actions    []string   `json:"actions"`
	ExpiresAt  *time.Time `json:"expiresAt"`
}

func (s *Service) CreateResourceGrant(ctx context.Context, req ResourceGrantRequest, actorID string) (*AccessRule, error) {
	u, err := s.st.Users().Get(ctx, req.UserID)
	if err != nil || u.Status != "active" {
		return nil, fmt.Errorf("%w: active internal user is required", ErrInvalid)
	}
	if req.ClusterID == "" {
		return nil, fmt.Errorf("%w: clusterId is required", ErrInvalid)
	}
	if _, err := s.st.Clusters().Get(ctx, req.ClusterID); err != nil {
		return nil, fmt.Errorf("%w: registered cluster is required", ErrInvalid)
	}
	profile := "builtin_namespaced"
	if req.ScopeType == ScopeCluster {
		profile = "builtin_cluster"
	}
	rule := AccessRule{ScopeType: req.ScopeType, Namespaces: req.Namespaces, Actions: req.Actions, ResourceProfile: profile, GrantID: newID()}
	if !validAccessRule(rule) {
		return nil, fmt.Errorf("%w: invalid resource scope or action", ErrInvalid)
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("%w: expiresAt must be in the future", ErrInvalid)
	}
	if err := s.checkShellGrant(ctx, req, rule, ""); err != nil {
		return nil, err
	}
	// Prerequisites are checked at execution against the union at one target;
	// a separate read grant in the same boundary may satisfy them.
	ns, _ := json.Marshal(req.Namespaces)
	if req.Namespaces == nil {
		ns = []byte("[]")
	}
	actions, _ := json.Marshal(req.Actions)
	g := &store.AuthzGrant{ID: rule.GrantID, PrincipalType: "user", PrincipalID: req.UserID, ClusterID: req.ClusterID, ScopeType: req.ScopeType, ResourceProfile: profile, Namespaces: string(ns), Permissions: string(actions), ExpiresAt: req.ExpiresAt, CreatedBy: actorID}
	if err := s.st.AuthzGrants().Create(ctx, g); err != nil {
		return nil, err
	}
	return &rule, nil
}

// ReplaceResourceGrant replaces an entire policy atomically; the owner and cluster never change.
func (s *Service) ReplaceResourceGrant(ctx context.Context, id string, req ResourceGrantRequest) (*Grant, error) {
	g, err := s.st.AuthzGrants().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("%w: grant unavailable", ErrGrantNotFound)
	}
	if g.ScopeType == "" || g.PrincipalType != "user" || g.PrincipalID != req.UserID || g.ClusterID != req.ClusterID {
		return nil, fmt.Errorf("%w: immutable grant subject or cluster", ErrInvalid)
	}
	profile := "builtin_namespaced"
	if req.ScopeType == ScopeCluster {
		profile = "builtin_cluster"
	}
	if !validAccessRule(AccessRule{ScopeType: req.ScopeType, ResourceProfile: profile, Namespaces: req.Namespaces, Actions: req.Actions}) || req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("%w: invalid rule or expiry", ErrInvalid)
	}
	if err := s.checkShellGrant(ctx, req, AccessRule{ScopeType: req.ScopeType, ResourceProfile: profile, Namespaces: req.Namespaces, Actions: req.Actions}, id); err != nil {
		return nil, err
	}
	if _, err := s.EffectiveAccess(ctx, req.UserID, req.ClusterID); err != nil {
		return nil, err
	}
	if req.Namespaces == nil {
		req.Namespaces = []string{}
	}
	ns, _ := json.Marshal(req.Namespaces)
	actions, _ := json.Marshal(req.Actions)
	g.ScopeType = req.ScopeType
	g.ResourceProfile = profile
	g.Namespaces = string(ns)
	g.Permissions = string(actions)
	g.ExpiresAt = req.ExpiresAt
	if err := s.st.AuthzGrants().Update(ctx, g); err != nil {
		return nil, err
	}
	return s.GetGrant(ctx, id)
}
func (s *Service) Authorize(ctx context.Context, userID string, target Target) error {
	if len(target.Actions) == 0 {
		return fmt.Errorf("%w: action is required", ErrInvalid)
	}
	a, err := s.EffectiveAccess(ctx, userID, target.ClusterID)
	if err != nil {
		return err
	}
	return a.Check(target)
}

func (s *Service) checkShellGrant(ctx context.Context, req ResourceGrantRequest, rule AccessRule, replacedID string) error {
	if !containsString(rule.Actions, ActionShell) {
		return nil
	}
	a, err := s.EffectiveAccess(ctx, req.UserID, req.ClusterID)
	if err != nil {
		return err
	}
	kept := []AccessRule{rule}
	for _, r := range a.Rules {
		if r.GrantID != replacedID {
			kept = append(kept, r)
		}
	}
	a.Rules = kept
	if err := a.Check(Target{ClusterID: req.ClusterID, ClusterScoped: true, Actions: []string{ActionShell}}); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}
