package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/k8s-ai/k8s-ai/internal/model"
)

// KindRoute binds a canonical input to an existing read-only tool contract.
type KindRoute struct {
	Kind              string
	Execute           func(context.Context, json.RawMessage) (ToolResult, error)
	Capability        Capability
	NamespaceRequired bool
	ClusterScoped     bool
}

type kindTool struct {
	name, description string
	routes            []KindRoute
}

func NewKindTool(name, description string, routes []KindRoute) Tool {
	return &kindTool{name: name, description: description, routes: routes}
}

func (t *kindTool) Name() string                         { return t.name }
func (t *kindTool) Description() string                  { return t.description }
func (t *kindTool) IsReadOnly(json.RawMessage) bool      { return true }
func (t *kindTool) Risk(json.RawMessage) model.RiskLevel { return model.RiskSafe }
func (t *kindTool) SchemaMetadata() string               { return "[READ ONLY][RISK: SAFE][APPROVAL REQUIRED: NO]" }

func (t *kindTool) InputSchema() any {
	return t.schema(nil)
}

func (t *kindTool) ScopedInputSchema(scope AccessScope) (any, bool) {
	schema := t.schema(&scope)
	return schema, schema != nil
}

func (t *kindTool) schema(scope *AccessScope) any {
	kinds := make([]string, 0, len(t.routes))
	for _, route := range t.routes {
		if scope == nil || scope.Advertises(route.Capability) {
			kinds = append(kinds, route.Kind)
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	slices.Sort(kinds)
	return objSchema(map[string]any{
		"kind":      map[string]any{"type": "string", "enum": kinds},
		"namespace": strProp("Namespace for namespaced resources"),
		"name":      strProp("Exact resource name"),
	}, "kind", "name")
}
func (t *kindTool) resolve(input json.RawMessage) (KindRoute, error) {
	args, err := ParseArgs(input, "kind", "name")
	if err != nil {
		return KindRoute{}, fmt.Errorf("INVALID_ARGUMENT: %w", err)
	}
	for _, route := range t.routes {
		if args["kind"] != route.Kind {
			continue
		}
		namespace := args["namespace"]
		if route.NamespaceRequired && namespace == "" {
			return KindRoute{}, fmt.Errorf("INVALID_ARGUMENT: namespace is required for %s", route.Kind)
		}
		if route.ClusterScoped && namespace != "" {
			return KindRoute{}, fmt.Errorf("INVALID_ARGUMENT: %s is cluster scoped", route.Kind)
		}
		return route, nil
	}
	return KindRoute{}, fmt.Errorf("INVALID_ARGUMENT: unsupported kind %q", args["kind"])
}
func (t *kindTool) OperationFacts(input json.RawMessage) (OperationFacts, error) {
	tool, err := t.resolve(input)
	if err != nil {
		return OperationFacts{}, err
	}
	return tool.Capability.Facts(input, AccessScope{})
}
func (t *kindTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	tool, err := t.resolve(input)
	if err != nil {
		return ResultErrorWithCode(ErrorInvalidArgument, err.Error()), nil
	}
	return tool.Execute(ctx, input)
}
