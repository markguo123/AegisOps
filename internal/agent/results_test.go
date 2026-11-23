package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func newResultAgent() *Agent {
	return New(Options{Tools: tools.NewRegistry(), Session: &session.Session{ID: "s1", UserID: "u1", Cluster: "b"}, ResultMaxBytes: 128})
}

func TestLargeResultPagingAndOwnership(t *testing.T) {
	a := newResultAgent()
	a.AccessScope = &tools.AccessScope{Namespaces: []string{"prod"}}
	preview := a.resultForModel(context.Background(), call("c1", "get_logs", `{"namespace":"prod","name":"web-0"}`), tools.ToolResult{Text: strings.Repeat("日志 data ", 100)})
	if !strings.Contains(preview, "result_id=") || len(a.resultCache.entries) != 1 {
		t.Fatal("missing scoped reference")
	}
	var id string
	for key := range a.resultCache.entries {
		id = key
	}
	read := func(offset int) tools.ToolResult {
		raw, _ := json.Marshal(map[string]any{"result_id": id, "offset": offset, "limit": 128})
		tr, err := a.resultCache.Execute(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		return tr
	}
	first := read(0)
	if !tools.Present(first).Ok {
		t.Fatalf("read failed: %s", first.Text)
	}
	next := first.Data.(map[string]any)["next_offset"].(int)
	if next <= 0 || !tools.Present(read(next)).Ok {
		t.Fatal("cannot continue UTF-8 page")
	}
	a.Session.UserID = "other"
	if tools.Present(read(0)).Ok {
		t.Fatal("cross-owner reference read allowed")
	}
	a.Session.UserID = "u1"
	a.Session.Cluster = "c"
	if tools.Present(read(0)).Ok {
		t.Fatal("cross-cluster reference read allowed")
	}
	a.Session.Cluster = "b"
	a.AccessScope.Refresh = func(context.Context) (*tools.AccessScope, error) {
		return &tools.AccessScope{Namespaces: []string{"other"}}, nil
	}
	if tools.Present(read(0)).Ok {
		t.Fatal("revoked source resource access allowed")
	}
}

func TestSensitiveResultsAndReaderNeverSpill(t *testing.T) {
	for _, mode := range []string{"model-only", "known-secret", "reader"} {
		t.Run(mode, func(t *testing.T) {
			a := newResultAgent()
			tr := tools.ToolResult{Text: strings.Repeat("large", 200)}
			name := "get_logs"
			if mode == "model-only" {
				tr.ModelOnly = map[string]any{"secret": "approved-value"}
			}
			if mode == "known-secret" {
				a.recordSensitiveValues([]string{"approved-value"})
			}
			if mode == "reader" {
				name = "read_tool_result"
			}
			a.resultForModel(context.Background(), call("c1", name, `{}`), tr)
			if len(a.resultCache.entries) != 0 {
				t.Fatal("sensitive or recursive result stored")
			}
		})
	}
}

func TestReaderRejectsUnknownReferencesAndBudgets(t *testing.T) {
	a := newResultAgent()
	a.resultForModel(context.Background(), call("c1", "read", `{}`), tools.ToolResult{Text: strings.Repeat("x", 2000)})
	var id string
	for key := range a.resultCache.entries {
		id = key
	}
	for _, args := range []string{`{"result_id":"../file"}`, `{"result_id":"` + id + `","offset":-1}`, `{"result_id":"` + id + `","limit":99999}`, `{"result_id":"` + id + `","path":"server"}`} {
		tr, _ := a.resultCache.Execute(context.Background(), json.RawMessage(args))
		if tools.Present(tr).Ok {
			t.Fatalf("accepted invalid reader arguments %s", args)
		}
	}
	a.resultCache.readBytes = resultCacheBytes
	tr, _ := a.resultCache.Execute(context.Background(), json.RawMessage(`{"result_id":"`+id+`"}`))
	if tools.Present(tr).Ok {
		t.Fatal("unbounded repeated reads allowed")
	}
}

func TestResultReaderRegistryIsIsolatedAndCanBeDisabled(t *testing.T) {
	reg := tools.NewRegistry()
	a := New(Options{Tools: reg, DisabledTools: []string{"read_tool_result"}})
	if _, ok := a.Tools.Lookup("read_tool_result"); ok {
		t.Fatal("disabled reader re-enabled")
	}
	_ = New(Options{Tools: reg})
	if _, ok := reg.Lookup("read_tool_result"); ok {
		t.Fatal("mutated shared registry")
	}
}

func TestReaderReferencesExpireWhenTurnEnds(t *testing.T) {
	a := newResultAgent()
	a.LLM = &fakeLLM{responses: []*llm.ChatResponse{{Content: "done"}}}
	a.resultCache.entries["old"] = resultEntry{text: "old", size: 3, session: "s1", owner: "u1", cluster: "b", source: llm.ToolCall{Name: "read"}}
	if _, _, err := a.Chat(context.Background(), "q"); err != nil {
		t.Fatal(err)
	}
	if len(a.resultCache.entries) != 0 {
		t.Fatal("references survived a turn boundary")
	}
}
