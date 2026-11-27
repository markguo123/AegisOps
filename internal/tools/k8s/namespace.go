package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_namespace。
type namespaceTool struct{ base }

func (t *namespaceTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	ns, err := t.reader.GetNamespace(ctx, args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ToolResult{Data: summarizeNamespace(ns), Text: fmt.Sprintf("namespace %s phase=%s", ns.Name, ns.Status.Phase)}, nil
}
