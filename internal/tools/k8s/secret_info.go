package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// secretInfoTool 是 Secret 元数据查询（方案 24 §4.2）：只输出字段白名单
// （名称、类型、immutable、键名），不输出 data/stringData、annotations、
// last-applied 配置或任何可能携带明文的字段。
//
// 它使用普通 read 权限：不要求 secret.read、不弹敏感读取审批，也不依赖
// access=read_on_demand。off 关闭的是 Secret 创建/明文能力，不是资源存在性信息。
type secretInfoTool struct{ base }

type secretInfoArgs struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// SecretInfoItem 是单个 Secret 的元数据白名单输出。
type SecretInfoItem struct {
	Name      string   `json:"name"`
	Type      string   `json:"type,omitempty"`
	Immutable bool     `json:"immutable"`
	Keys      []string `json:"keys"`
}

func (t *secretInfoTool) Name() string { return "get_secret_info" }

func (t *secretInfoTool) Description() string {
	return "查询 Secret 的元数据与键名（名称、类型、immutable、键名），不读取任何值。" +
		"namespace 必填，name 可选（填写时只返回该 Secret）。" +
		"这是普通读取权限，不需要敏感读取审批；先用它发现相关 Secret 与 key，" +
		"确需查看明文时再按需调用 get_secret_value。"
}

func (t *secretInfoTool) SchemaMetadata() string {
	return "[READ ONLY][RISK: SAFE][APPROVAL REQUIRED: NO][NO SECRET VALUES]"
}

func (t *secretInfoTool) InputSchema() any {
	return objSchema(map[string]any{
		"namespace": strProp("Secret 所在命名空间"),
		"name":      strProp("可选：只查询指定名称的 Secret"),
	}, "namespace")
}

func (t *secretInfoTool) Execute(ctx context.Context, input json.RawMessage) (tools.ToolResult, error) {
	var args secretInfoArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "invalid arguments: "+err.Error()), nil
	}
	args.Namespace = strings.TrimSpace(args.Namespace)
	args.Name = strings.TrimSpace(args.Name)
	if args.Namespace == "" {
		return tools.ResultErrorWithCode(tools.ErrorInvalidArgument, "namespace is required"), nil
	}
	metas, err := t.reader.ListSecretMeta(ctx, args.Namespace)
	if err != nil {
		return tools.ResultError(fmt.Sprintf("list secret metadata in %s: %v", args.Namespace, err)), nil
	}
	items := make([]SecretInfoItem, 0, len(metas))
	for _, meta := range metas {
		if args.Name != "" && meta.Ref.Name != args.Name {
			continue
		}
		items = append(items, SecretInfoItem{
			Name:      meta.Ref.Name,
			Type:      meta.Type,
			Immutable: meta.Immutable,
			Keys:      append([]string(nil), meta.Keys...),
		})
	}
	if args.Name != "" && len(items) == 0 {
		return tools.ResultErrorWithCode(tools.ErrorNotFound,
			fmt.Sprintf("Secret %s/%s not found or not readable", args.Namespace, args.Name)), nil
	}
	data := map[string]any{
		"kind":           "SecretInfoResult",
		"namespace":      args.Namespace,
		"secrets":        items,
		"count":          len(items),
		"valuesIncluded": false,
	}
	return tools.ToolResult{
		Data: data,
		Text: fmt.Sprintf("Secret 元数据：%s 下 %d 个 Secret（不含任何值）", args.Namespace, len(items)),
	}, nil
}
