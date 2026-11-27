package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/security"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

var variableReference = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)(?:(?::)?[-=?+][^{}\r\n]*)?\}$`)
var configAssignment = regexp.MustCompile(`^[ \t]*([A-Za-z_][A-Za-z0-9_.-]*)[ \t]*[:=][ \t]*(.*)$`)

func safeConfigData(data map[string]string) (map[string]string, []map[string]string) {
	redactor := security.NewRedactor()
	safe := make(map[string]string, len(data))
	var references []map[string]string
	for key, value := range data {
		if match := variableReference.FindStringSubmatch(strings.TrimSpace(value)); match != nil {
			references = append(references, map[string]string{"dataKey": key, "variable": match[1]})
			safe[key] = "[REFERENCE]"
			continue
		}
		lines := strings.Split(value, "\n")
		for i, line := range lines {
			field := configAssignment.FindStringSubmatch(line)
			if field == nil {
				if strings.Contains(line, "${") {
					lines[i] = "[REDACTED]"
				}
				continue
			}
			if match := variableReference.FindStringSubmatch(strings.TrimSpace(field[2])); match != nil {
				references = append(references, map[string]string{"dataKey": key, "field": field[1], "variable": match[1]})
				lines[i] = field[1] + "=[REFERENCE]"
			} else if strings.Contains(field[2], "${") || redactor.RedactValueByKey(field[1], field[2]) == "[REDACTED]" {
				lines[i] = field[1] + "=[REDACTED]"
			}
		}
		safe[key] = redactor.RedactValueByKey(key, strings.Join(lines, "\n"))
	}
	return safe, references
}

// get_configmap：读取单个 ConfigMap（诊断配置类启动失败/崩溃）。
type configMapTool struct{ base }

func (t *configMapTool) Name() string { return "get_configmap" }
func (t *configMapTool) Description() string {
	return "读取 ConfigMap 的 data（配置内容，进入 LLM 前脱敏）。Pod 启动失败/崩溃时先看它挂载的 ConfigMap 是否有配置错误。"
}
func (t *configMapTool) InputSchema() any {
	return objSchema(map[string]any{
		"namespace": strProp("命名空间"),
		"name":      strProp("ConfigMap 名称"),
	}, "namespace", "name")
}
func (t *configMapTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace", "name")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	cm, err := t.reader.GetConfigMap(ctx, args["namespace"], args["name"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	data, references := safeConfigData(cm.Data)
	return tools.ToolResult{
		Data: map[string]any{
			"kind": "ConfigMap", "namespace": cm.Namespace, "name": cm.Name,
			"resourceVersion": cm.ResourceVersion,
			"data":            data, "variableReferences": references, "binaryDataKeys": keys(cm.BinaryData),
		},
		Text: fmt.Sprintf("configmap %s/%s", cm.Namespace, cm.Name),
	}, nil
}

// get_configmaps：按命名空间列出 ConfigMap 名称。
type configMapsTool struct{ base }

func (t *configMapsTool) Name() string { return "get_configmaps" }
func (t *configMapsTool) Description() string {
	return "列出命名空间内的 ConfigMap 名称，便于定位 Pod 挂载的配置。"
}
func (t *configMapsTool) InputSchema() any {
	return objSchema(map[string]any{
		"namespace": strProp("命名空间"),
	}, "namespace")
}
func (t *configMapsTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	args, err := tools.ParseArgs(input, "namespace")
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	list, err := t.reader.ListConfigMaps(ctx, args["namespace"])
	if err != nil {
		return tools.ResultError(err.Error()), nil
	}
	names := make([]string, 0, len(list))
	for _, cm := range list {
		names = append(names, cm.Name)
	}
	return tools.ToolResult{
		Data: map[string]any{"kind": "ConfigMapList", "namespace": args["namespace"], "count": len(names), "names": names},
		Text: fmt.Sprintf("configmaps=%d", len(names)),
	}, nil
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
