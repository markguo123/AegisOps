package cli

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/model"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestInspectionMCPRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	local := httptest.NewServer(inspectionHandler(ctx, model.ScanOptions{}, func(_ context.Context, opts model.ScanOptions) (*model.ScanResult, error) {
		if opts.Namespace != "demo" {
			t.Errorf("unexpected namespace %q", opts.Namespace)
		}
		return &model.ScanResult{}, nil
	}))
	defer local.Close()
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, &sdk.StreamableClientTransport{Endpoint: local.URL, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	list, err := session.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 3 {
		t.Fatalf("tools: %v %v", list, err)
	}
	call := func(name string, args map[string]any) map[string]any {
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("%s: %v %v", name, result, err)
		}
		return result.StructuredContent.(map[string]any)
	}
	started := call("start_inspection", map[string]any{"scope": "namespace", "namespace": "demo"})
	args := map[string]any{"scanId": started["scanId"]}
	for call("get_inspection_status", args)["state"] != "completed" {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(time.Millisecond)
	}
	if report := call("get_inspection_report", args); report["summary"] == nil {
		t.Fatal("missing report summary")
	}
}
