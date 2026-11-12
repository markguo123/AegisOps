package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
)

// defaultProgramTimeout 是可证明只读组合的整体超时上限。任务本身可能带更短的
// context 上限（以任务为准）；这里只是防止过滤器无限运行。
const defaultProgramTimeout = 60 * time.Second

// 子进程最小环境白名单（方案 24 §5）：只带必要 PATH/语言/证书/代理/临时目录、
// 任务绑定凭据，以及各云厂商 kubeconfig exec 插件需要的凭据变量前缀。
// 不继承数据库密码、模型密钥和其他集群凭据。
var childEnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"TMPDIR", "TEMP", "TMP",
	"SYSTEMROOT", "SYSTEMDRIVE", "WINDIR", "COMSPEC", "PATHEXT",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// childEnvPrefixAllowlist 是 kubeconfig exec 插件常用的凭据变量前缀。
var childEnvPrefixAllowlist = []string{
	"AWS_", "AZURE_", "GOOGLE_", "GCLOUD_", "CLOUDSDK_", "KUBERNETES_",
}

// childEnvDenylist 是能改变命令行为的变量：即使命中白名单前缀也不继承。
var childEnvDenylist = map[string]bool{
	"KUBECTL_EXTERNAL_DIFF": true,
	"BASH_ENV":              true,
	"ENV":                   true,
	"SHELLOPTS":             true,
	"BASHOPTS":              true,
	"PROMPT_COMMAND":        true,
	"PS4":                   true,
}

// childEnv 构造受控子进程环境。kubeconfig 为空时沿用进程的 KUBECONFIG
// （部署方注入的任务绑定凭据），非空时以显式任务绑定值为准。
func (r *Runner) childEnv() []string {
	env := make([]string, 0, 32)
	for _, key := range childEnvAllowlist {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if childEnvDenylist[key] {
			continue
		}
		for _, prefix := range childEnvPrefixAllowlist {
			if strings.HasPrefix(key, prefix) {
				env = append(env, entry)
				break
			}
		}
	}
	if r.kubeconfig != "" {
		env = append(env, "KUBECONFIG="+r.kubeconfig)
	} else if value, ok := os.LookupEnv("KUBECONFIG"); ok && value != "" {
		env = append(env, "KUBECONFIG="+value)
	}
	return env
}

// resolvedStage 是一段已经完成二进制解析的 argv。
type resolvedStage struct {
	bin  string
	args []string
}

// pipeStatus 是一段在管道中执行后的结论。
type pipeStatus struct {
	code   int
	stderr string
}

// runReadOnlyProgram 执行可证明只读的受控 argv 程序（方案 24 §3.2）：
// 不把已校验文本重新交给 bash，逐段直调固定二进制，段间只用内存管道传递，
// 保留退出码语义，取消时清理全部子进程与管道。
func (r *Runner) runReadOnlyProgram(ctx context.Context, command string, prog cmdplan.Program) (Result, error) {
	kubectlPath, err := exec.LookPath(r.bin)
	if err != nil {
		return Result{Command: command}, fmt.Errorf(
			"kubectl not found: PATH 中没有 kubectl（%v）；默认交付镜像内置 kubectl，"+
				"请检查镜像是否为无 shell 变体，或用 kubernetes.kubectl_path 指定二进制路径。"+
				"只读链路（client-go 结构化工具与 scan）不受影响", err)
	}
	pipelines, err := r.resolveProgram(prog, kubectlPath)
	if err != nil {
		return Result{Command: command}, err
	}
	runCtx, cancel := r.programContext(ctx)
	defer cancel()
	env := r.childEnv()

	result := Result{Command: command}
	lastCode := 0
	lastOutput := ""
	truncated := false
	var stderr strings.Builder
	var kubectlFailure *pipeStatus
	for i, stages := range pipelines {
		if i > 0 {
			switch prog.Commands[i].Op {
			case cmdplan.OpAnd:
				if lastCode != 0 {
					continue
				}
			case cmdplan.OpOr:
				if lastCode == 0 {
					continue
				}
			}
		}
		out, statuses, outTruncated, err := r.runPipeline(runCtx, stages, env)
		if err != nil {
			return Result{Command: command}, err
		}
		lastOutput = out
		truncated = truncated || outTruncated
		lastCode = statuses[len(statuses)-1].code
		for idx, st := range statuses {
			if st.stderr != "" {
				stderr.WriteString(st.stderr)
				if !strings.HasSuffix(st.stderr, "\n") {
					stderr.WriteString("\n")
				}
			}
			// Kubernetes 查询失败不能被后置过滤器掩盖。
			if prog.Commands[i].Stages[idx].Kind == cmdplan.StageKubectl && st.code != 0 && kubectlFailure == nil {
				failure := st
				kubectlFailure = &failure
			}
		}
		if kubectlFailure != nil {
			break
		}
	}
	if ctx.Err() != nil && kubectlFailure == nil {
		return Result{Command: command}, fmt.Errorf("command cancelled: %w", ctx.Err())
	}
	result.Stdout = redactor.Redact(lastOutput)
	result.Stderr = redactor.Redact(strings.TrimRight(stderr.String(), "\n"))
	result.Truncated = truncated
	if kubectlFailure != nil {
		result.ExitCode = kubectlFailure.code
		return result, nil
	}
	// grep 无匹配（退出码 1 且无输出）不是集群故障，必须给出清晰的无匹配结论。
	if lastCode == 1 && strings.TrimSpace(lastOutput) == "" {
		result.NoMatch = true
	}
	result.ExitCode = lastCode
	return result, nil
}

