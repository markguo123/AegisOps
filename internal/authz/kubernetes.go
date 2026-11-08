package authz

import (
	"errors"
	"net/http"

	"k8s.io/apimachinery/pkg/util/sets"
	krequest "k8s.io/apiserver/pkg/endpoints/request"
)

// Catalog keys are GVR group and plural resource. Discovery-only requests do
// not carry resource contents.
var namespaceResources = map[string]bool{"pods": true, "events": true, "services": true, "endpoints": true, "configmaps": true, "secrets": true, "persistentvolumeclaims": true, "apps/deployments": true, "apps/replicasets": true, "apps/statefulsets": true, "apps/daemonsets": true, "batch/jobs": true, "batch/cronjobs": true, "discovery.k8s.io/endpointslices": true, "networking.k8s.io/ingresses": true, "autoscaling/horizontalpodautoscalers": true, "policy/poddisruptionbudgets": true, "metrics.k8s.io/pods": true}
var clusterResources = map[string]bool{"nodes": true, "persistentvolumes": true, "namespaces": true, "storage.k8s.io/storageclasses": true, "metrics.k8s.io/nodes": true}
var requestInfo = krequest.RequestInfoFactory{APIPrefixes: sets.NewString("api", "apis"), GrouplessAPIPrefixes: sets.NewString("api")}

func (a EffectiveAccess) CheckRequest(r *http.Request) error {
	denied := errors.New("SCOPE_DENIED: unsupported API request")
	info, e := requestInfo.NewRequestInfo(r)
	if e != nil {
		return denied
	}
	if !info.IsResourceRequest {
		if r.Method == "GET" && (r.URL.Path == "/version" || info.APIPrefix == "api" || info.APIPrefix == "apis" || r.URL.Path == "/apis" || r.URL.Path == "/api") {
			return nil
		}
		return denied
	}
	if info.APIGroup == "authorization.k8s.io" && info.Resource == "selfsubjectaccessreviews" && info.Verb == "create" && len(info.Parts) == 1 {
		return nil
	}
	act := ActionRead
	switch info.Verb {
	case "get", "list", "watch":
	case "create", "update", "patch":
		act = ActionOperate
	case "delete", "deletecollection":
		act = ActionDestroy
	default:
		return denied
	}
	if len(info.Parts) > 3 {
		return denied
	}
	switch info.Subresource {
	case "":
	case "scale":
		if info.APIGroup != "apps" {
			return denied
		}
	case "log":
		if info.Resource != "pods" || info.Verb != "get" {
			return denied
		}
		act = ActionLogs
	default:
		return denied
	}
	cl, ok := ProfileResource(info.APIGroup, info.Resource, act)
	if !ok {
		return denied
	}
	ns := info.Namespace
	if cl && ns != "" && (info.Resource != "namespaces" || ns != info.Name) {
		return denied
	}
	if ns == "" {
		ns = "*"
	}
	return a.Check(Target{Resource: info.APIGroup + "/" + info.Resource, Subresource: info.Subresource, ClusterID: a.ClusterID, ClusterScoped: cl, Namespace: ns, Actions: []string{act}})
}
func ProfileResource(group, res, action string) (bool, bool) {
	key := res
	if group != "" {
		key = group + "/" + res
	}
	cluster := clusterResources[key]
	if !cluster && !namespaceResources[key] {
		return false, false
	}
	if action == ActionOperate || action == ActionDestroy {
		if res == "secrets" || group == "metrics.k8s.io" || cluster && res != "nodes" && res != "persistentvolumes" && res != "namespaces" {
			return false, false
		}
	}
	return cluster, true
}
