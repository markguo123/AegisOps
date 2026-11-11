// Large-result previews and on-demand reads borrow HolmesGPT's
// tool_context_window_limiter.py at 27c623ee3ab074734cf0d72964bafd11245b5f80
// (Apache-2.0). AegisOps uses run-bound opaque references instead of shell paths.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/objectstore"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// ResultStore reuses the existing object gateway without exposing storage paths
// or credentials to the model. No new durable session/source of truth is added.
type ResultStore interface {
	Put(context.Context, objectstore.Spec, io.Reader) (objectstore.Ref, error)
	Get(context.Context, string) (io.ReadCloser, objectstore.Ref, error)
}

const resultCacheBytes = 2 << 20
const resultObjectBytes = 1 << 20
const resultPageBytes = 4096

type resultEntry struct {
	text                    string
	key                     string
	size                    int
	digest                  [32]byte
	source                  llm.ToolCall
	session, owner, cluster string
}

type resultCache struct {
	agent     *Agent
	entries   map[string]resultEntry
	bytes     int
	readBytes int
}

func (*resultCache) Name() string { return "read_tool_result" }
func (*resultCache) Description() string {
	return "Read a UTF-8 byte page from an oversized tool result in this investigation. Use only a returned result_id and next_offset. No file paths or shell. References expire when this turn ends."
}
func (*resultCache) InputSchema() any {
	return map[string]any{
		"type": "object", "properties": map[string]any{
			"result_id": map[string]any{"type": "string"},
			"offset":    map[string]any{"type": "integer", "minimum": 0},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": resultPageBytes},
		}, "required": []string{"result_id"}, "additionalProperties": false,
	}
}
func (*resultCache) IsReadOnly(json.RawMessage) bool      { return true }
func (*resultCache) Risk(json.RawMessage) model.RiskLevel { return model.RiskLow }

func (c *resultCache) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var args struct {
		ID     string `json:"result_id"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	d := json.NewDecoder(strings.NewReader(string(input)))
	d.DisallowUnknownFields()
	if err := d.Decode(&args); err != nil || args.ID == "" || args.Offset < 0 || args.Limit < 0 || args.Limit > resultPageBytes {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "invalid result_id, offset or limit"), nil
	}
	if d.Decode(new(any)) != io.EOF {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "expected one argument object"), nil
	}
	if err := ctx.Err(); err != nil {
		return tools.ToolResult{}, err
	}
	if args.Limit == 0 {
		args.Limit = resultPageBytes
	}
	e, ok := c.entries[args.ID]
	a := c.agent
	if !ok || a.Session == nil || e.session != a.Session.ID || e.owner != a.Session.UserID || e.cluster != a.Session.Cluster || a.hasSensitiveValues() {
		return tools.ResultErrorWithCode(tools.ErrorNotFound, "result reference unavailable in this investigation"), nil
	}
	// Recheck the ORIGINAL resource and permissions, not just a local-reader name.
	if err := a.scopeCheck(ctx, e.source); err != nil {
		return tools.ResultErrorWithCode(tools.ErrorScopeDenied, "source resource access is no longer valid"), nil
	}
	if args.Offset > e.size || c.readBytes+args.Limit > resultCacheBytes {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "result offset or total read budget exceeded"), nil
	}
	text := e.text
	if e.key != "" {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		r, _, err := a.ResultStore.Get(readCtx, e.key)
		if err != nil {
			return tools.ResultErrorWithCode(tools.ErrorToolUnavailable, "stored result unavailable; narrow the original query"), nil
		}
		defer r.Close()
		raw, err := io.ReadAll(io.LimitReader(r, resultObjectBytes+1))
		if err != nil || len(raw) != e.size || sha256.Sum256(raw) != e.digest {
			return tools.ResultErrorWithCode(tools.ErrorToolUnavailable, "stored result size mismatch"), nil
		}
		text = string(raw)
	}
	if args.Offset < len(text) && !utf8.RuneStart(text[args.Offset]) {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "offset must start at a UTF-8 character boundary"), nil
	}
	end := min(args.Offset+args.Limit, len(text))
	for end > args.Offset && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	if end == args.Offset && end < len(text) {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "limit too small for the next UTF-8 character"), nil
	}
	c.readBytes += end - args.Offset
	return tools.ToolResult{Data: map[string]any{"result_id": args.ID, "offset": args.Offset, "next_offset": end, "total_bytes": len(text), "eof": end == len(text), "content": text[args.Offset:end]}}, nil
}

func (a *Agent) resultForModel(ctx context.Context, call llm.ToolCall, tr tools.ToolResult) string {
	preview, truncated := tools.MarshalForLLM(tr, a.ResultMaxBytes)
	// Never spill a sensitive turn or spill the reader itself into a new result.
	if !truncated || tr.ModelOnly != nil || a.hasSensitiveValues() || a.resultCache == nil || call.Name == "read_tool_result" {
		return preview
	}
	full, fullTruncated := tools.MarshalSafe(tr, resultObjectBytes)
	full = a.redactSensitive(full)
	if fullTruncated || len(full) > resultObjectBytes || a.resultCache.bytes+len(full) > resultCacheBytes {
		return preview + "\nResult storage budget exceeded; narrow the original query."
	}
	id := newID()
	e := resultEntry{text: full, size: len(full), digest: sha256.Sum256([]byte(full)), source: call, session: a.Session.ID, owner: a.Session.UserID, cluster: a.Session.Cluster}
	// Store only safe data. Orphan objects remain subject to the existing GC;
	// the run's allow-list is the only authority to retrieve one of these objects.
	if a.ResultStore != nil {
		storeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		ref, err := a.ResultStore.Put(storeCtx, objectstore.Spec{Category: objectstore.CategoryExports,
			Scope: []string{a.ID}, Ext: ".txt", MediaType: "text/plain", CreatedBy: a.Session.UserID}, strings.NewReader(full))
		if err == nil {
			e.key = ref.Key
			e.text = ""
		}
	}
	a.resultCache.entries[id] = e
	a.resultCache.bytes += len(full)
	return preview + fmt.Sprintf("\nOversized result: result_id=%s total_bytes=%d. Use read_tool_result with offset=0 and limit<=%d; never use shell or file paths.", id, len(full), resultPageBytes)
}
