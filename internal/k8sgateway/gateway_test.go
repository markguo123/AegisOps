package k8sgateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func gatewayFixture(t *testing.T, readOnly bool) (Lease, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	hits := new(atomic.Int32)
	revoked := new(atomic.Bool)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Error("upstream credential not applied")
		}
		if r.Header.Get("Impersonate-User") != "" || r.Header.Get("X-Remote-User") != "" {
			t.Error("identity override forwarded")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"Deployment"}`)
	}))
	t.Cleanup(upstream.Close)
	access := &AccessContext{Current: func(context.Context) (*authz.EffectiveAccess, error) {
		return &authz.EffectiveAccess{ClusterID: "a", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{authz.ActionRead, authz.ActionOperate}}}}, nil
	}}
	gateway := NewInProcess(func(_ context.Context, target string) (BoundTarget, error) {
		if target != "a" {
			return BoundTarget{}, fmt.Errorf("wrong cluster")
		}
		return BoundTarget{Config: &rest.Config{Host: upstream.URL, BearerToken: "upstream-secret"}, Authorize: access.AuthorizeRequest, Validate: func(context.Context) error {
			if revoked.Load() {
				return fmt.Errorf("revoked")
			}
			return nil
		}}, nil
	})
	lease, e := gateway.Open(context.Background(), OpenRequest{TargetID: "a", ExecutionID: "single-call", ReadOnly: readOnly})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(lease.Close)
	if lease.Token == "upstream-secret" {
		t.Fatal("real credentials escaped")
	}
	return lease, hits, revoked
}
func gatewayCall(t *testing.T, lease Lease, method, path, body, token string, header http.Header) int {
	t.Helper()
	req, e := http.NewRequest(method, lease.Endpoint+path, strings.NewReader(body))
	if e != nil {
		t.Fatal(e)
	}
	req.Header = header
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["bound"] = &clientcmdapi.Cluster{Server: lease.Endpoint, CertificateAuthorityData: lease.CAData}
	cfg.AuthInfos["channel"] = &clientcmdapi.AuthInfo{Token: token}
	cfg.Contexts["bound"] = &clientcmdapi.Context{Cluster: "bound", AuthInfo: "channel"}
	cfg.CurrentContext = "bound"
	raw, e := clientcmd.Write(*cfg)
	if e != nil {
		t.Fatal(e)
	}
	loader, e := clientcmd.NewClientConfigFromBytes(raw)
	if e != nil {
		t.Fatal(e)
	}
	// Use kubectl's config loader to exercise TLS verification and bearer token loading.
	config, e := loader.ClientConfig()
	if e != nil {
		t.Fatal(e)
	}
	client, e := rest.HTTPClientFor(config)
	if e != nil {
		t.Fatal(e)
	}
	res, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)
	return res.StatusCode
}
func TestGatewayAuthorizesActualRequests(t *testing.T) {
	lease, hits, revoked := gatewayFixture(t, false)
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{{"GET", "/api/v1/namespaces/team/pods", "", 200}, {"PATCH", "/apis/apps/v1/namespaces/team/deployments/api", `{"spec":{"template":{"spec":{"containers":[{"name":"api","image":"example:v2"}]}}}}`, 200}, {"GET", "/api/v1/namespaces/other/pods", "", 403}, {"GET", "/api/v1/nodes", "", 403}, {"GET", "/api/v1/namespaces/team/secrets/db", "", 403}, {"POST", "/api/v1/namespaces/team/pods/api/exec", "", 403}, {"POST", "/api/v1/namespaces/team/pods", `{}`, 403}, {"PATCH", "/apis/apps/v1/namespaces/team/deployments/api", `[{"op":"replace","path":"/spec/template/spec/hostNetwork","value":true}]`, 403}} {
		if got := gatewayCall(t, lease, tc.method, tc.path, tc.body, lease.Token, nil); got != tc.want {
			t.Errorf("%s %s: %d", tc.method, tc.path, got)
		}
	}
	if hits.Load() != 2 {
		t.Fatalf("unsafe requests reached upstream: %d", hits.Load())
	}
	revoked.Store(true)
	if got := gatewayCall(t, lease, "GET", "/api/v1/namespaces/team/pods", "", lease.Token, nil); got != 403 {
		t.Fatalf("revocation: %d", got)
	}
}
func TestGatewayChannelCredentialsAndReadOnlyPermit(t *testing.T) {
	lease, hits, _ := gatewayFixture(t, true)
	if got := gatewayCall(t, lease, "GET", "/api/v1/namespaces/team/pods", "", "bad-token", nil); got != 401 {
		t.Fatal(got)
	}
	if got := gatewayCall(t, lease, "PATCH", "/apis/apps/v1/namespaces/team/deployments/api", `{}`, lease.Token, nil); got != 403 {
		t.Fatal(got)
	}
	if hits.Load() != 0 {
		t.Fatal("denied request escaped")
	}
}
func TestGatewayStripsIdentityHeaders(t *testing.T) {
	lease, _, _ := gatewayFixture(t, true)
	if got := gatewayCall(t, lease, "GET", "/api/v1/namespaces/team/pods", "", lease.Token, http.Header{"Impersonate-User": []string{"admin"}, "X-Remote-User": []string{"admin"}}); got != 200 {
		t.Fatal(got)
	}
}
func TestGatewayReceiptDoesNotDuplicateChecksOrAudit(t *testing.T) {
	checks, audits := 0, 0
	a := &AccessContext{Current: func(context.Context) (*authz.EffectiveAccess, error) {
		checks++
		return &authz.EffectiveAccess{ClusterID: "a", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{authz.ActionRead}}}}, nil
	}, Observe: func(context.Context, RequestRecord) error { audits++; return nil }}
	req := httptest.NewRequest("GET", "/api/v1/namespaces/team/pods", nil)
	if e := a.AuthorizeRequest(req); e != nil {
		t.Fatal(e)
	}
	if e := a.AuthorizeRequest(req); e != nil {
		t.Fatal(e)
	}
	if checks != 1 || audits != 1 {
		t.Fatalf("checks=%d audits=%d", checks, audits)
	}
	authz.InvalidateAuthorization()
	if e := a.AuthorizeRequest(req); e != nil {
		t.Fatal(e)
	}
	if checks != 2 {
		t.Fatal("stale authorization receipt reused")
	}
}
func TestGatewayAuditFailurePreventsAccess(t *testing.T) {
	a := &AccessContext{Current: func(context.Context) (*authz.EffectiveAccess, error) {
		return &authz.EffectiveAccess{ClusterID: "a", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{authz.ActionRead}}}}, nil
	}, Observe: func(context.Context, RequestRecord) error { return fmt.Errorf("offline") }}
	if e := a.AuthorizeRequest(httptest.NewRequest("GET", "/api/v1/namespaces/team/pods", nil)); e == nil {
		t.Fatal("audit failure ignored")
	}
}

func TestGatewayRejectsAuthorizationChangeDuringCheck(t *testing.T) {
	a := &AccessContext{Current: func(context.Context) (*authz.EffectiveAccess, error) {
		return &authz.EffectiveAccess{ClusterID: "a", Rules: []authz.AccessRule{{ScopeType: authz.ScopeNamespace, ResourceProfile: "builtin_namespaced", Namespaces: []string{"team"}, Actions: []string{authz.ActionRead}}}}, nil
	}, Observe: func(context.Context, RequestRecord) error { authz.InvalidateAuthorization(); return nil }}
	if err := a.AuthorizeRequest(httptest.NewRequest("GET", "/api/v1/namespaces/team/pods", nil)); err == nil {
		t.Fatal("stale decision issued a receipt")
	}
}

func TestGatewayScopesSelfSubjectAccessReviews(t *testing.T) {
	lease, hits, _ := gatewayFixture(t, true)
	for _, tc := range []struct {
		namespace string
		want      int
	}{{"team", 200}, {"other", 403}, {"", 403}} {
		body := fmt.Sprintf(`{"spec":{"resourceAttributes":{"namespace":%q,"verb":"get","resource":"pods"}}}`, tc.namespace)
		if got := gatewayCall(t, lease, "POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", body, lease.Token, nil); got != tc.want {
			t.Errorf("%s: %d", tc.namespace, got)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("out of scope review reached upstream")
	}
}
