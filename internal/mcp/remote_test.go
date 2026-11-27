package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/tools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRemoteTransports(t *testing.T) {
	for _, transport := range []string{"streamable-http", "sse"} {
		t.Run(transport, func(t *testing.T) {
			server := sdk.NewServer(&sdk.Implementation{Name: "test"}, nil)
			sdk.AddTool(server, &sdk.Tool{Name: "lookup"}, func(context.Context, *sdk.CallToolRequest, map[string]any) (*sdk.CallToolResult, map[string]any, error) {
				return nil, map[string]any{"value": "found"}, nil
			})
			resolve := func(*http.Request) *sdk.Server { return server }
			var handler http.Handler = sdk.NewStreamableHTTPHandler(resolve, &sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
			if transport == "sse" {
				handler = sdk.NewSSEHandler(resolve, nil)
			}
			local := httptest.NewServer(handler)
			defer local.Close()
			registry := tools.NewRegistry()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closeRemote, err := RegisterRemote(ctx, registry, config.MCPServerConfig{Name: "demo", URL: local.URL, Transport: transport, AllowHTTP: true, AllowedTools: []string{"lookup"}, ReadOnlyTools: []string{"lookup"}})
			if err != nil {
				t.Fatal(err)
			}
			defer closeRemote()
			cancel()
			tool, ok := registry.Lookup("mcp_demo_lookup")
			if !ok {
				t.Fatal("tool missing")
			}
			if !tool.IsReadOnly(nil) || !tool.(tools.MandatoryHumanApproval).RequiresHumanApproval(nil) {
				t.Fatal("configured read-only MCP tool must retain human approval")
			}
			call, cancelCall := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelCall()
			result, err := tool.Execute(call, []byte("{}"))
			data, _ := result.Data.(map[string]any)
			content, _ := data["content"].(map[string]any)
			if err != nil || content["value"] != "found" || data["untrusted"] != true {
				t.Fatalf("result: %+v, error: %v", result, err)
			}
		})
	}
}
