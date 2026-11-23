package tools

import "encoding/json"

// ShellSyntaxAware is an optional interface for tools that can express
// shell constructs (currently kubectl). The Agent loop uses it as the
// approval shell gate: 未能证明只读的 shell 构造永不自动执行
// （--skip-permissions 与 require_approval=false 都不免除人工审批）。
// 可证明只读的管道组合不属于此列：它们由执行器改走受控 argv 管道。
type ShellSyntaxAware interface {
	HasShellSyntax(input json.RawMessage) bool
}

// SensitiveReadAware 是可选接口：敏感读取工具（当前为 get_secret_value）
// 报告管理员是否已通过配置显式授权免逐次审批（agent.secret.read_approval=auto）。
// 返回 true 时 Agent 使用独立的「敏感读取自动放行」决策（DecisionSensitiveAuto），
// 而不是把敏感读取伪装成普通只读：secret.read 权限、目标绑定、ModelOnly 通道
// 与全部非模型出口脱敏必须保持不变。
type SensitiveReadAware interface {
	SensitiveReadAuto(input json.RawMessage) bool
}
