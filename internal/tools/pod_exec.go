package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	"github.com/k8s-ai/k8s-ai/internal/executor"
	"github.com/k8s-ai/k8s-ai/internal/model"
)

// ExecutionContextPreparer binds an execution plan before confirmation and
// consumes the actual human answer immediately before dispatch.
type ExecutionContextPreparer interface {
	PrepareExecutionContext(context.Context, json.RawMessage, string, string) (context.Context, error)
	AuthorizeExecutionContext(context.Context, json.RawMessage) (context.Context, error)
}

type PreparedApprovalPlanner interface {
	PreparedApproval(context.Context, json.RawMessage) ApprovalPlan
}

func (t *podExecTool) PreparedApproval(ctx context.Context, input json.RawMessage) ApprovalPlan {
	if r, ok := t.runner.(interface {
		PreparedPodExec(context.Context) (cmdplan.PodExecRequest, bool)
	}); ok {
		if req, ok := r.PreparedPodExec(ctx); ok {
			return preparedPodExecApproval(ctx, t.runner, req)
		}
	}
	return t.PreviewApproval(input)
}
func (t *kubectlTool) PreparedApproval(ctx context.Context, input json.RawMessage) ApprovalPlan {
	if r, ok := t.runner.(interface {
		PreparedPodExec(context.Context) (cmdplan.PodExecRequest, bool)
	}); ok {
		if req, ok := r.PreparedPodExec(ctx); ok {
			return preparedPodExecApproval(ctx, t.runner, req)
		}
	}
	return t.PreviewApproval(input)
}

type PodExecRunner interface {
	PreparePodExec(context.Context, cmdplan.PodExecRequest, string, string) (context.Context, error)
	ApprovePodExec(context.Context) (context.Context, error)
	ExecPod(context.Context, cmdplan.PodExecRequest) (executor.Result, error)
}

func preparedPodExecApproval(ctx context.Context, runner any, req cmdplan.PodExecRequest) ApprovalPlan {
	plan := podExecApproval(req)
	status := "unknown"
	if r, ok := runner.(interface{ PodExecRBACStatus(context.Context) string }); ok {
		status = r.PodExecRBACStatus(ctx)
	}
	plan.Checks = []ApprovalCheck{{Name: "集群 RBAC", Status: status, Message: "审批前 pods/exec 预检；实际执行仍由 API Server 校验"}}
	return plan
}

type podExecTool struct{ runner PodExecRunner }

func NewPodExecTool(r PodExecRunner) Tool { return &podExecTool{r} }
func (t *podExecTool) Name() string       { return "exec_pod" }
func (t *podExecTool) Description() string {
	return "在当前绑定集群的明确 Pod/普通容器内执行有限非交互诊断，每次需要用户批准。支持 hostname/uptime/id/pwd、目录属性 ls/stat、df/du、有限字段 ps 和 ss/netstat；不支持文件内容、环境变量、解释器、脚本或任意网络请求。多容器必须指定 container。"
}
func (t *podExecTool) SchemaMetadata() string {
	return "[POD DIAGNOSTIC][RISK: HIGH][APPROVAL REQUIRED: YES]"
}
func (t *podExecTool) InputSchema() any {
	return map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"namespace": strProp("明确 namespace"), "pod": strProp("单个 Pod 名称"), "container": strProp("普通容器名称"), "argv": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "maxItems": 64}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 30}}, "required": []string{"namespace", "pod", "argv"}}
}
func (t *podExecTool) IsReadOnly(json.RawMessage) bool            { return false }
func (t *podExecTool) Risk(json.RawMessage) model.RiskLevel       { return model.RiskHigh }
func (t *podExecTool) RequiresHumanApproval(json.RawMessage) bool { return true }
func (t *podExecTool) PrepareExecutionContext(ctx context.Context, input json.RawMessage, user, session string) (context.Context, error) {
	r, e := cmdplan.DecodePodExec(input)
	if e != nil {
		return ctx, e
	}
	return t.runner.PreparePodExec(ctx, r, user, session)
}
func (t *podExecTool) AuthorizeExecutionContext(ctx context.Context, _ json.RawMessage) (context.Context, error) {
	return t.runner.ApprovePodExec(ctx)
}
func (t *podExecTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	r, e := cmdplan.DecodePodExec(input)
	if e != nil {
		return ExecutionError(e), nil
	}
	res, e := t.runner.ExecPod(ctx, r)
	return podExecResult(res, e), nil
}
func (t *podExecTool) PreviewApproval(input json.RawMessage) ApprovalPlan {
	r, e := cmdplan.DecodePodExec(input)
	if e != nil {
		return ApprovalPlan{Summary: e.Error()}
	}
	return podExecApproval(r)
}
func podExecApproval(r cmdplan.PodExecRequest) ApprovalPlan {
	return ApprovalPlan{Title: "容器诊断执行", Operation: "exec", Risk: string(model.RiskHigh), Target: ApprovalTarget{Namespace: r.Namespace, Kind: "Pod", Name: r.Pod}, Command: fmt.Sprintf("container=%s argv=%q", r.Container, r.Argv), Summary: fmt.Sprintf("非交互诊断；每次真人批准；超时 %d 秒；输出最多 64KiB。目标容器程序不由平台控制。", r.TimeoutSeconds)}
}
func podExecResult(res executor.Result, err error) ToolResult {
	if err != nil {
		r := ExecutionError(err)
		if res.Command != "" {
			r.Data = res
			r.Text = err.Error()
		}
		return r
	}
	return ToolResult{Text: extractKubectlSummary(res), Data: res, Truncated: res.Truncated}
}
func ExecutionError(err error) ToolResult {
	return ResultErrorWithCode(classifyToolError(err.Error()), err.Error())
}

func (t *kubectlTool) PrepareExecutionContext(ctx context.Context, input json.RawMessage, user, session string) (context.Context, error) {
	args, e := ParseArgs(input, "command")
	if e != nil {
		return ctx, e
	}
	r, matched, e := cmdplan.ParsePodExec(args["command"])
	if !matched {
		if runner, ok := t.runner.(interface {
			PrepareKubectl(context.Context, string) (context.Context, error)
		}); ok {
			return runner.PrepareKubectl(ctx, args["command"])
		}
		return ctx, nil
	}
	if e != nil {
		return ctx, e
	}
	runner, ok := t.runner.(PodExecRunner)
	if !ok {
		return ctx, fmt.Errorf("TOOL_UNAVAILABLE: Pod exec client is unavailable")
	}
	return runner.PreparePodExec(ctx, r, user, session)
}
func (t *kubectlTool) AuthorizeExecutionContext(ctx context.Context, input json.RawMessage) (context.Context, error) {
	args, e := ParseArgs(input, "command")
	if e != nil {
		return ctx, e
	}
	_, matched, _ := cmdplan.ParsePodExec(args["command"])
	if !matched {
		if runner, ok := t.runner.(interface {
			AuthorizeKubectl(context.Context, string) (context.Context, error)
		}); ok {
			return runner.AuthorizeKubectl(ctx, args["command"])
		}
		return ctx, nil
	}
	runner, ok := t.runner.(PodExecRunner)
	if !ok {
		return ctx, fmt.Errorf("TOOL_UNAVAILABLE: Pod exec client unavailable")
	}
	return runner.ApprovePodExec(ctx)
}
