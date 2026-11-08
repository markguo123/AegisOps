package authz

import (
	"context"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

func TestResourceGrantsPreserveNamespaceAndClusterBoundaries(t *testing.T) {
	s, st, uid := newTestService(t)
	ctx := context.Background()
	for _, id := range []string{"prod", "stage"} {
		if err := st.Clusters().Create(ctx, &store.Cluster{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(cluster, ns string, actions ...string) *AccessRule {
		t.Helper()
		r, err := s.CreateResourceGrant(ctx, ResourceGrantRequest{UserID: uid, ClusterID: cluster, ScopeType: ScopeNamespace, Namespaces: []string{ns}, Actions: actions}, "admin")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	grant("prod", "orders", ActionRead, ActionLogs)
	grant("prod", "testing", ActionRead, ActionOperate)
	destroy := grant("prod", "orders", ActionDestroy)
	grant("stage", "orders", ActionRead, ActionOperate, ActionDestroy)
	for _, tc := range []struct {
		name, cluster, ns string
		clusterScoped     bool
		action            string
		allowed           bool
	}{
		{"read prod", "prod", "orders", false, ActionRead, true},
		{"logs prod", "prod", "orders", false, ActionLogs, true},
		{"prod read not write", "prod", "orders", false, ActionOperate, false},
		{"destroy needs operate in same namespace", "prod", "orders", false, ActionDestroy, false},
		{"destroy not leaked", "prod", "testing", false, ActionDestroy, false},
		{"operate testing", "prod", "testing", false, ActionOperate, true},
		{"explicit stage grant", "stage", "orders", false, ActionDestroy, true},
		{"no implicit node", "prod", "", true, ActionRead, false},
		{"no empty cluster", "", "orders", false, ActionRead, false},
		{"no implicit secret", "prod", "orders", false, ActionSecretRead, false},
		{"no exec", "prod", "testing", false, ActionExec, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Authorize(ctx, uid, Target{ClusterID: tc.cluster, Namespace: tc.ns, ClusterScoped: tc.clusterScoped, Actions: []string{tc.action}})
			if (err == nil) != tc.allowed {
				t.Fatalf("allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
	if err := s.DeleteGrant(ctx, destroy.GrantID); err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, uid, Target{ClusterID: "prod", Namespace: "orders", Actions: []string{ActionDestroy}}); err == nil {
		t.Fatal("revoked grant accepted")
	}
	_, err := s.CreateResourceGrant(ctx, ResourceGrantRequest{UserID: uid, ClusterID: "prod", ScopeType: ScopeCluster, Actions: []string{ActionRead}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, uid, Target{ClusterID: "prod", ClusterScoped: true, Actions: []string{ActionRead}}); err != nil {
		t.Fatal(err)
	}
}

func TestResourceAccessDoesNotTrustRoleUsernameOrLegacyGrant(t *testing.T) {
	s, st, uid := newTestService(t)
	ctx := context.Background()
	if err := st.Clusters().Create(ctx, &store.Cluster{ID: "prod", Name: "prod"}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []*store.AuthzGrant{
		{ID: "legacy", PrincipalType: "user", PrincipalID: uid, ClusterID: "prod", Namespaces: `["*"]`, Permissions: `["*"]`},
		{ID: "username", PrincipalType: "user", PrincipalID: "alice", ClusterID: "prod", ScopeType: ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: `["orders"]`, Permissions: `["read"]`},
		{ID: "group", PrincipalType: "group", PrincipalID: uid, ClusterID: "prod", ScopeType: ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: `["orders"]`, Permissions: `["read"]`},
	} {
		if err := st.AuthzGrants().Create(ctx, g); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Authorize(ctx, uid, Target{ClusterID: "prod", Namespace: "orders", Actions: []string{ActionRead}}); err == nil {
		t.Fatal("legacy or username/group granted access")
	}
	admin, err := st.Users().GetByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, admin.ID, Target{ClusterID: "prod", ClusterScoped: true, Actions: []string{ActionRead}}); err != nil {
		t.Fatalf("platform admin should access a registered cluster without grants: %v", err)
	}
	if err := s.Authorize(ctx, admin.ID, Target{ClusterID: "prod", Namespace: "orders", Actions: []string{ActionDestroy}}); err != nil {
		t.Fatalf("platform admin should operate without self-grants: %v", err)
	}
	access, err := s.EffectiveAccess(ctx, admin.ID, "prod")
	if err != nil {
		t.Fatal(err)
	}
	limited := access.Coverage().Limit(access)
	for _, action := range []string{ActionExec, ActionSecretRead} {
		if err := limited.Check(Target{ClusterID: "prod", Namespace: "orders", Actions: []string{action}}); err != nil {
			t.Fatalf("new admin conversation should retain %s: %v", action, err)
		}
	}
	if err := s.Authorize(ctx, admin.ID, Target{ClusterID: "prod", ClusterScoped: true, Actions: []string{ActionShell}}); err != nil {
		t.Fatalf("platform admin should have full registered-cluster scope: %v", err)
	}
	available, err := s.AccessibleClusters(ctx, admin.ID)
	if err != nil || len(available) != 1 || available[0].ID != "prod" {
		t.Fatalf("admin should see registered cluster without grants: clusters=%v err=%v", available, err)
	}
	if err := s.Authorize(ctx, admin.ID, Target{ClusterID: "missing", Namespace: "orders", Actions: []string{ActionRead}}); err == nil {
		t.Fatal("admin must still select a registered cluster")
	}
}

func TestResourceGrantValidationAndLiveRevocation(t *testing.T) {
	s, st, uid := newTestService(t)
	ctx := context.Background()
	if err := st.Clusters().Create(ctx, &store.Cluster{ID: "prod", Name: "prod"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []ResourceGrantRequest{
		{UserID: uid, ScopeType: ScopeNamespace, Namespaces: []string{"orders"}, Actions: []string{ActionRead}},
		{UserID: "alice", ClusterID: "prod", ScopeType: ScopeNamespace, Namespaces: []string{"orders"}, Actions: []string{ActionRead}},
		{UserID: uid, ClusterID: "missing", ScopeType: ScopeNamespace, Namespaces: []string{"orders"}, Actions: []string{ActionRead}},
		{UserID: uid, ClusterID: "prod", ScopeType: ScopeNamespace, Namespaces: []string{"*", "orders"}, Actions: []string{ActionRead}},
		{UserID: uid, ClusterID: "prod", ScopeType: ScopeNamespace, Namespaces: []string{"order*"}, Actions: []string{ActionRead}},
		{UserID: uid, ClusterID: "prod", ScopeType: ScopeCluster, Namespaces: []string{"orders"}, Actions: []string{ActionRead}},
		{UserID: uid, ClusterID: "prod", ScopeType: ScopeNamespace, Namespaces: []string{"orders"}, Actions: []string{"*"}},
	} {
		if _, err := s.CreateResourceGrant(ctx, req, "admin"); err == nil {
			t.Fatalf("invalid grant accepted: %+v", req)
		}
	}
	r, err := s.CreateResourceGrant(ctx, ResourceGrantRequest{UserID: uid, ClusterID: "prod", ScopeType: ScopeNamespace, Namespaces: []string{"orders"}, Actions: []string{ActionRead}}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	target := Target{ClusterID: "prod", Namespace: "orders", Actions: []string{ActionRead}}
	if err := s.Authorize(ctx, uid, target); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.AuthzGrants().Get(ctx, r.GrantID)
	past := time.Now().Add(-time.Minute)
	raw.ExpiresAt = &past
	if err := st.AuthzGrants().Update(ctx, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, uid, target); err == nil {
		t.Fatal("expired grant accepted")
	}
	raw.ExpiresAt = nil
	if err := st.AuthzGrants().Update(ctx, raw); err != nil {
		t.Fatal(err)
	}
	u, _ := st.Users().Get(ctx, uid)
	u.Status = "disabled"
	if err := st.Users().Update(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.Authorize(ctx, uid, target); err == nil {
		t.Fatal("disabled user accepted")
	}
}
