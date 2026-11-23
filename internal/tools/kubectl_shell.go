package tools

import (
	"encoding/json"

	"github.com/k8s-ai/k8s-ai/internal/approval"
)

// HasShellSyntax reports whether the kubectl command in the input must be gated
// by a human approver because of shell constructs. 可证明只读的管道组合
// （如 kubectl get po -A | grep mysql）由执行器改走受控 argv 管道，
// 不属于"必须审批的 shell"，因此这里返回 false；其余 shell 构造保持二期行为：
// 一律真人审批，--skip-permissions 与 require_approval=false 不能绕过。
func (t *kubectlTool) HasShellSyntax(input json.RawMessage) bool {
	args, err := ParseArgs(input, "command")
	if err != nil {
		return false
	}
	return approval.NeedsShellApproval(args["command"])
}
