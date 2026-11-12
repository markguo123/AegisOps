package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 可证明只读的组合走受控 argv 管道（方案 24 §3.2）：
// 不依赖 bash、不受 platform.exec.shell_enabled 阻挡、段间只用内存管道。

func TestReadOnlyProgramRunsSequenceWithoutShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "calls.txt")
	bin := fakeKubectl(t, "echo %* >> "+marker)
	r := NewRunner(Options{KubectlBin: bin, MaxBytes: 4096, ShellEnabled: false})

	res, err := r.RunKubectl(context.Background(), "kubectl get pods -n team-a && kubectl get deploy -n team-a")
	if err != nil {
		t.Fatalf("只读组合不应受 shell 通道开关影响: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := readFileString(t, marker)
	if strings.Count(calls, "get ") != 2 {
		t.Fatalf("两条只读查询都应执行，实际调用记录: %q", calls)
	}
	if !strings.Contains(calls, "pods") || !strings.Contains(calls, "deploy") {
		t.Fatalf("调用记录缺少查询内容: %q", calls)
	}
}

func TestReadOnlyProgramRunsPipelineWithoutShell(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "calls.txt")
	bin := fakeKubectl(t, "echo %* >> "+marker)
	r := NewRunner(Options{KubectlBin: bin, MaxBytes: 4096, ShellEnabled: false})

	res, err := r.RunKubectl(context.Background(), "kubectl get pods -n a | kubectl get deploy -n b")
	if err != nil {
		t.Fatalf("只读管道不应受 shell 通道开关影响: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := readFileString(t, marker)
	if strings.Count(calls, "get ") != 2 {
		t.Fatalf("管道两段都应执行，实际调用记录: %q", calls)
	}
}

func TestReadOnlyProgramUsesMinimalEnv(t *testing.T) {
	bin := fakeKubectl(t, "echo MODEL=[%AEGISOPS_TEST_MODEL_KEY%] KUBECONFIG=[%KUBECONFIG%]")
	r := NewRunner(Options{KubectlBin: bin, MaxBytes: 4096, Kubeconfig: `C:\k8s\config.yaml`})
	t.Setenv("AEGISOPS_TEST_MODEL_KEY", "leakedmodelkey123")

	res, err := r.RunKubectl(context.Background(), "kubectl get pods -n default")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stdout, "leakedmodelkey123") {
		t.Fatalf("子进程不应继承模型密钥等无关环境变量: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, `KUBECONFIG=[C:\k8s\config.yaml]`) {
		t.Fatalf("任务绑定 kubeconfig 必须传给子进程: %q", res.Stdout)
	}
}

func TestReadOnlyProgramDeniedBeforeExecution(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "forked.marker")
	bin := fakeKubectl(t, "echo touched > "+marker)
	r := NewRunner(Options{KubectlBin: bin, MaxBytes: 4096, ShellEnabled: false})

	// 未放行的过滤器参数/命令不会进入受控只读路径，只能落到 shell 闸门（关闭态直接拒绝）。
	for _, cmd := range []string{
		"kubectl get pods -n a | grep foo /etc/passwd",
		"kubectl get pods -n a | sort -o out.txt",
		"kubectl get pods -n a | awk '{print $1}'",
	} {
		res, err := r.RunKubectl(context.Background(), cmd)
		if err != nil {
			t.Errorf("%q: 关闭态应返回工具级结果而不是 error: %v", cmd, err)
			continue
		}
		if res.ExitCode != exitShellDisabled {
			t.Errorf("%q: 应被 shell 闸门拒绝（exit=%d）", cmd, res.ExitCode)
		}
	}
	if _, err := r.RunKubectl(context.Background(), "kubectl get secret db -n a | grep password"); err == nil {
		t.Error("Secret 硬红线必须直接拒绝")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("被拒绝的命令不应 fork 子进程")
	}
}

// TestReadOnlyProgramFilterPipeline 用真实过滤器验证管道与无匹配语义。
// 过滤器缺失的机器跳过（Windows 默认无 grep/jq）。
func TestReadOnlyProgramFilterPipeline(t *testing.T) {
	if _, err := exec.LookPath("grep"); err != nil {
		t.Skip("本机无 grep，跳过真实过滤器管道用例")
	}
	bin := fakeKubectl(t, "echo mysql-db-0\r\necho redis-0")
	r := NewRunner(Options{KubectlBin: bin, MaxBytes: 4096, ShellEnabled: false})

	res, err := r.RunKubectl(context.Background(), "kubectl get pods -n team-a | grep mysql | head -n 5")
	if err != nil {
		t.Fatalf("只读组合应执行: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Stdout, "mysql-db-0") {
		t.Fatalf("管道结果异常: %+v", res)
	}
	if strings.Contains(res.Stdout, "redis-0") {
		t.Fatalf("过滤器未生效: %q", res.Stdout)
	}

	nomatch, err := r.RunKubectl(context.Background(), "kubectl get pods -n team-a | grep postgres")
	if err != nil {
		t.Fatalf("无匹配不应是 Go error: %v", err)
	}
	if !nomatch.NoMatch {
		t.Fatalf("无匹配结果应标记 NoMatch: %+v", nomatch)
	}
	if strings.TrimSpace(nomatch.Stdout) != "" {
		t.Fatalf("无匹配时不应有输出: %q", nomatch.Stdout)
	}
}

func fileExists(path string) bool {
	_, err := exec.LookPath(path)
	return err == nil
}
