package agent

import (
	"encoding/json"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func normalizedFailureKey(call llm.ToolCall) string {
	var value any
	if json.Unmarshal(call.Arguments, &value) == nil {
		if normalized, err := json.Marshal(value); err == nil {
			return call.Name + "\x00" + string(normalized)
		}
	}
	return toolCallFailureKey(call)
}

func failureFeedback(tr tools.ToolResult, readonly bool) string {
	p := tools.Present(tr)
	if p.Ok {
		return ""
	}
	switch p.ErrorCode {
	case tools.ErrorScopeDenied, tools.ErrorRBACDenied, tools.ErrorAuthentication:
		return "Permission denied: do not retry or switch tools to bypass authorization. Ask for the required access."
	case tools.ErrorInvalidArgument:
		return "Invalid arguments: correct them using the tool schema and observed resource identity; do not invent a target."
	case tools.ErrorNotFound:
		return "Resource not found: verify its exact name, namespace and cluster with an authorized read; do not invent it."
	case tools.ErrorTimeout, tools.ErrorClusterUnavailable:
		if readonly {
			return "Transient read failure: at most one identical retry is allowed; stop if it fails again."
		}
		return "Write outcome UNKNOWN: do not replay this write. Reconcile current resource state using read-only tools or ask a human to verify before proposing a new operation."
	default:
		if !readonly && !strings.Contains(strings.ToLower(tr.Text), "declin") {
			return "Write failed or outcome unknown: do not replay. Verify the actual resource state with an authorized read before any further change."
		}
		return "Do not repeat the identical failed or declined call; change the investigation using observed evidence."
	}
}
