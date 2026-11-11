package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/security"
	"github.com/k8s-ai/k8s-ai/internal/session"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// Fixed synthetic evidence makes the real-model baseline repeatable without
// granting the evaluator Kubernetes credentials or a mutation channel.
type faultFixture struct {
	ID       string
	Question string
	Evidence string
	Markers  []string
}

var faultFixtures = []faultFixture{
	{
		ID:       "oom",
		Question: "诊断 eval/web-0 重启原因，引用返回的资源名和 termination reason；没有证据的结论明确标记未知。",
		Evidence: `{"namespace":"eval","name":"web-0","restartCount":4,"lastTermination":{"reason":"OOMKilled","exitCode":137},"memoryLimit":"128Mi"}`,
		Markers:  []string{"web-0", "OOMKilled"},
	},
	{
		ID:       "image-pull",
		Question: "诊断 eval/api-0 启动失败，引用资源名和事件 reason；不要声称已修复。",
		Evidence: `{"namespace":"eval","name":"api-0","waitingReason":"ImagePullBackOff","events":[{"reason":"Failed","message":"manifest unknown for image example.invalid/api:missing"}]}`,
		Markers:  []string{"api-0", "ImagePullBackOff", "manifest"},
	},
	{
		ID:       "unschedulable",
		Question: "诊断 eval/worker-0 Pending 原因，引用资源名和调度证据；不能从这些数据推断节点 CPU 利用率。",
		Evidence: `{"namespace":"eval","name":"worker-0","phase":"Pending","events":[{"reason":"FailedScheduling","message":"0/2 nodes are available: 2 Insufficient cpu"}]}`,
		Markers:  []string{"worker-0", "Insufficient cpu"},
	},
}

type baselineTool struct {
	fixture faultFixture
	calls   int
}

func (*baselineTool) Name() string { return "get_diagnostic_evidence" }
func (*baselineTool) Description() string {
	return "Read the fixed synthetic Kubernetes evidence for this evaluation case. Call before diagnosing; this is the only evidence source."
}
func (*baselineTool) InputSchema() any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}
func (*baselineTool) IsReadOnly(json.RawMessage) bool      { return true }
func (*baselineTool) Risk(json.RawMessage) model.RiskLevel { return model.RiskLow }
func (b *baselineTool) Execute(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return tools.ToolResult{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(args, &fields); err != nil || fields == nil || len(fields) != 0 {
		return tools.ResultError("expected an empty JSON object"), nil
	}
	b.calls++
	return tools.ToolResult{Text: b.fixture.Evidence}, nil
}

type baselineClient struct {
	client llm.LLMClient
	calls  int
}

func (b *baselineClient) Chat(ctx context.Context, req llm.ChatRequest) (*llm.ChatResponse, error) {
	b.calls++
	return b.client.Chat(ctx, req)
}

type baselineRecord struct {
	Case            string         `json:"case"`
	FixtureSHA256   string         `json:"fixture_sha256"`
	Mode            string         `json:"mode"`
	Model           string         `json:"model"`
	Completed       bool           `json:"completed"`
	EvidenceMarkers bool           `json:"evidence_markers"`
	Requests        int            `json:"requests"`
	ToolCalls       int            `json:"tool_calls"`
	Usage           llm.TokenUsage `json:"usage"`
	ElapsedMS       int64          `json:"elapsed_ms"`
	ReviewAnswer    string         `json:"review_answer"`
}

func TestAgentFaultBaseline(t *testing.T) {
	realModel := os.Getenv("AEGISOPS_EVAL_REAL_MODEL") == "1"
	endpoint, modelName := os.Getenv("AEGISOPS_EVAL_ENDPOINT"), os.Getenv("AEGISOPS_EVAL_MODEL")
	if realModel && (endpoint == "" || modelName == "") {
		t.Fatal("real-model evaluation requires AEGISOPS_EVAL_ENDPOINT and AEGISOPS_EVAL_MODEL")
	}
	for _, fixture := range faultFixtures {
		t.Run(fixture.ID, func(t *testing.T) {
			var client llm.LLMClient
			mode, recordedModel := "scripted-preflight", "scripted"
			if realModel {
				mode, recordedModel = "real-model-synthetic-evidence", modelName
				client = llm.New(llm.Options{Endpoint: endpoint, Model: modelName,
					APIKey: os.Getenv("AEGISOPS_EVAL_API_KEY"), Temperature: 0,
					MaxTokens: 2048, Timeout: 60 * time.Second, DisableStream: true})
			} else {
				client = &fakeLLM{responses: []*llm.ChatResponse{
					toolCallResp(call("evidence-1", "get_diagnostic_evidence", `{}`)),
					{Content: strings.Join(fixture.Markers, " ")},
				}}
			}
			measured := &baselineClient{client: client}
			tool := &baselineTool{fixture: fixture}
			registry := tools.NewRegistry()
			if err := registry.Register(tool); err != nil {
				t.Fatal(err)
			}
			a := New(Options{LLM: measured, Tools: registry,
				OutputTokens:  2048,
				Session:       &session.Session{ID: "baseline-" + fixture.ID},
				MaxIterations: 5, MaxToolCalls: 5, Timeout: 2 * time.Minute})
			// Drain UI events so an evaluation cannot block on an absent UI consumer.
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for range a.Output {
				}
			}()
			start := time.Now()
			answer, usage, err := a.Chat(context.Background(), fixture.Question)
			close(a.Output)
			<-drained
			markers := true
			for _, marker := range fixture.Markers {
				markers = markers && strings.Contains(strings.ToLower(answer), strings.ToLower(marker))
			}
			hashInput, marshalErr := json.Marshal(fixture)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			digest := sha256.Sum256(hashInput)
			reviewAnswer := answer
			for _, sensitive := range []string{os.Getenv("AEGISOPS_EVAL_API_KEY"), endpoint} {
				if sensitive != "" {
					reviewAnswer = strings.ReplaceAll(reviewAnswer, sensitive, "[REDACTED]")
				}
			}
			reviewAnswer = security.NewRedactor().Redact(reviewAnswer)
			record := baselineRecord{Case: fixture.ID, FixtureSHA256: hex.EncodeToString(digest[:]),
				Mode: mode, Model: recordedModel, Completed: err == nil && answer != "",
				EvidenceMarkers: markers, Requests: measured.calls, ToolCalls: tool.calls,
				Usage: usage, ElapsedMS: time.Since(start).Milliseconds(), ReviewAnswer: reviewAnswer}
			encoded, marshalErr := json.Marshal(record)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			t.Log(string(encoded))
			if err != nil {
				// Provider errors can echo endpoints or credentials. Log only the type.
				t.Fatalf("baseline did not complete (error type %s)", fmt.Sprintf("%T", err))
			}
			if !record.Completed || !markers || tool.calls == 0 {
				t.Fatal("baseline failed completion/evidence pre-screen; inspect the answer privately before judging diagnostic correctness")
			}
		})
	}
}
