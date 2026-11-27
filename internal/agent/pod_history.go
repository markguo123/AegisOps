package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/llm"
	"github.com/k8s-ai/k8s-ai/internal/session"
)

func (a *Agent) podObservation(data any) *session.PodObservation {
	if a.AccessScope == nil || a.AccessScope.Policy == nil || a.AccessScope.Policy.ClusterID == "" {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil
	}
	var result struct {
		session.PodObservation
		Kind string `json:"kind"`
	}
	if json.Unmarshal(raw, &result) != nil || result.Kind != "Pod" ||
		result.Namespace == "" || result.Name == "" || result.UID == "" {
		return nil
	}
	result.Cluster = a.AccessScope.Policy.ClusterID
	result.TurnID = a.turn.ID
	return &result.PodObservation
}

func (a *Agent) podHistory(ctx context.Context, messages []session.Message) ([]session.Message, string) {
	if a.AccessScope == nil || a.AccessScope.Policy == nil {
		return messages, ""
	}
	type snapshot struct {
		session.PodObservation
		ObservedAt time.Time `json:"observedAt"`
		ToolCallID string    `json:"toolCallId,omitempty"`
	}
	history := make([]session.Message, 0, len(messages))
	for _, message := range messages {
		if message.Type != "pod-followup" && (message.Name != "get_pod" || message.Type != "tool-call-response") {
			history = append(history, message)
		}
	}
	var snapshots []snapshot
	seen := make(map[[2]string]bool)
	// Bound the amount of history scanned and the number of fresh scope checks.
	for i, checks := len(messages)-1, 0; i >= 0 && i >= len(messages)-200 && checks < 4; i-- {
		message := messages[i]
		pod := message.PodObservation
		if pod == nil || pod.Cluster == "" || pod.Cluster != a.AccessScope.Policy.ClusterID {
			continue
		}
		key := [2]string{pod.Namespace, pod.Name}
		if seen[key] {
			continue
		}
		seen[key] = true
		checks++
		if a.authorizePodObservation(ctx, pod) {
			snapshots = append(snapshots, snapshot{*pod, message.Timestamp, message.ToolCallID})
		}
	}
	if len(snapshots) == 0 {
		return history, podFollowupGuidance
	}
	raw, _ := json.Marshal(snapshots)
	var note strings.Builder
	note.WriteString("\n\n历史 Pod 状态快照（工具观测，仅适用于记录时间及 UID）：\n")
	note.Write(raw)
	note.WriteString("\n只将这些快照用于同一调查。waitingFor 表示上一轮等待用户补充的证据。用户补充重启或重建结果时，将反馈作为用户陈述，优先核对变化实例的 UID、状态或新日志时间窗口。仅凭 Pod 就绪不能确认原故障已恢复。")
	note.WriteString(podFollowupGuidance)
	return history, note.String()
}

const podFollowupGuidance = "\n需要结束本轮并等待用户补充已观测 Pod 的重启或原故障复测结果时，使用 ask_user_question 的 pod_followup 记录待验证项，然后在最终回复中说明等待事项。"

func (a *Agent) authorizePodObservation(ctx context.Context, pod *session.PodObservation) bool {
	if a.AccessScope == nil || a.AccessScope.Policy == nil || pod.Cluster != a.AccessScope.Policy.ClusterID {
		return false
	}
	args, _ := json.Marshal(map[string]string{"namespace": pod.Namespace, "name": pod.Name})
	return a.scopeCheck(ctx, llm.ToolCall{Name: "get_pod", Arguments: args}) == nil
}
