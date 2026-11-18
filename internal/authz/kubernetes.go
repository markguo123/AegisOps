package authz

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

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
			return a.CheckMetadata()
		}
		return denied
	}
	if info.APIGroup == "authorization.k8s.io" && info.Resource == "selfsubjectaccessreviews" && info.Verb == "create" && len(info.Parts) == 1 {
		// A review may inspect only a resource within the caller's platform scope.
		// Preserve the body for client-go's transport after checking it.
		if r.Body == nil {
			return denied
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if err != nil || len(raw) > 65536 {
			return denied
		}
		var review struct {
			Spec struct {
				ResourceAttributes *struct{ Namespace, Verb, Group, Resource, Subresource, Name string }
			}
		}
		if json.Unmarshal(raw, &review) != nil || review.Spec.ResourceAttributes == nil {
			return denied
		}
		attr := review.Spec.ResourceAttributes
		action := ActionRead
		switch attr.Verb {
		case "get", "list", "watch":
		case "create", "patch", "update":
			action = ActionOperate
		case "delete", "deletecollection":
			action = ActionDestroy
		default:
			return denied
		}
		switch attr.Subresource {
		case "":
		case "exec":
			if attr.Resource != "pods" {
				return denied
			}
			action = ActionExec
		case "log":
			if attr.Resource != "pods" {
				return denied
			}
			action = ActionLogs
		case "scale":
			if attr.Group != "apps" {
				return denied
			}
		default:
			return denied
		}
		cluster, ok := ProfileResource(attr.Group, attr.Resource, action)
		if !ok || cluster && attr.Namespace != "" {
			return denied
		}
		ns := attr.Namespace
		if ns == "" {
			ns = "*"
		}
		return a.Check(Target{ClusterID: a.ClusterID, Namespace: ns, ClusterScoped: cluster, Resource: attr.Group + "/" + attr.Resource, Name: attr.Name, Subresource: attr.Subresource, Actions: []string{action}})
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
	case "exec":
		if info.Resource != "pods" || info.Name == "" || r.Method != "POST" {
			return denied
		}
		act = ActionExec
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
	return a.Check(Target{Name: info.Name, Resource: info.APIGroup + "/" + info.Resource, Subresource: info.Subresource, ClusterID: a.ClusterID, ClusterScoped: cl, Namespace: ns, Actions: []string{act}})
}

// CheckMetadata grants only discovery/version access, never resource contents.
func (a EffectiveAccess) CheckMetadata() error {
	for _, rule := range a.Rules {
		if rule.ExpiresAt != nil && !rule.ExpiresAt.After(time.Now()) {
			continue
		}
		if validAccessRule(rule) && len(rule.Actions) > 0 {
			return nil
		}
	}
	return errors.New("SCOPE_DENIED: cluster metadata requires a resource grant")
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
