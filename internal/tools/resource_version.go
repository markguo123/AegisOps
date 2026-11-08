package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/kubernetes/scheme"
)

// PrepareExecution captures high-risk targets before confirmation and checks
// UID/resourceVersion again immediately before dispatch.
func (t *kubectlTool) PrepareExecution(ctx context.Context, input json.RawMessage, scope *AccessScope) (func(context.Context) error, error) {
	if t.Risk(input) != model.RiskHigh && t.Risk(input) != model.RiskCritical {
		return nil, nil
	}
	reader, ok := t.runner.(interface {
		GetObjectStatus(context.Context, executor.ObjectRef) (executor.ObjectStatus, error)
	})
	if !ok {
		return nil, fmt.Errorf("resource version checking unavailable")
	}
	current := scope
	if scope.Refresh != nil {
		var err error
		current, err = scope.Refresh(ctx)
		if err != nil {
			return nil, err
		}
	}
	policy := *current.Policy
	var targets []authz.Target
	policy.Observe = func(target authz.Target) {
		for _, action := range target.Actions {
			if action == authz.ActionOperate || action == authz.ActionDestroy || action == authz.ActionExec {
				targets = append(targets, target)
				break
			}
		}
	}
	checked := *current
	checked.Policy = &policy
	if err := checked.Check(t.Name(), input); err != nil {
		return nil, err
	}
	type snapshot struct {
		ref   executor.ObjectRef
		state executor.ObjectStatus
	}
	states := make([]snapshot, 0, len(targets))
	for _, target := range targets {
		if target.Name == "" || target.Namespace == "*" {
			return nil, fmt.Errorf("high-risk confirmation requires explicitly named resources")
		}
		ref, ok := versionedObjectRef(target)
		if !ok {
			return nil, fmt.Errorf("cannot resolve high-risk target")
		}
		state, err := reader.GetObjectStatus(ctx, ref)
		if err != nil {
			return nil, err
		}
		states = append(states, snapshot{ref, state})
	}
	return func(ctx context.Context) error {
		for _, saved := range states {
			now, err := reader.GetObjectStatus(ctx, saved.ref)
			if err != nil {
				return err
			}
			if now.Found != saved.state.Found || now.UID != saved.state.UID || now.ResourceVersion != saved.state.ResourceVersion {
				return fmt.Errorf("resource changed while awaiting confirmation; request a new confirmation")
			}
		}
		return nil
	}, nil
}
func versionedObjectRef(target authz.Target) (executor.ObjectRef, bool) {
	group, resource, ok := strings.Cut(target.Resource, "/")
	if !ok {
		return executor.ObjectRef{}, false
	}
	for _, version := range scheme.Scheme.PrioritizedVersionsForGroup(group) {
		for kind := range scheme.Scheme.KnownTypes(version) {
			gvr, _ := meta.UnsafeGuessKindToResource(version.WithKind(kind))
			if gvr.Resource != resource {
				continue
			}
			namespace := target.Namespace
			if target.ClusterScoped {
				namespace = ""
			}
			return executor.ObjectRef{APIVersion: version.String(), Kind: kind, Namespace: namespace, Name: target.Name}, true
		}
	}
	return executor.ObjectRef{}, false
}
