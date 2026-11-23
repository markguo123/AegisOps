package config

import (
	"strings"
	"testing"
)

// TestValidateSecretAccess 锁住 Secret 访问模式：
// create_only 只允许创建；read_on_demand 允许逐 key 审批读取；read_write
// 这个宽泛模式仍被拒绝，避免配置层留下无审批的全量读入口。
func TestValidateSecretAccess(t *testing.T) {
	base, err := Load(LoadOptions{NoEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}
	if base.Agent.Secret.Access != "read_on_demand" {
		t.Fatalf("默认 Secret 模式 = %q, want read_on_demand", base.Agent.Secret.Access)
	}

	for _, access := range []string{"", "off", "create_only", "read_on_demand"} {
		cfg := *base
		cfg.Agent.Secret.Access = access
		if err := cfg.Validate(); err != nil {
			t.Errorf("access=%q 应通过校验: %v", access, err)
		}
	}

	for _, tc := range []struct{ access, want string }{
		{"read_write", "not supported"},
		{"read-only", "must be one of"},
	} {
		cfg := *base
		cfg.Agent.Secret.Access = tc.access
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("access=%q 应被拒绝并说明原因（含 %q），实际: %v", tc.access, tc.want, err)
		}
	}
}

// TestValidateSecretReadApproval 锁住新增的读取审批模式（方案 24 §4.1）：
// 缺省/旧配置保持 always；非法值启动失败；off/create_only 搭配 auto 是矛盾配置。
func TestValidateSecretReadApproval(t *testing.T) {
	base, err := Load(LoadOptions{NoEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if base.Agent.Secret.ReadApproval != SecretReadApprovalAlways {
		t.Fatalf("默认 read_approval = %q, want %q", base.Agent.Secret.ReadApproval, SecretReadApprovalAlways)
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("默认配置应通过校验: %v", err)
	}

	for _, tc := range []struct{ access, approval string }{
		{"read_on_demand", ""},
		{"read_on_demand", "always"},
		{"read_on_demand", "auto"},
		{"off", "always"},
		{"create_only", ""},
	} {
		cfg := *base
		cfg.Agent.Secret.Access = tc.access
		cfg.Agent.Secret.ReadApproval = tc.approval
		if err := cfg.Validate(); err != nil {
			t.Errorf("access=%q read_approval=%q 应通过校验: %v", tc.access, tc.approval, err)
		}
	}

	for _, tc := range []struct{ access, approval, want string }{
		{"read_on_demand", "sometimes", "must be one of"},
		{"off", "auto", "requires agent.secret.access=read_on_demand"},
		{"create_only", "auto", "requires agent.secret.access=read_on_demand"},
	} {
		cfg := *base
		cfg.Agent.Secret.Access = tc.access
		cfg.Agent.Secret.ReadApproval = tc.approval
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("access=%q read_approval=%q 应被拒绝并说明原因（含 %q），实际: %v",
				tc.access, tc.approval, tc.want, err)
		}
	}
}
