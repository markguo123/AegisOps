package tools

import (
	"fmt"
	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/manifestx"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes/scheme"
)

func ObjectOperationFacts(objects []manifestx.Object) (OperationFacts, error) {
	facts := OperationFacts{}
	if len(objects) == 0 {
		return facts, fmt.Errorf("INVALID_ARGUMENT: no objects")
	}
	for _, obj := range objects {
		gvr := obj.GVR
		if !obj.Mapped {
			if !scheme.Scheme.Recognizes(obj.GVK) {
				return facts, fmt.Errorf("UNSUPPORTED: unresolved resource")
			}
			gvr, _ = meta.UnsafeGuessKindToResource(obj.GVK)
		}
		ns := obj.Namespace
		if ns == "" {
			ns = "default"
		}
		cluster, ok := authz.ProfileResource(gvr.Group, gvr.Resource, authz.ActionOperate)
		if !ok {
			return facts, fmt.Errorf("UNSUPPORTED: resource profile")
		}
		if cluster {
			ns = ""
		}
		facts.Targets = append(facts.Targets, authz.Target{Namespace: ns, ClusterScoped: cluster, Resource: gvr.Group + "/" + gvr.Resource, Name: obj.Name, Actions: []string{authz.ActionOperate}})
	}
	return facts, nil
}
