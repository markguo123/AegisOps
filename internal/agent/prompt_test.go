package agent

import (
	"strings"
	"testing"
)

func TestBasePromptIncludesR6CreateProtocol(t *testing.T) {
	p := basePrompt()
	required := []string{
		"创建行为约束",
		"create_resource",
		"mode=dry_run",
		"proposal_id",
		"mode=apply",
		"required_inputs",
		"禁止编造默认值",
		"$GENERATE",
		"$ASK",
		"被拒绝 2 次",
		"kubectl create secret",
	}
	for _, want := range required {
		if !strings.Contains(p, want) {
			t.Errorf("basePrompt missing %q", want)
		}
	}
}

func TestBasePromptUsesAegisOpsIdentity(t *testing.T) {
	p := basePrompt()
	for _, want := range []string{
		"你是 AegisOps（云原生智能工作台）",
		"你的云原生智能工作台与运维伴侣",
		"必须自称 AegisOps",
		"不得自称 k8s-ai",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("basePrompt 缺少产品身份约束 %q", want)
		}
	}
}

func TestBasePromptRoutesCreateIntentAwayFromKubectl(t *testing.T) {
	p := basePrompt()
	for _, want := range []string{
		"创建/部署/新建意图",
		"不要把资源创建改写成 kubectl create/apply",
		"资源创建统一使用 **create_resource**",
		"不走 kubectl create/apply",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("basePrompt 缺少创建意图路由约束 %q", want)
		}
	}
}

func TestBasePromptOrdersDryRunBeforeApply(t *testing.T) {
	p := basePrompt()
	i := strings.Index(p, "先调 **create_resource(mode=dry_run")
	j := strings.Index(p, "再调 **create_resource(mode=apply")
	if i == -1 || j == -1 || i >= j {
		t.Fatalf("basePrompt 未按 dry_run → 提案 → apply 顺序描述创建流程 (i=%d j=%d)", i, j)
	}
}

func TestShimPromptIncludesCreateProtocol(t *testing.T) {
	p := shimPrompt("[]", "create_resource")
	for _, want := range []string{
		"创建行为约束",
		"create_resource",
		"mode=dry_run",
		"mode=apply",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("shimPrompt missing %q", want)
		}
	}
}

func TestClarificationRuleAppliesToNativeAndShimPrompts(t *testing.T) {
	for _, prompt := range []string{basePrompt(), shimPrompt("[]", "")} {
		for _, clause := range []string{
			"适用于所有任务",
			"在当前授权范围内只读核实候选",
			"能可靠确定任务意图、目标、范围及必要参数时，直接推进",
			"只有剩余歧义会实质改变操作、目标、范围、结果或风险时，才用 ask_user_question",
			"工具不可用时，在回复中提出问题并等待用户下一轮回答",
		} {
			if !strings.Contains(prompt, clause) {
				t.Errorf("missing clarification clause %q", clause)
			}
		}
	}
}

func TestPromptUsesDecisionToolWithoutForcedAlternatives(t *testing.T) {
	p := basePrompt()
	if !strings.Contains(p, "ask_user_question") || strings.Contains(p, "≥2 条") {
		t.Fatal("decision guidance missing or forced alternatives remain")
	}
	if !strings.Contains(p, "选择方案不授权具体写操作") {
		t.Fatal("approval boundary missing")
	}
}
