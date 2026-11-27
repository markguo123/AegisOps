package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/k8s-ai/k8s-ai/internal/model"
)

// ScanRunner 是一期 ScanService 的最小消费接口（service 包实现，避免 tools→service 依赖环）。
type ScanRunner interface {
	Run(ctx context.Context, opts model.ScanOptions) (*model.ScanResult, error)
}

// NewDiagnosisTools builds the canonical scan and object diagnosis entries.
func NewDiagnosisTools(runner ScanRunner, baseOpts model.ScanOptions, disabled map[string]bool) []Tool {
	base := scanToolBase{runner: runner, baseOpts: baseOpts}
	var out []Tool
	if !disabled["scan_cluster"] {
		out = append(out, &scanClusterTool{scanToolBase: base})
	}
	if disabled["diagnose_resource"] {
		return out
	}
	var routes []KindRoute
	namespaced := Capability{Domain: "kubernetes", Action: "read"}
	if !disabled["diagnose_pod"] || !disabled["scan_pod"] {
		routes = append(routes, KindRoute{Kind: "Pod", Execute: (&diagnosePodTool{scanToolBase: base}).Execute, Capability: namespaced, NamespaceRequired: true})
	}
	if !disabled["diagnose_node"] {
		routes = append(routes, KindRoute{Kind: "Node", Execute: (&diagnoseNodeTool{scanToolBase: base}).Execute, Capability: Capability{Domain: "kubernetes", Resource: "/nodes", ClusterRead: true}, ClusterScoped: true})
	}
	if !disabled["diagnose_workload"] {
		workload := &diagnoseWorkloadTool{scanToolBase: base}
		for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet"} {
			routes = append(routes, KindRoute{Kind: kind, Execute: workload.Execute, Capability: namespaced, NamespaceRequired: true})
		}
	}
	if len(routes) == 0 {
		return out
	}
	return append(out, NewKindTool("diagnose_resource", "Gather rule-based evidence for a specific unhealthy object. Use targeted logs or events to fill evidence gaps.", routes))
}

type scanToolBase struct {
	runner   ScanRunner
	baseOpts model.ScanOptions
}

func (b *scanToolBase) IsReadOnly(json.RawMessage) bool { return true }
func (b *scanToolBase) Risk(json.RawMessage) model.RiskLevel {
	return model.RiskSafe
}

// summarize 给诊断子调用结果补上绑定 ClusterID（P12.2）：会话已固定目标，
// 证据与报告必须能追溯到真实集群，而不是只靠一次运行的内存上下文。
func (b *scanToolBase) summarize(result *model.ScanResult) map[string]any {
	data := summarizeScanResult(result)
	if id := b.baseOpts.Attribution.ClusterID; id != "" {
		data["clusterId"] = id
	}
	return data
}

// scanClusterTool：全集群/命名空间体检（规则级，快；LLM 诊断由 Agent 后续分析）。
type scanClusterTool struct{ scanToolBase }

func (t *scanClusterTool) Name() string { return "scan_cluster" }
func (t *scanClusterTool) Description() string {
	return "对集群（或指定命名空间）执行规则体检：复用 Scanner+规则引擎+Incident+历史对比，返回问题清单与健康评分。适合『检查集群有没有异常』类问题。"
}
func (t *scanClusterTool) InputSchema() any {
	return objSchema(map[string]any{"namespace": strProp("命名空间（可选，省略=全集群）")})
}
func (t *scanClusterTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	args, err := ParseArgs(input)
	if err != nil {
		return ResultError(err.Error()), nil
	}
	opts := t.baseOpts
	opts.Namespace = args["namespace"]
	opts.PodTarget = ""
	opts.LLM.Enabled = false // 规则级体检保持快速，LLM 分析由 Agent 负责
	opts.ReportMode = "none"
	result, err := t.runner.Run(ctx, opts)
	if err != nil {
		return ResultError("scan_cluster: " + err.Error()), nil
	}
	return ToolResult{Data: t.summarize(result)}, nil
}

