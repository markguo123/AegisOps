package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// get_pod：单个 Pod 深查。
type podTool struct{ base }

func (t *podTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	if ns := args["namespace"]; ns != "" {
		pod, err := t.reader.GetPod(ctx, ns, args["name"])
		if err == nil {
			return podWithEvents(ctx, t.reader, pod), nil
		}
		// 精确名未命中（如用 Deployment 名查）时，在该 namespace 内按前缀回退。
		if !apierrors.IsNotFound(err) {
			return readError(err), nil
		}
		m, discoveryErr := prefixMatches(ctx, t.reader, ns, args["name"])
		if discoveryErr != nil {
			return readError(discoveryErr), nil
		}
		if len(m) > 0 {
			return podCandidates(m), nil
		}
		return readError(err), nil
	}
	// namespace 省略：全集群按名称精确查找；同名多个时返回候选，避免模型猜命名空间。
	matches, err := prefixMatches(ctx, t.reader, "", args["name"])
	if err != nil {
		return readError(err), nil
	}
	if len(matches) == 0 {
		return tools.ResultError("pod " + args["name"] + " not found in any namespace (exact or prefix)"), nil
	}
	switch len(matches) {
	case 1:
		if matches[0]["match"] != "exact" {
			return podCandidates(matches), nil
		}
		ns := matches[0]["namespace"].(string)
		podName := matches[0]["name"].(string)
		pod, err := t.reader.GetPod(ctx, ns, podName)
		if err != nil {
			return tools.ResultError(err.Error()), nil
		}
		return podWithEvents(ctx, t.reader, pod), nil
	default:
		return podCandidates(matches), nil
	}
}

func podCandidates(matches []map[string]any) tools.ToolResult {
	return tools.ToolResult{Data: map[string]any{
		"kind": "PodCandidates", "count": len(matches), "items": matches,
		"hint": "请确认实际 Pod 名称和命名空间",
	}, Text: fmt.Sprintf("%d Pod candidates", len(matches))}
}

// prefixMatches 在 namespace（空=全部）内按"精确名优先、前缀兜底"返回候选。
func prefixMatches(ctx context.Context, reader Reader, namespace, name string) ([]map[string]any, error) {
	pods, err := reader.ListPods(ctx, namespace)
	if err != nil {
		return nil, err
	}
	exact := make([]map[string]any, 0, 2)
	prefix := make([]map[string]any, 0, 4)
	for _, p := range pods {
		if p.Name == name {
			item := summarizePodListItem(p)
			item["match"] = "exact"
			exact = append(exact, item)
		} else if strings.HasPrefix(p.Name, name+"-") {
			item := summarizePodListItem(p)
			item["match"] = "name_prefix"
			prefix = append(prefix, item)
		}
	}
	if len(exact) > 0 {
		return exact, nil
	}
	return prefix, nil
}

// podWithEvents 在 Pod 摘要中内联最近 Events（describe 效果，减少 LLM 往返）。
func podWithEvents(ctx context.Context, reader Reader, pod *corev1.Pod) tools.ToolResult {
	data := summarizePod(pod)
	result := tools.ToolResult{Data: data, Text: fmt.Sprintf("pod %s/%s phase=%s", pod.Namespace, pod.Name, pod.Status.Phase)}
	if pod.UID == "" {
		data["partial"] = true
		data["eventsError"] = "Pod UID unavailable"
		return result
	}
	filter := eventFilter{kind: "Pod", name: pod.Name, uid: string(pod.UID)}
	events, coverageLimited, err := eventSample(ctx, reader, pod.Namespace, filter)
	if err != nil {
		data["partial"] = true
		data["eventsError"] = readError(err).ErrorCode
		return result
	}
	items, truncated := recentEvents(events, filter, 10)
	data["events"] = items
	data["eventsLimited"] = truncated
	data["eventCoverageLimited"] = coverageLimited
	if coverageLimited {
		data["partial"] = true
	}
	return result
}
