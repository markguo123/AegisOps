package k8s

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

func (*secretValueTool) OperationFacts(input json.RawMessage) (tools.OperationFacts, error) {
	args, err := parseSecretValueArgs(input)
	if err != nil {
		return tools.OperationFacts{}, err
	}
	facts := tools.OperationFacts{Targets: []authz.Target{
		{Namespace: args.Namespace, Resource: "/secrets", Actions: []string{authz.ActionSecretRead}},
	}}
	if args.Operation == "compare_config" {
		facts.Targets = append(facts.Targets,
			authz.Target{Namespace: args.Namespace, Resource: "/pods", Actions: []string{authz.ActionRead}},
			authz.Target{Namespace: args.Namespace, Resource: "/configmaps", Actions: []string{authz.ActionRead}})
	}
	return facts, nil
}

func (t *secretValueTool) compareConfig(ctx context.Context, args secretValueArgs) tools.ToolResult {
	result := func(status, explanation string) tools.ToolResult {
		return tools.ToolResult{Text: explanation, Data: map[string]any{
			"kind": "SecretConfigComparison", "status": status, "observedAt": time.Now().UTC(),
			"namespace": args.Namespace, "secretName": args.Name, "key": args.Keys[0],
			"pod": args.Pod, "podVersion": args.PodVersion, "container": args.Container,
			"configmap": args.ConfigMap, "configKey": args.ConfigKey, "configVersion": args.ConfigVersion,
		}}
	}
	if t.config == nil {
		return result("unknown", "配置比较通道未装配")
	}
	pod, err := t.config.GetPod(ctx, args.Namespace, args.Pod)
	if err != nil || pod.ResourceVersion != args.PodVersion {
		return result("unknown", "无法复核 Pod 版本，请重新观测")
	}
	var secretRef, configRef bool
	for _, container := range pod.Spec.Containers {
		if container.Name != args.Container {
			continue
		}
		for _, env := range container.Env {
			if env.ValueFrom == nil {
				continue
			}
			if ref := env.ValueFrom.SecretKeyRef; ref != nil && ref.Name == args.Name && ref.Key == args.Keys[0] {
				secretRef = true
			}
			if ref := env.ValueFrom.ConfigMapKeyRef; ref != nil && ref.Name == args.ConfigMap && ref.Key == args.ConfigKey {
				configRef = true
			}
		}
	}
	if !secretRef || !configRef {
		return result("unknown", "未确认同一容器引用了比较双方")
	}
	config, err := t.config.GetConfigMap(ctx, args.Namespace, args.ConfigMap)
	if err != nil || config.ResourceVersion != args.ConfigVersion {
		return result("unknown", "无法复核 ConfigMap 版本，请重新观测")
	}
	value, found := config.Data[args.ConfigKey]
	if !found {
		return result("missing", "ConfigMap 字段缺失")
	}
	if strings.Contains(value, "$") {
		return result("unknown", "配置包含未经解析的表达式")
	}
	values, err := t.reader.GetSecretValues(ctx, args.Namespace, args.Name, args.Keys)
	if err != nil {
		return result("unknown", "无法读取指定 Secret key")
	}
	secret, found := values[args.Keys[0]]
	if !found {
		return result("missing", "Secret key 缺失")
	}
	if bytes.Equal(secret, []byte(value)) {
		return result("equal", "采集时两个配置字段的值一致")
	}
	return result("different", "采集时两个配置字段的值不一致")
}
