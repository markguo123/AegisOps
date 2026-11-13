package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	"github.com/k8s-ai/k8s-ai/internal/security"
)

// ErrTargetIsolation 是目标绑定模式下拒绝执行的稳定原因。
//
// 多集群平台模式下，人工审批不能替代目标隔离：任意 shell 命令都能通过
// 环境赋值、--kubeconfig/--context/--server、读取其它配置文件或调用其它程序
// 改变实际目标。无法证明目标边界的命令必须在构造子进程之前拒绝（P12.2 /
// 方案 22 §8.1），而不是只设置一个 KUBECONFIG 就当作隔离。
var ErrTargetIsolation = errors.New("目标绑定模式拒绝该命令")

// targetOverrideFlags 是能改变 kubectl 实际连接目标的参数。目标绑定模式下
// 即使命令本身是 kubectl argv 也必须拒绝：审批只解决"要不要执行"，不解决
// "执行到哪个集群"。
var targetOverrideFlags = map[string]bool{
	"--kubeconfig": true, "--context": true, "--cluster": true,
	"--server": true, "--token": true, "--user": true, "--username": true,
	"--password": true, "--client-certificate": true, "--client-key": true,
	"--certificate-authority": true, "--insecure-skip-tls-verify": true,
	"--as": true, "--as-group": true, "--as-uid": true,
	"--tls-server-name": true,
}

// validateBoundTargetArgv 拒绝目标覆盖参数。`--` 之后是 exec 的远端命令，
// 不是 kubectl 参数，扫描到分隔符即停止。
func validateBoundTargetArgv(argv []string) error {
	for _, token := range argv {
		if token == "--" {
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			continue
		}
		name, _, _ := strings.Cut(token, "=")
		if targetOverrideFlags[name] {
			return fmt.Errorf("%w：kubectl 参数 %s 会改写实际连接目标；平台会话只能操作服务端绑定的集群", ErrTargetIsolation, name)
		}
	}
	return nil
}

// defaultStreamTimeout 是流式命令（logs -f / get -w）的默认采样时长。
// 借鉴 kubectl-ai streaming.go：到时后捕获部分输出并标记 StreamTimeout，不阻塞 Agent Loop。
const defaultStreamTimeout = 7 * time.Second

// redactor 脱敏 kubectl 自由文本输出（FR-018；Redactor 编译正则只读，线程安全）。
var redactor = security.NewRedactor()

// exitShellDisabled 借用 shell 的 126 语义（命令存在但不可执行）：shell 通道被策略
// 关闭时不 fork 进程，所以这不是真实子进程退出码，只是给上层与审计的稳定标记。
const exitShellDisabled = 126

// shellDisabledHint 是关闭态的可指导文案（D-33/ADR-044 v1.1）。首行会被工具层摘要
// 取走（tools.extractKubectlSummary），所以第一句必须自带结论与替代动作。
const shellDisabledHint = "shell 通道已由平台配置关闭（platform.exec.shell_enabled=false）：该实例不提供 bash，" +
	"含管道 |、串联 &&/;、重定向 >、命令替换 $() 或 heredoc 的命令一律不执行。\n" +
	"请改用单条不含 shell 语法的 kubectl 命令（例如 kubectl get pods -A -o json），" +
	"过滤、统计交给结构化只读工具（scan_cluster / get_pod / get_deployment 等）。\n" +
	"确实需要 shell 排障能力时，请让管理员改用默认交付镜像（内置 bash 与 kubectl）" +
	"或把 platform.exec.shell_enabled 置为 true 后重启服务。"

