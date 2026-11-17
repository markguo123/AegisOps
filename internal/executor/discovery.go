package executor

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResolveCommandResource resolves identity and scope only. The caller must still
// require a supported platform profile and authorize the resolved resource.
// channel's discovery cache belongs to this target/credential-bound Runner.
func (r *Runner) ResolveCommandResource(ctx context.Context, kind string) (string, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return "", "", false, err
		}
	}
	ch, err := r.channel()
	if err != nil {
		return "", "", false, fmt.Errorf("UNSUPPORTED: resource discovery unavailable: %w", err)
	}
	specified, gr := schema.ParseResourceArg(kind)
	gvr := gr.WithVersion("")
	if specified != nil {
		gvr = *specified
	}
	resource, err := ch.mapper.ResourceFor(gvr)
	if err != nil && specified != nil {
		resource, err = ch.mapper.ResourceFor(gr.WithVersion(""))
	}
	if err != nil {
		return "", "", false, fmt.Errorf("UNSUPPORTED: resource discovery failed: %w", err)
	}
	gvk, err := ch.mapper.KindFor(resource)
	if err != nil {
		return "", "", false, fmt.Errorf("UNSUPPORTED: kind discovery failed: %w", err)
	}
	mapping, err := ch.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return "", "", false, fmt.Errorf("UNSUPPORTED: scope discovery failed: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	return resource.Group, resource.Resource, mapping.Scope.Name() == meta.RESTScopeNameRoot, nil
}