// resolveProgram 把执行计划里的每一段解析成固定二进制路径。
// 任何一段缺失都在起进程之前失败，避免半途启动子进程。
func (r *Runner) resolveProgram(prog cmdplan.Program, kubectlPath string) ([][]resolvedStage, error) {
	out := make([][]resolvedStage, 0, len(prog.Commands))
	for _, cmd := range prog.Commands {
		stages := make([]resolvedStage, 0, len(cmd.Stages))
		for _, st := range cmd.Stages {
			if st.Kind == cmdplan.StageKubectl {
				stages = append(stages, resolvedStage{bin: kubectlPath, args: st.Argv[1:]})
				continue
			}
			path, err := exec.LookPath(st.Argv[0])
			if err != nil {
				return nil, fmt.Errorf("只读过滤器 %q 不可用：%w", st.Argv[0], err)
			}
			stages = append(stages, resolvedStage{bin: path, args: st.Argv[1:]})
		}
		out = append(out, stages)
	}
	return out, nil
}

// programContext 在任务已带上限时沿用任务上限，否则套用默认上限。
func (r *Runner) programContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultProgramTimeout)
}

// runPipeline 并发启动一条管线的所有段，按顺序回收退出码与 stderr。
func (r *Runner) runPipeline(ctx context.Context, stages []resolvedStage, env []string) (string, []pipeStatus, bool, error) {
	maxBytes := r.maxBytes
	if maxBytes <= 0 {
		maxBytes = 64 * 1024
	}
	stdout := newCappedBuffer(maxBytes)
	statuses := make([]pipeStatus, len(stages))
	cmds := make([]*exec.Cmd, len(stages))
	writers := make([]*io.PipeWriter, len(stages))
	buffers := make([]*lockedBuffer, len(stages))
	var reader *io.PipeReader

	abort := func() {
		for _, c := range cmds {
			if c != nil && c.Process != nil {
				_ = c.Process.Kill()
			}
		}
		for _, w := range writers {
			if w != nil {
				_ = w.Close()
			}
		}
		if reader != nil {
			_ = reader.Close()
		}
	}

	for i, st := range stages {
		cmd := exec.CommandContext(ctx, st.bin, st.args...)
		cmd.Env = env
		buf := newLockedBuffer(maxBytes)
		buffers[i] = buf
		cmd.Stderr = buf
		if i > 0 && reader != nil {
			cmd.Stdin = reader
		}
		if i < len(stages)-1 {
			pr, pw := io.Pipe()
			cmd.Stdout = pw
			writers[i] = pw
			reader = pr
		} else {
			cmd.Stdout = stdout
		}
		if err := cmd.Start(); err != nil {
			abort()
			return "", nil, false, fmt.Errorf("start %s: %w", st.bin, err)
		}
		cmds[i] = cmd
	}
	for i, cmd := range cmds {
		waitErr := cmd.Wait()
		if writers[i] != nil {
			_ = writers[i].Close()
		}
		statuses[i].stderr = buffers[i].String()
		statuses[i].code = exitCode(waitErr)
		if i < len(stages)-1 && isBrokenPipe(waitErr) {
			// 下游提前结束（head -n N）导致的 SIGPIPE 不是查询失败。
			statuses[i].code = 0
		}
	}
	truncated := stdout.truncated
	for _, buf := range buffers {
		if buf.truncated() {
			truncated = true
		}
	}
	return stdout.String(), statuses, truncated, nil
}

// exitCode 归一化子进程退出码：0 表示成功，-1（被信号终止且无退出码）记为 128。
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 128
	}
	return 1
}

// isBrokenPipe 判断子进程是否因下游关闭管道而退出。
func isBrokenPipe(err error) bool {
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 141 {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") || strings.Contains(msg, "epipe")
}

// lockedBuffer 是并发安全、带上限的 stderr 缓冲（管道里多个子进程会并发写）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf cappedBuffer
}

func newLockedBuffer(max int) *lockedBuffer {
	if max <= 0 {
		max = 64 * 1024
	}
	return &lockedBuffer{buf: cappedBuffer{max: max}}
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.truncated
}