// diagnosePodTool：单 Pod 深度证据采集（规则级；LLM 分析由 Agent 负责，
// 工具内部不再跑诊断 LLM，避免双重 LLM 拖慢与信息损耗）。
type diagnosePodTool struct{ scanToolBase }

func (t *diagnosePodTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	args, err := ParseArgs(input, "namespace", "name")
	if err != nil {
		return ResultError(err.Error()), nil
	}
	opts := t.baseOpts
	opts.Namespace = args["namespace"]
	opts.PodTarget = args["name"]
	opts.LLM.Enabled = false // Agent 是唯一大脑：证据采集秒级返回，分析由 Agent LLM 完成
	opts.ReportMode = "none"
	result, err := t.runner.Run(ctx, opts)
	if err != nil {
		return ResultError("diagnose_resource (Pod): " + err.Error()), nil
	}
	return ToolResult{Data: t.summarize(result)}, nil
}

// diagnoseNodeTool：Node 深度证据采集（按 Node 过滤一期结果；LLM 分析由 Agent 负责）。
type diagnoseNodeTool struct{ scanToolBase }

func (t *diagnoseNodeTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	args, err := ParseArgs(input, "name")
	if err != nil {
		return ResultError(err.Error()), nil
	}
	opts := t.baseOpts
	opts.Namespace = ""
	opts.DiagnosticTarget = &model.ResourceRef{Kind: "Node", Name: args["name"]}
	opts.LLM.Enabled = false // Agent 是唯一大脑：证据采集秒级返回，分析由 Agent LLM 完成
	opts.ReportMode = "none"
	result, err := t.runner.Run(ctx, opts)
	if err != nil {
		return ResultError("diagnose_resource (Node): " + err.Error()), nil
	}
	filtered := filterFindings(result, func(r model.ResourceRef) bool {
		return strings.EqualFold(r.Kind, "Node") && r.Name == args["name"]
	})
	return ToolResult{Data: diagnosticSummary(&t.scanToolBase, filtered, opts.DiagnosticTarget)}, nil
}

// diagnoseWorkloadTool：Workload 深度证据采集（Deployment/StatefulSet/DaemonSet
// 及其 Pod 链；LLM 分析由 Agent 负责）。
type diagnoseWorkloadTool struct{ scanToolBase }

func (t *diagnoseWorkloadTool) Execute(ctx context.Context, input json.RawMessage) (ToolResult, error) {
	args, err := ParseArgs(input, "kind", "namespace", "name")
	if err != nil {
		return ResultError(err.Error()), nil
	}
	wantKind := args["kind"]
	opts := t.baseOpts
	opts.Namespace = args["namespace"]
	opts.DiagnosticTarget = &model.ResourceRef{Kind: wantKind, Namespace: opts.Namespace, Name: args["name"]}
	opts.LLM.Enabled = false // Agent 是唯一大脑：证据采集秒级返回，分析由 Agent LLM 完成
	opts.ReportMode = "none"
	result, err := t.runner.Run(ctx, opts)
	if err != nil {
		return ResultError("diagnose_resource: " + err.Error()), nil
	}
	filtered := filterFindings(result, func(r model.ResourceRef) bool {
		if strings.EqualFold(r.Kind, wantKind) && r.Namespace == args["namespace"] && r.Name == args["name"] {
			return true
		}
		if result != nil && r.UID != "" {
			for _, subject := range result.Meta.DiagnosticSubjects {
				if subject.UID == r.UID {
					return true
				}
			}
		}
		return false
	})
	return ToolResult{Data: diagnosticSummary(&t.scanToolBase, filtered, opts.DiagnosticTarget)}, nil
}

