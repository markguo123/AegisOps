package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_service。
type serviceTool struct{ base }

func (t *serviceTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	s, err := t.reader.GetService(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ToolResult{Data: summarizeService(s), Text: fmt.Sprintf("service %s/%s type=%s clusterIP=%s", s.Namespace, s.Name, s.Spec.Type, s.Spec.ClusterIP)}, nil
}

// get_endpoints。
type endpointsTool struct{ base }

func (t *endpointsTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	e, err := t.reader.GetEndpoints(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ToolResult{Data: summarizeEndpoints(e), Text: fmt.Sprintf("endpoints %s/%s subsets=%d", e.Namespace, e.Name, len(e.Subsets))}, nil
}
