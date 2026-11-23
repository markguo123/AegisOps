package approval

import "github.com/k8s-ai/k8s-ai/internal/model"

// Mode selects how approval decisions are made. strict preserves the
// v1 behavior; smart is the Phase C observation mode that still requires
// first-time approval and never enables Grant auto-approval yet.
type Mode string

const (
	ModeStrict Mode = "strict"
	ModeSmart  Mode = "smart"
)

// Decision is the normalized approval outcome used by the Agent loop.
// Code Guard in the executor remains the final authority for DENY; this
// decision only determines whether the loop needs a human approver.
type Decision string

const (
	DecisionAuto            Decision = "auto"
	DecisionRequireApproval Decision = "require_approval"
	DecisionDeny            Decision = "deny"
	// DecisionSensitiveAuto 是「管理员显式授权」的敏感读取自动放行
	// （agent.secret.read_approval=auto）。它与普通写操作的免审不同：
	// 调用方必须仍然标记该调用为敏感读取，并保留 secret.read 权限、
	// 目标绑定、ModelOnly 通道与全部非模型出口脱敏。
	DecisionSensitiveAuto Decision = "auto_sensitive"
)

// Input is the minimal per-call context the policy needs. Command/risk
// strings are intentionally omitted so approval decisions never log raw
// command text through the audit path.
type Input struct {
	ReadOnly bool
	Risk     model.RiskLevel
	Shell    bool
	// RequireHuman marks sensitive operations that can never be skipped by
	// automation flags (for example non-interactive exec or Secret reads).
	RequireHuman bool
	// SensitiveAuto marks an explicitly configured sensitive-read consent
	// (agent.secret.read_approval=auto). 它是配置层的显式授权，不是普通写操作
	// 免审：调用方仍须完成 secret.read / 目标绑定校验，且该授权不能由
	// --skip-permissions、require_approval=false 或普通授权缓存替代。
	SensitiveAuto bool
	// Denied 是 Code Guard 的硬拒绝结论（kind 黑名单、未 dry run、命名空间缺失等）。
	// 它优先级最高：跳过 SkipPermissions 与 RequireApproval=false，因为这两条
	// 自动化通道只免除「人工确认」，不能免除代码级门禁。
	Denied bool
}

// Evaluate normalizes the v1 behavior into a strict ApprovalPolicy.
//
// RequireHuman (sensitive reads in always mode, non-interactive exec) and
// Shell (未证明只读的 shell 构造) are gated before skip_permissions /
// require_approval=false, matching AGENTS.md: they require a real human
// approver and are never covered by non-interactive automation.
//
// SensitiveAuto 是 read_approval=auto 的显式配置授权：只改变是否需要逐次
// 弹窗，调用方仍必须完成 secret.read、目标绑定与出口脱敏，因此返回独立的
// DecisionSensitiveAuto 而不是普通 AUTO。
func (p Policy) Evaluate(in Input) Decision {
	if p.Mode == "" {
		p.Mode = ModeStrict
	}
	if in.Denied {
		return DecisionDeny
	}
	if in.RequireHuman {
		return DecisionRequireApproval
	}
	if in.Shell {
		return DecisionRequireApproval
	}
	// 敏感读取的显式配置授权：先于普通只读判断，得到独立审计结论，
	// 避免把敏感读取混进「普通只读自动执行」。
	if in.SensitiveAuto {
		return DecisionSensitiveAuto
	}
	if in.ReadOnly {
		return DecisionAuto
	}
	// strict 与 smart 在本函数里行为一致：首次 MEDIUM/HIGH/CRITICAL 一律
	// REQUIRE_APPROVAL，模式只进入审计用于观测。真正的放行来自 Agent 循环里叠加的
	// Authorization Grant 命中判断（见 Agent.approvalDecisionDetail）：Grant 只会由
	// 用户选「确认，本次不再询问」在 LOW/MEDIUM 风险时产生，HIGH 与 shell 永不命中。
	// skip/require_approval=false 只免除普通非 shell 写操作的人工确认（legacy 自动化
	// 入口），既越不过 shell 闸门，也越不过 Code Guard 的 Denied。
	if p.SkipPermissions {
		return DecisionAuto
	}
	if !p.RequireApproval {
		// Legacy require_approval=false: plain write operations may auto
		// execute, but shell commands above never can.
		return DecisionAuto
	}
	return DecisionRequireApproval
}