func diagnosticSummary(tool *scanToolBase, result *model.ScanResult, target *model.ResourceRef) map[string]any {
	data := tool.summarize(result)
	data["target"] = target
	if result.Meta.DiagnosticTarget != nil {
		data["target"] = result.Meta.DiagnosticTarget
		data["coverageScope"] = result.Meta.DiagnosticTarget
		data["relatedSubjects"] = result.Meta.DiagnosticSubjects
	}
	for _, key := range []string{"summary", "healthScore", "healthScoreScope", "history", "components"} {
		delete(data, key)
	}
	return data
}

// filterFindings 只保留 Resource 或 Related 命中匹配器的 Finding（及其诊断）。
func filterFindings(result *model.ScanResult, match func(model.ResourceRef) bool) *model.ScanResult {
	if result == nil {
		return &model.ScanResult{}
	}
	keep := make([]model.Finding, 0, len(result.Findings))
	keepIDs := map[string]bool{}
	for _, f := range result.Findings {
		if match(f.Resource) {
			keep = append(keep, f)
			keepIDs[f.ID] = true
			continue
		}
		for _, r := range f.Related {
			if match(r) {
				keep = append(keep, f)
				keepIDs[f.ID] = true
				break
			}
		}
	}
	diags := make([]model.Diagnosis, 0, len(result.Diagnoses))
	for _, d := range result.Diagnoses {
		if keepIDs[d.FindingID] {
			diags = append(diags, d)
		}
	}
	out := *result
	out.Findings = keep
	out.Diagnoses = diags
	return &out
}

// summarizeScanResult 把一期 ScanResult 转成紧凑视图（LLM 上下文友好）。
func summarizeScanResult(result *model.ScanResult) map[string]any {
	if result == nil {
		return map[string]any{"kind": "ScanResult", "error": "no result"}
	}
	findings := make([]map[string]any, 0, len(result.Findings))
	for _, f := range result.Findings {
		findings = append(findings, map[string]any{
			"id":         f.ID,
			"rule":       f.Rule,
			"severity":   f.Severity,
			"title":      f.Title,
			"summary":    f.Summary,
			"resource":   map[string]any{"kind": f.Resource.Kind, "namespace": f.Resource.Namespace, "name": f.Resource.Name, "uid": f.Resource.UID},
			"correlated": f.Correlated,
			"related":    f.Related,
		})
	}
	diagnoses := make([]map[string]any, 0, len(result.Diagnoses))
	for _, d := range result.Diagnoses {
		diagnoses = append(diagnoses, map[string]any{
			"findingId":       d.FindingID,
			"summary":         d.Summary,
			"rootCause":       d.RootCause,
			"confidence":      d.Confidence,
			"confidenceLevel": d.ConfidenceLevel,
			"evidenceChain":   d.EvidenceChain,
			"remediation":     d.Remediation,
			"llmUsed":         d.LLMUsed,
		})
	}
	history := map[string]any{"added": 0, "continued": 0, "recovered": 0}
	if result.History != nil {
		history["added"] = len(result.History.Added)
		history["continued"] = len(result.History.Continued)
		history["recovered"] = len(result.History.Recovered)
	}
	components := make([]map[string]any, 0, len(result.Components))
	for _, c := range result.Components {
		components = append(components, map[string]any{
			"name":    c.Name,
			"present": c.Present,
			"ready":   c.Ready,
			"detail":  c.Detail,
		})
	}
	return map[string]any{
		"kind":             "ScanResult",
		"cluster":          result.Meta.Cluster,
		"namespace":        result.Meta.Namespace,
		"summary":          result.Summary,
		"healthScore":      result.HealthScore.Score,
		"findings":         findings,
		"diagnoses":        diagnoses,
		"history":          history,
		"components":       components,
		"collectionErrors": len(result.CollectionErrors),
		"partial":          len(result.CollectionErrors) > 0 || !result.Collection.Complete(),
		"healthScoreScope": map[string]string{"namespace": result.Meta.Namespace, "pod": result.Meta.Pod},
		"collection":       result.Collection,
	}
}