// RunKubectl 执行 kubectl 命令：
// deny_ops → 安全红线（secret/--raw）→ 可证明只读组合走受控 argv 管道 →
// 解析（argv 或 shell）→ 动词白名单 → exec.Command → 输出截断 + 脱敏。
// 可证明只读的单命令/组合（含管道、&&/||、;）不走 bash，由 runReadOnlyProgram
// 逐段直调固定二进制；因此不依赖 bash，也不受 platform.exec.shell_enabled 阻挡。
// 其余命令仍按二期行为：isShell=true（含无法证明只读的 shell 构造）时走 bash -c，
// 且只在人工审批通过且本实例 shell 通道开启时才会执行。
// 流式命令（logs -f / get -w）套固定超时，超时后捕获部分输出，不视为错误。
func (r *Runner) RunKubectl(ctx context.Context, command string) (Result, error) {
	if approval.IsDenied(r.denyOps, command) {
		return Result{Command: command}, fmt.Errorf("command denied by deny_ops policy")
	}
	// LLM 数据安全红线：Secret 数据永不进入 LLM 上下文（AGENTS.md）。
	// 拒绝文案必须可指导：指向结构化元数据工具与按需明文通道，而不是让模型换个写法重试。
	if approval.IsForbiddenCommand(command) {
		return Result{Command: command}, fmt.Errorf("command forbidden: kubectl 不能直接读取 Secret 资源或使用 --raw。" +
			"Secret 元数据（名称/类型/键名）请用 get_secret_info（普通读取权限）；" +
			"确需查看明文时用 get_secret_value（按 key、审批或配置授权、仅当前轮模型可见）。" +
			"敏感数据不进入 LLM 上下文")
	}
	// 目标前置校验（P12.2）：每次执行前重验用户授权、集群存在性与凭据版本。
	// 撤权或凭据轮换后，旧命令与旧批准在真正起进程前失效，不再依赖看护周期。
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return Result{Command: command}, fmt.Errorf("%w：%v", ErrTargetIsolation, err)
		}
	}
	// 可证明只读的受控路径（方案 24 §3.2）：执行前重新分析实际命令，
	// 批准后命令不可替换，且整条组合共用任务超时与输出上限。
	// 该路径逐段直调固定 argv、不经过 shell，是目标绑定模式下唯一放行的
	// shell 形状命令。
	analysis := cmdplan.Analyze(command)
	if analysis.ProvablyReadOnly && len(analysis.Program.Commands) > 0 {
		if r.targetBound && r.kubeconfig == "" {
			return Result{Command: command}, fmt.Errorf("%w：未装配绑定目标的临时凭据", ErrTargetIsolation)
		}
		return r.runReadOnlyProgram(ctx, command, analysis.Program)
	}
	// 目标绑定模式：无法证明目标边界的 shell 命令一律拒绝，人工审批不放行。
	// 覆盖重定向、命令替换、变量、awk/sed/xargs、任意解释器、环境赋值等全部
	// 非受控构造；--context/--kubeconfig/--server 等目标覆盖参数在 argv 路径另判。
	pc, err := ParseCommandDetail(command)
	if err != nil {
		if r.targetBound && analysis.NeedsShell {
			return Result{Command: command}, fmt.Errorf("%w：无法证明命令目标边界", ErrTargetIsolation)
		}
		return Result{Command: command}, err
	}
	if r.targetBound {
		if pc.IsShell {
			return Result{Command: command}, fmt.Errorf(
				"%w：该命令需要 shell 解释器，目标无法被证明绑定到当前集群", ErrTargetIsolation)
		}
		if r.kubeconfig == "" {
			return Result{Command: command}, fmt.Errorf("%w：未装配绑定目标的临时凭据", ErrTargetIsolation)
		}
		if err := validateBoundTargetArgv(pc.Argv); err != nil {
			return Result{Command: command}, err
		}
	}
	// shell 策略闸门（P6.3 / D-33）：必须在构造 exec.Cmd 之前拦下。
	// 无 shell 镜像变体里 bash 不存在，放行只会让模型收到 "bash: executable file
	// not found" 这种它修不了的环境错误并反复重试；这里给可指导文案，且不起进程。
	// 注意：开启态下 shell 命令能否执行由审批层决定（shell 永不自动执行、
	// 不被 --skip-permissions 免除），本闸门只决定"这个实例有没有解释器"。
	if pc.IsShell && !r.shellEnabled {
		return Result{Command: command, ExitCode: exitShellDisabled, Stderr: shellDisabledHint}, nil
	}
	argv := pc.Argv
	if r.bin == "kubectl" {
		if _, err := exec.LookPath("kubectl"); err != nil {
			// 保留 "kubectl not found" 关键字：tools 层据此识别"环境缺二进制"并回
			// SYSTEM_UNAVAILABLE，避免模型把它当命令写错而反复重试。
			return Result{Command: command}, fmt.Errorf(
				"kubectl not found: PATH 中没有 kubectl（%v）；默认交付镜像内置 kubectl，"+
					"请检查镜像是否为无 shell 变体，或用 kubernetes.kubectl_path 指定二进制路径。"+
					"只读链路（client-go 结构化工具与 scan）不受影响", err)
		}
	}
	// 流式命令（logs -f / get -w）：套固定超时，捕获部分输出，超时不报错。
	// 交互式流（attach/exec -it）已在 ParseCommand 拒绝，不会进入此分支。
	runCtx := ctx
	var cancel context.CancelFunc
	streaming := IsStreaming(argv)
	if streaming {
		timeout := r.streamTimeout
		if timeout <= 0 {
			timeout = defaultStreamTimeout
		}
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	if pc.IsShell {
		// shell 模式：bash -c <完整命令>，支持 pipe/heredoc/redirect/&&/|| 等。
		// 安全保证：进入此分支的写操作已由 Agent Loop 的 executeTool 走过人工审批；
		// deny_ops、Secret 拦截与 shell 通道开关在上方已校验；输出仍统一脱敏。
		cmd = exec.CommandContext(runCtx, shellBinary, "-c", command)
	} else {
		// argv 模式：直调二进制，无 shell。
		cmd = exec.CommandContext(runCtx, r.bin, argv[1:]...)
	}
	stdout := newCappedBuffer(r.maxBytes)
	stderr := newCappedBuffer(r.maxBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if r.targetBound {
		// 目标绑定：不继承进程环境，只注入绑定的 KUBECONFIG（childEnv 已剔除
		// 进程里其它 KUBECONFIG 与凭据）。
		cmd.Env = r.childEnv()
	} else {
		cmd.Env = os.Environ()
		if r.kubeconfig != "" {
			cmd.Env = append(cmd.Env, "KUBECONFIG="+r.kubeconfig)
		}
	}
	runErr := cmd.Run()
	// 流式超时：捕获已收到的部分输出，标记 StreamTimeout，不视为错误。
	// 仅当父 ctx 未取消、且我们的超时截止才触发，避免吞掉真实取消。
	if streaming && ctx.Err() == nil && runCtx.Err() == context.DeadlineExceeded {
		return Result{
			Command:       command,
			Stdout:        redactor.Redact(stdout.String()),
			Stderr:        redactor.Redact(stderr.String()),
			StreamTimeout: true,
			Truncated:     stdout.truncated || stderr.truncated,
		}, nil
	}
	res := Result{
		Command:   command,
		Stdout:    redactor.Redact(stdout.String()),
		Stderr:    redactor.Redact(stderr.String()),
		Truncated: stdout.truncated || stderr.truncated,
	}
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			res.ExitCode = exitErr.ExitCode()
			return res, nil // 非零退出码交给 LLM 分析，不视为调用失败
		}
		return res, runErr
	}
	return res, nil
}

// HasShellSyntax 已移至 approval 包以避免循环依赖。
// executor 通过 approval.HasShellSyntax 间接调用（仅在审批分类层使用）。

// cappedBuffer 超过上限后丢弃多余字节并标记截断。
type cappedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCappedBuffer(max int) *cappedBuffer {
	if max <= 0 {
		max = 64 * 1024
	}
	return &cappedBuffer{max: max}
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	if len(p) > room {
		c.truncated = true
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}
