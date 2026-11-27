// Package k8s 实现二期第一批只读 Kubernetes Tools（复用一期 kubernetes/scanner
// 只读门面，不重复实现采集逻辑）。
package k8s

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/model"
	"github.com/k8s-ai/k8s-ai/internal/scanner"
	"github.com/k8s-ai/k8s-ai/internal/tools"
)

// Reader 是只读 Tool 依赖的 Kubernetes 只读门面（scanner.Reader）。
type Reader = scanner.Reader

// base 提供只读工具公共行为（全部只读、风险 SAFE）。
type base struct {
	reader Reader
}

func (b *base) IsReadOnly(json.RawMessage) bool { return true }
func (b *base) Risk(json.RawMessage) model.RiskLevel {
	return model.RiskSafe
}

func (b *base) SchemaMetadata() string {
	return "[READ ONLY][RISK: SAFE][APPROVAL REQUIRED: NO]"
}

// RegisterOptions 控制按部署配置选择性注册的工具。
type RegisterOptions struct {
	// SecretAccess 对应 agent.secret.access；只有 read_on_demand 注册
	// get_secret_value，off/create_only 均不暴露读取工具。
	SecretAccess string
	Cluster      string
	Disabled     map[string]bool
}

// RegisterAll 把全部只读工具注册进注册表。
func RegisterAll(reg *tools.Registry, r Reader) error {
	return RegisterAllWithOptions(reg, r, RegisterOptions{})
}

// RegisterAllWithOptions 按运行配置注册只读工具及可选的敏感读取工具。
// get_secret_info（Secret 元数据/键名）始终注册：它是普通 read 权限，
// 与 access/read_approval 无关；get_secret_value 只在 access=read_on_demand 时注册。
func RegisterAllWithOptions(reg *tools.Registry, r Reader, opts RegisterOptions) error {
	if err := registerResourceCatalog(reg, r, opts.Cluster, opts.Disabled); err != nil {
		return err
	}
	b := base{reader: r}
	all := []tools.Tool{&eventsTool{base: b}, &secretInfoTool{base: b}}
	if metrics, ok := r.(MetricsReader); ok {
		all = append(all, &nodeUsageTool{base: b, metrics: metrics})
	}
	all = append(all, &logsTool{base: b, previousDisabled: opts.Disabled["get_previous_logs"]})
	if strings.EqualFold(strings.TrimSpace(opts.SecretAccess), "read_on_demand") {
		if secretReader, ok := r.(SecretValuesReader); ok {
			all = append(all, &secretValueTool{
				reader: secretReader,
				config: r,
			})
		}
	}
	for _, t := range all {
		if opts.Disabled[t.Name()] {
			continue
		}
		if err := reg.Register(t); err != nil {
			return fmt.Errorf("register tool %s: %w", t.Name(), err)
		}
	}
	return nil
}

// schema helpers：JSON Schema 构建。
func objSchema(props map[string]any, required ...string) map[string]any {
	req := make([]any, 0, len(required))
	for _, r := range required {
		req = append(req, r)
	}
	return map[string]any{"type": "object", "properties": props, "required": req}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
