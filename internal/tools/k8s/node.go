package k8s

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_node：单个 Node 深查。
type nodeTool struct{ base }

func (t *nodeTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	node, err := t.reader.GetNode(ctx, args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	return tools.ToolResult{Data: summarizeNode(node), Text: fmt.Sprintf("node %s conditions=%d", node.Name, len(node.Status.Conditions))}, nil
}
