package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// ToolAdapter 把 MCP 工具包装为 AegisOps 的 tools.Tool。
// 安全侧默认：非只读 + 风险 CRITICAL，必须经审批策略后才能执行。
type ToolAdapter struct {
	readOnly  bool
	authorize func(context.Context) error
	alias     string
	server    string
	spec      Tool
	client    Client
}

// NewToolAdapter 创建 MCP 工具适配器。
func NewToolAdapter(client Client, spec Tool) *ToolAdapter {
	return &ToolAdapter{server: client.Name(), spec: spec, client: client}
}

// Name 使用 <server>.<tool> 唯一名，避免与内置工具或其他 MCP 服务器冲突。
func (a *ToolAdapter) Name() string {
	if a.alias != "" {
		return a.alias
	}
	return a.server + "." + a.spec.Name
}

func (a *ToolAdapter) Capability() tools.Capability               { return tools.Capability{Domain: "external_mcp"} }
func (a *ToolAdapter) RequiresHumanApproval(json.RawMessage) bool { return true }

func (a *ToolAdapter) Description() string { return a.spec.Description }
func (a *ToolAdapter) InputSchema() any {
	if a.spec.InputSchema != nil {
		return a.spec.InputSchema
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func (a *ToolAdapter) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	if a.authorize != nil {
		if err := a.authorize(ctx); err != nil {
			return tools.ResultErrorWithCode(tools.ErrorConflict, err.Error()), nil
		}
	}
	var args map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &args); err != nil {
			return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "mcp arguments: "+err.Error()), nil
		}
	}
	out, err := a.client.CallTool(ctx, a.spec.Name, args)
	if err != nil {
		var failure *callError
		if errors.As(err, &failure) {
			return tools.ResultErrorWithCode(failure.code, failure.message), nil
		}
		return tools.ResultErrorWithCode(tools.ErrorToolFailed, "MCP tool call failed"), nil
	}
	return tools.ToolResult{Data: map[string]any{"content": out, "source": a.Name(), "untrusted": true}}, nil
}

type callError struct{ code, message string }

func (e *callError) Error() string { return e.message }

// IsReadOnly MCP 工具默认为非只读（安全侧，必须审批）。
func (a *ToolAdapter) IsReadOnly(json.RawMessage) bool { return a.readOnly }

// Risk MCP 工具默认 CRITICAL（安全侧）。
func (a *ToolAdapter) Risk(json.RawMessage) model.RiskLevel {
	if a.readOnly {
		return model.RiskLow
	}
	return model.RiskCritical
}

// RegisterAll 把某 MCP 客户端的全部工具注册进注册表（唯一名前缀）。
func RegisterAll(reg *tools.Registry, client Client) error {
	if reg == nil || client == nil {
		return fmt.Errorf("mcp: registry and client must not be nil")
	}
	toolsList, err := client.ListTools(context.Background())
	if err != nil {
		return fmt.Errorf("mcp list tools %s: %w", client.Name(), err)
	}
	for _, spec := range toolsList {
		if err := reg.Register(NewToolAdapter(client, spec)); err != nil {
			return fmt.Errorf("mcp register %s.%s: %w", client.Name(), spec.Name, err)
		}
	}
	return nil
}
