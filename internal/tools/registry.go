package tools

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/llm"
)

const CatalogVersion = "4.0.5"

// Registry 是工具注册表。
type Registry struct {
	tools map[string]Tool
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

// Register 注册工具；重名报错。
func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if name == "" {
		return fmt.Errorf("tool name must not be empty")
	}
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %q already registered", name)
	}
	r.tools[name] = t
	return nil
}

// Lookup 按名查询工具。
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Remove 删除指定名字的工具；不存在时静默忽略。
func (r *Registry) Remove(name string) {
	delete(r.tools, name)
}

func (r *Registry) Clone() *Registry {
	return &Registry{tools: maps.Clone(r.tools)}
}

// All 返回全部工具（名字排序，顺序稳定）。
func (r *Registry) All() []Tool {
	names := make([]string, 0, len(r.tools))
	for n := range r.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, n := range names {
		out = append(out, r.tools[n])
	}
	return out
}

// Schemas 渲染给 LLM 的工具定义（native 模式 tools 数组）。
func (r *Registry) Schemas() []llm.ToolSpec { return r.SchemasFor(nil) }

func (r *Registry) SchemasFor(scope *AccessScope) []llm.ToolSpec {
	all := r.All()
	out := make([]llm.ToolSpec, 0, len(all))
	for _, t := range all {
		schema := t.InputSchema()
		if scope != nil {
			if scoped, ok := t.(interface{ ScopedInputSchema(AccessScope) (any, bool) }); ok {
				var visible bool
				schema, visible = scoped.ScopedInputSchema(*scope)
				if !visible {
					continue
				}
			} else if provider, ok := t.(CapabilityProvider); ok {
				capability := provider.Capability()
				if !capability.Command && !scope.Advertises(capability) {
					continue
				}
			}
		}
		desc := t.Description()
		if md, ok := t.(MetadataDescriber); ok && strings.TrimSpace(md.SchemaMetadata()) != "" {
			desc = md.SchemaMetadata() + "\n" + desc
		}
		out = append(out, llm.ToolSpec{
			Name:        t.Name(),
			Description: desc,
			Parameters:  schema,
		})
	}
	return out
}

// Advertises tests whether any target for this capability is visible in the
// current presentation snapshot. Execution still reauthorizes the actual target.
func (scope AccessScope) Advertises(capability Capability) bool {
	switch capability.Domain {
	case "local":
		return true
	case "external_mcp":
		return scope.ExternalMCP
	case "kubernetes":
	default:
		return false
	}
	if scope.Policy == nil {
		if scope.IsAdmin || scope.ClusterScoped {
			return true
		}
		raw, _ := json.Marshal(map[string]string{"namespace": "candidate"})
		facts, err := capability.Facts(raw, scope)
		if err != nil || len(scope.Namespaces) == 0 {
			return false
		}
		for _, target := range facts.Targets {
			if target.ClusterScoped {
				return false
			}
		}
		return true
	}
	policy := *scope.Policy
	policy.Observe = nil
	scope.Policy = &policy
	candidates := []string{"*"}
	for _, rule := range scope.Policy.Rules {
		if rule.ScopeType == authz.ScopeNamespace {
			candidates = append(candidates, rule.Namespaces...)
		}
	}
	for _, ns := range candidates {
		raw, _ := json.Marshal(map[string]string{"namespace": ns})
		facts, err := capability.Facts(raw, scope)
		if err == nil && scope.AuthorizeOperation(facts) == nil {
			return true
		}
	}
	if capability.ClusterRead && capability.Resource == "" {
		facts, err := capability.Facts(json.RawMessage(`{}`), scope)
		return err == nil && scope.AuthorizeOperation(facts) == nil
	}
	return false
}
