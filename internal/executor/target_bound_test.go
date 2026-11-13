package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestTargetBoundRejectsUnprovableShell 锁定 P12.2 的核心边界：多集群平台会话
// 里，无法证明目标边界的 shell 命令必须在起进程之前拒绝，人工审批不能替代隔离。
func TestTargetBoundRejectsUnprovableShell(t *testing.T) {
	r := NewRunner(Options{TargetBound: true, Kubeconfig: "/tmp/aegisops-bound.yaml", ShellEnabled: true})
	cases := []struct {
		name    string
		command string
	}{
		{"redirect", "kubectl get pods -n arc > /tmp/out"},
		{"env assignment", "KUBECONFIG=/tmp/evil kubectl get pods -n arc"},
		{"awk filter", "kubectl get pods -n arc | awk '{print $1}'"},
		{"command substitution", "kubectl get pods -n arc $(cat /tmp/args)"},
		{"interpreter", "bash -c 'kubectl get pods -n arc'"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.RunKubectl(context.Background(), tc.command)
			if !errors.Is(err, ErrTargetIsolation) {
				t.Fatalf("目标绑定模式必须拒绝无法证明目标边界的命令，got %v", err)
			}
		})
	}
}

// TestTargetBoundRejectsTargetOverrideFlags 锁定目标覆盖参数闸门：即便命令是
// 普通 argv、且“看起来”只读，--kubeconfig/--context/--server 也不能改写目标。
func TestTargetBoundRejectsTargetOverrideFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"kubectl", "get", "pods", "--kubeconfig", "/tmp/other"},
		{"kubectl", "--context", "other", "get", "pods"},
		{"kubectl", "get", "pods", "--server=https://other:6443"},
		{"kubectl", "get", "pods", "--as", "system:admin"},
	} {
		if err := validateBoundTargetArgv(argv); !errors.Is(err, ErrTargetIsolation) {
			t.Fatalf("argv %v 应被目标覆盖闸门拒绝，got %v", argv, err)
		}
	}
	if err := validateBoundTargetArgv([]string{"kubectl", "exec", "pod", "--", "sh", "-c", "echo --context"}); err != nil {
		t.Fatalf("exec 远端命令里的 --context 不是 kubectl 参数，不应误杀: %v", err)
	}
}

// TestTargetBoundChildEnvOnlyBoundKubeconfig 锁定子进程环境：不继承进程里的
// 其它 KUBECONFIG/密钥变量，只出现一条绑定目标的 KUBECONFIG。
func TestTargetBoundChildEnvOnlyBoundKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "/tmp/other-cluster.yaml")
	t.Setenv("DB_PASSWORD", "should-not-leak")
	r := NewRunner(Options{TargetBound: true, Kubeconfig: "/tmp/aegisops-bound.yaml"})
	env := r.childEnv()
	count := 0
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "KUBECONFIG":
			count++
			if value != "/tmp/aegisops-bound.yaml" {
				t.Fatalf("子进程 KUBECONFIG 必须是绑定目标，got %q", value)
			}
		case "DB_PASSWORD":
			t.Fatal("目标绑定子进程不得继承数据库密码等平台密钥")
		}
	}
	if count != 1 {
		t.Fatalf("子进程环境必须恰好有一条 KUBECONFIG，got %d", count)
	}
}

// TestTargetBoundPreflightRejectsRotatedCredential 锁定凭据轮换/撤权后旧调用
// 立即失效：前置校验失败时连只读命令也不能起进程。
func TestTargetBoundPreflightRejectsRotatedCredential(t *testing.T) {
	rotated := false
	r := NewRunner(Options{
		TargetBound: true,
		Kubeconfig:  "/tmp/aegisops-bound.yaml",
		TargetCheck: func(context.Context) error {
			if rotated {
				return errors.New("target credentials changed")
			}
			return nil
		},
	})
	rotated = true
	if _, err := r.RunKubectl(context.Background(), "kubectl get pods -n arc"); !errors.Is(err, ErrTargetIsolation) {
		t.Fatalf("凭据变化后必须拒绝旧调用，got %v", err)
	}
}

// TestTargetBoundRequiresCredential 防止“打开绑定模式却忘记注入凭据”被当成
// 继承进程默认 kubeconfig 的合法路径。
func TestTargetBoundRequiresCredential(t *testing.T) {
	r := NewRunner(Options{TargetBound: true})
	_, err := r.RunKubectl(context.Background(), "kubectl delete pod x -n arc")
	if !errors.Is(err, ErrTargetIsolation) {
		t.Fatalf("缺少绑定凭据时必须拒绝，got %v", err)
	}
}
