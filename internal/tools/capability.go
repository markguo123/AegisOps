package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
)

// Capability describes operation facts. It contains no principal or grant rules.
type Capability struct {
	Domain      string
	Resource    string
	Action      string
	ClusterRead bool
	Command     bool
}
type CapabilityProvider interface{ Capability() Capability }
type OperationFacts struct {
	Local    bool
	Metadata bool
	Deferred bool
	Targets  []authz.Target
}
type OperationDescriber interface {
	OperationFacts(json.RawMessage) (OperationFacts, error)
}

func (c Capability) Facts(input json.RawMessage, scope AccessScope) (OperationFacts, error) {
	if c.Domain == "external_mcp" {
		if !scope.ExternalMCP {
			return OperationFacts{}, fmt.Errorf("external MCP requires administrator access")
		}
		return OperationFacts{Local: true}, nil
	}
	if c.Domain == "local" {
		return OperationFacts{Local: true}, nil
	}
	if c.Domain != "kubernetes" {
		return OperationFacts{}, fmt.Errorf("UNSUPPORTED: resource domain is not connected")
	}
	if c.Command && scope.RequestEnforced {
		return OperationFacts{Deferred: true}, nil
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return OperationFacts{}, fmt.Errorf("INVALID_ARGUMENT: %v", err)
	}
	ns := stringArg(args, "namespace")
	if ns == "" {
		ns = "*"
	}
	action := c.Action
	if action == "" {
		action = authz.ActionRead
	}
	resource := c.Resource
	cluster := c.ClusterRead
	if c.ClusterRead && c.Resource == "" && stringArg(args, "namespace") != "" {
		cluster = false
	}
	if resource != "" {
		g, res, ok := strings.Cut(resource, "/")
		if !ok {
			return OperationFacts{}, fmt.Errorf("UNSUPPORTED: resource must use group/resource identity")
		}
		var supported bool
		cluster, supported = authz.ProfileResource(g, res, action)
		if !supported {
			return OperationFacts{}, fmt.Errorf("UNSUPPORTED: RESOURCE_PROFILE_UNSUPPORTED: %s", resource)
		}
	}
	target := authz.Target{Namespace: ns, ClusterScoped: cluster, Resource: resource, Actions: []string{action}}
	if cluster {
		target.Namespace = ""
	}
	facts := OperationFacts{Targets: []authz.Target{target}}
	if c.ClusterRead && c.Resource == "" && stringArg(args, "namespace") == "" {
		facts.Targets = append(facts.Targets, authz.Target{Namespace: "*", Actions: []string{action}})
	}
	return facts, nil
}

func (s AccessScope) AuthorizeOperation(facts OperationFacts) error {
	if facts.Local {
		return nil
	}
	if s.Policy == nil {
		return fmt.Errorf("SCOPE_DENIED: platform Authorizer is unavailable")
	}
	if facts.Metadata || facts.Deferred {
		return s.Policy.CheckMetadata()
	}
	if len(facts.Targets) == 0 {
		return fmt.Errorf("UNSUPPORTED: tool has no operation declaration")
	}
	for _, target := range facts.Targets {
		target.ClusterID = s.Policy.ClusterID
		if err := s.Policy.Check(target); err != nil {
			return err
		}
	}
	return nil
}

func (t *kubectlTool) Capability() Capability { return Capability{Domain: "kubernetes", Command: true} }
func (t *podExecTool) OperationFacts(input json.RawMessage) (OperationFacts, error) {
	r, e := cmdplan.DecodePodExec(input)
	return OperationFacts{Targets: []authz.Target{{Namespace: r.Namespace, Resource: "/pods", Name: r.Pod, Actions: []string{authz.ActionExec}}}}, e
}
func (t *listSkillsTool) Capability() Capability { return Capability{Domain: "local"} }
func (t *getSkillTool) Capability() Capability   { return Capability{Domain: "local"} }
func (t *scanClusterTool) Capability() Capability {
	return Capability{Domain: "kubernetes", ClusterRead: true}
}

// CommandFacts is only an input adapter for the legacy CLI path. Platform
// kubectl uses request facts from its mandatory API channel instead.
func (t *kubectlTool) CommandFacts(input json.RawMessage) (cmdplan.Analysis, error) {
	a, e := ParseArgs(input, "command")
	if e != nil {
		return cmdplan.Analysis{}, e
	}
	return cmdplan.Analyze(a["command"]), nil
}

// Authorize resolves trusted facts and delegates all permission decisions.
func (r *Registry) Authorize(scope AccessScope, name string, input json.RawMessage) error {
	tool, ok := r.Lookup(name)
	if !ok {
		return fmt.Errorf("UNSUPPORTED: unknown tool")
	}
	if d, ok := tool.(OperationDescriber); ok {
		facts, e := d.OperationFacts(input)
		if e != nil {
			return e
		}
		return scope.AuthorizeOperation(facts)
	}
	if d, ok := tool.(CapabilityProvider); ok {
		c := d.Capability()
		if c.Command && !scope.RequestEnforced {
			return scope.commandPolicy(stringCommand(input))
		}
		facts, e := c.Facts(input, scope)
		if e != nil {
			return e
		}
		return scope.AuthorizeOperation(facts)
	}
	return fmt.Errorf("UNSUPPORTED: tool has no capability declaration")
}

// AuthorizeLegacy applies scope-only authorization to canonical read routes
// for callers that do not have a platform EffectiveAccess.
func (r *Registry) AuthorizeLegacy(scope AccessScope, name string, input json.RawMessage) error {
	if !isKindReadTool(name) {
		return scope.Check(name, input)
	}
	tool, _ := r.Lookup(name)
	route, ok := tool.(OperationDescriber)
	if !ok {
		return fmt.Errorf("UNSUPPORTED: tool unavailable")
	}
	facts, err := route.OperationFacts(input)
	if err != nil {
		return err
	}
	if scope.IsAdmin || scope.ClusterScoped {
		return nil
	}
	for _, target := range facts.Targets {
		if target.ClusterScoped {
			return scope.deny("cluster scoped read access")
		}
		if err := scope.checkNamespace(target.Namespace); err != nil {
			return err
		}
	}
	return nil
}

func stringCommand(input json.RawMessage) string {
	args, _ := ParseArgs(input, "command")
	return args["command"]
}
