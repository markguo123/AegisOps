package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/approval"
	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	utilexec "k8s.io/client-go/util/exec"
)

type ExecPodOptions struct {
	Enabled                  bool
	TimeoutSeconds           int
	MaxTimeoutSeconds        int
	MaxOutputBytes           int
	MaxConcurrentPerInstance int
	MaxConcurrentPerUser     int
}

func DefaultExecPodOptions() ExecPodOptions { return ExecPodOptions{true, 15, 30, 65536, 8, 2} }

type podExecKey struct{}

// DisablePodExec is an assembly-time switch shared by both tool entry points.
func (r *Runner) DisablePodExec() { r.execPod.Enabled = false }

type podExecPermit struct {
	runner        *Runner
	request       cmdplan.PodExecRequest
	digest        [32]byte
	requestDigest [32]byte
	snapshot      string
	approved      atomic.Bool
	used          atomic.Bool
	user, session string
	rbacStatus    string
}

func execDigest(r cmdplan.PodExecRequest) [32]byte {
	b, _ := json.Marshal(r)
	return sha256.Sum256(append([]byte(cmdplan.PodExecPolicyVersion), b...))
}

// PreparePodExec captures a server-owned plan before human confirmation. It
// performs no remote command and grants no execution authority on its own.
func (r *Runner) PreparePodExec(ctx context.Context, req cmdplan.PodExecRequest, user, session string) (context.Context, error) {
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	if !r.execPod.Enabled {
		return ctx, fmt.Errorf("TOOL_UNAVAILABLE: EXEC_DISABLED")
	}
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return ctx, err
		}
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = r.execPod.TimeoutSeconds
	}
	req, err := cmdplan.NormalizePodExec(req)
	if err != nil {
		return ctx, err
	}
	if approval.IsDenied(r.denyOps, "kubectl exec "+req.Pod+" -n "+req.Namespace+" -- "+strings.Join(req.Argv, " ")) {
		return ctx, fmt.Errorf("POLICY_DENIED: deny_ops")
	}
	if req.TimeoutSeconds > r.execPod.MaxTimeoutSeconds {
		return ctx, fmt.Errorf("INVALID_ARGUMENT: exec timeout exceeds configured maximum")
	}
	if r.cs == nil || r.restConfig == nil {
		return ctx, fmt.Errorf("TOOL_UNAVAILABLE: Pod exec client is unavailable")
	}
	p, err := r.cs.CoreV1().Pods(req.Namespace).Get(ctx, req.Pod, metav1.GetOptions{})
	if err != nil {
		return ctx, err
	}
	if req.Container == "" {
		if len(p.Spec.Containers) != 1 {
			return ctx, fmt.Errorf("INVALID_ARGUMENT: CONTAINER_REQUIRED")
		}
		req.Container = p.Spec.Containers[0].Name
	}
	snapshot, err := podExecSnapshot(p, req.Container)
	if err != nil {
		return ctx, err
	}
	for _, arg := range req.Argv[1:] {
		if strings.HasPrefix(arg, "/") {
			if err := validatePodPath(p, req.Container, arg, req.Argv[0] == "du"); err != nil {
				return ctx, err
			}
		}
	}
	// Positive/negative SAR is advisory; the actual exec request remains the RBAC boundary.
	allowed, e := r.CanI(ctx, "create", "", "pods/exec", req.Namespace, req.Pod)
	if e == nil && !allowed {
		return ctx, fmt.Errorf("RBAC_DENIED: POD_EXEC_FORBIDDEN")
	}
	if session == "" {
		return ctx, fmt.Errorf("INVALID_ARGUMENT: exec requires a session identity")
	}
	if user == "" {
		user = "cli:" + session
	}
	requestDigest := execDigest(req)
	planDigest := sha256.Sum256([]byte(fmt.Sprintf("%x\n%s\n%s\n%s\n%s\n%s", requestDigest, snapshot, user, session, r.kubeconfig, r.restConfig.Host)))
	permit := &podExecPermit{runner: r, request: req, digest: planDigest, requestDigest: requestDigest, snapshot: snapshot, user: user, session: session}
	permit.rbacStatus = "unknown"
	if e == nil && allowed {
		permit.rbacStatus = "passed"
	}
	return context.WithValue(ctx, podExecKey{}, permit), nil
}

// ApprovePodExec is called only by the Agent after consuming an explicit human
// confirmation. The unexported permit cannot originate in tool JSON or history.
func (r *Runner) ApprovePodExec(ctx context.Context) (context.Context, error) {
	p, ok := ctx.Value(podExecKey{}).(*podExecPermit)
	if !ok || p.runner != r || p.used.Load() || !p.approved.CompareAndSwap(false, true) {
		return ctx, fmt.Errorf("POLICY_DENIED: invalid exec confirmation")
	}
	return ctx, nil
}

// PreparedPodExec returns a copy for an accurate, non-authoritative approval preview.
func (r *Runner) PreparedPodExec(ctx context.Context) (cmdplan.PodExecRequest, bool) {
	p, ok := ctx.Value(podExecKey{}).(*podExecPermit)
	if !ok || p.runner != r {
		return cmdplan.PodExecRequest{}, false
	}
	req := p.request
	req.Argv = append([]string(nil), req.Argv...)
	return req, true
}

func (r *Runner) PodExecRBACStatus(ctx context.Context) string {
	p, ok := ctx.Value(podExecKey{}).(*podExecPermit)
	if !ok || p.runner != r {
		return "unknown"
	}
	return p.rbacStatus
}

func podExecSnapshot(p *corev1.Pod, container string) (string, error) {
	if p.DeletionTimestamp != nil || p.Spec.HostPID || p.Spec.HostIPC || p.Spec.HostNetwork {
		return "", fmt.Errorf("POLICY_DENIED: POD_ISOLATION_UNSUPPORTED")
	}
	for _, v := range p.Spec.Volumes {
		if v.HostPath != nil {
			return "", fmt.Errorf("POLICY_DENIED: POD_ISOLATION_UNSUPPORTED")
		}
	}
	found := false
	for _, c := range p.Spec.Containers {
		if c.Name == container {
			found = true
		}
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			return "", fmt.Errorf("POLICY_DENIED: POD_ISOLATION_UNSUPPORTED")
		}
	}
	if !found {
		return "", fmt.Errorf("INVALID_ARGUMENT: unknown regular container")
	}
	id := ""
	restarts := int32(0)
	for _, s := range p.Status.ContainerStatuses {
		if s.Name == container && s.State.Running != nil {
			id = s.ContainerID
			restarts = s.RestartCount
		}
	}
	if id == "" {
		return "", fmt.Errorf("CONFLICT: CONTAINER_NOT_RUNNING")
	}
	b, _ := json.Marshal(struct {
		UID      string
		ID       string
		Restarts int32
		Spec     corev1.PodSpec
	}{string(p.UID), id, restarts, p.Spec})
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func validatePodPath(p *corev1.Pod, container, target string, recursive bool) error {
	if err := cmdplan.ValidateDiagnosticPath(target); err != nil {
		return err
	}
	if recursive {
		for _, sensitive := range []string{"/proc", "/sys", "/dev", "/var/run/secrets", "/run/secrets", "/var/secrets", "/etc/secrets", "/vault/secrets"} {
			if cmdplan.PathUnder(sensitive, path.Clean(target)) {
				return fmt.Errorf("POLICY_DENIED: recursive traversal includes a sensitive root")
			}
		}
	}
	sensitive := map[string]bool{}
	for _, v := range p.Spec.Volumes {
		if v.Secret != nil || v.Projected != nil || v.CSI != nil {
			sensitive[v.Name] = true
		}
	}
	for _, c := range p.Spec.Containers {
		if c.Name == container {
			for _, m := range c.VolumeMounts {
				if sensitive[m.Name] && (cmdplan.PathUnder(path.Clean(target), path.Clean(m.MountPath)) || recursive && cmdplan.PathUnder(path.Clean(m.MountPath), path.Clean(target))) {
					return fmt.Errorf("POLICY_DENIED: SENSITIVE_MOUNT")
				}
			}
		}
	}
	return nil
}

var podExecSlots = struct {
	sync.Mutex
	total    int
	users    map[string]int
	sessions map[string]bool
}{users: map[string]int{}, sessions: map[string]bool{}}

func (r *Runner) acquirePodExec(p *podExecPermit) (func(), error) {
	podExecSlots.Lock()
	defer podExecSlots.Unlock()
	if podExecSlots.total >= r.execPod.MaxConcurrentPerInstance || podExecSlots.users[p.user] >= r.execPod.MaxConcurrentPerUser || podExecSlots.sessions[p.session] {
		return nil, fmt.Errorf("EXECUTION_BUSY: Pod exec concurrency limit")
	}
	podExecSlots.total++
	podExecSlots.users[p.user]++
	podExecSlots.sessions[p.session] = true
	return func() {
		podExecSlots.Lock()
		defer podExecSlots.Unlock()
		podExecSlots.total--
		podExecSlots.users[p.user]--
		if podExecSlots.users[p.user] == 0 {
			delete(podExecSlots.users, p.user)
		}
		delete(podExecSlots.sessions, p.session)
	}, nil
}

// ExecPod accepts only a matching, single-use, human-approved prepared plan.
func (r *Runner) ExecPod(ctx context.Context, req cmdplan.PodExecRequest) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !r.execPod.Enabled {
		return Result{}, fmt.Errorf("TOOL_UNAVAILABLE: EXEC_DISABLED")
	}
	p, ok := ctx.Value(podExecKey{}).(*podExecPermit)
	if !ok || p.runner != r || !p.approved.Load() {
		return Result{}, fmt.Errorf("POLICY_DENIED: Pod exec requires a prepared human approval")
	}
	if req.Container == "" {
		req.Container = p.request.Container
	}
	if req.TimeoutSeconds == 0 {
		req.TimeoutSeconds = r.execPod.TimeoutSeconds
	}
	req, err := cmdplan.NormalizePodExec(req)
	if err != nil {
		return Result{}, err
	}
	if execDigest(req) != p.requestDigest || !p.used.CompareAndSwap(false, true) {
		return Result{}, fmt.Errorf("CONFLICT: PLAN_CHANGED_OR_CONSUMED")
	}
	release, err := r.acquirePodExec(p)
	if err != nil {
		return Result{}, err
	}
	defer release()
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return Result{}, err
		}
	}
	pod, err := r.cs.CoreV1().Pods(req.Namespace).Get(ctx, req.Pod, metav1.GetOptions{})
	if err != nil {
		return Result{}, err
	}
	snapshot, err := podExecSnapshot(pod, req.Container)
	if err != nil {
		return Result{}, err
	}
	if snapshot != p.snapshot {
		return Result{}, fmt.Errorf("CONFLICT: POD_OR_CONTAINER_CHANGED")
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	// Resolve every explicit path before traversing it. Failure never falls back
	// to lexical checks; the helper is part of this exact approved diagnostic.
	argv := append([]string(nil), req.Argv...)
	for i := 1; i < len(argv); i++ {
		if !strings.HasPrefix(argv[i], "/") {
			continue
		}
		if err := validatePodPath(pod, req.Container, argv[i], req.Argv[0] == "du"); err != nil {
			return Result{}, err
		}
		out, _, code, e := r.streamPodExec(runCtx, req, []string{"readlink", "-f", argv[i]}, 4096)
		if e != nil || code != 0 {
			return Result{}, fmt.Errorf("UNSUPPORTED: PATH_CANONICALIZATION_UNAVAILABLE")
		}
		resolved := strings.TrimSpace(out)
		if err := validatePodPath(pod, req.Container, resolved, req.Argv[0] == "du"); err != nil {
			return Result{}, err
		}
		argv[i] = resolved
	}
	started := time.Now()
	stdout, stderr, code, runErr := r.streamPodExec(runCtx, req, argv, min(r.execPod.MaxOutputBytes, r.maxBytes))
	res := Result{Command: fmt.Sprintf("exec_pod %s/%s container=%s argv=%q", req.Namespace, req.Pod, req.Container, req.Argv), Stdout: redactor.Redact(stdout), Stderr: redactor.Redact(stderr), ExitCode: code, ExecutionLocation: "pod", DiagnosticRule: cmdplan.PodExecPolicyVersion + "/" + req.Argv[0]}
	res.PlanFingerprint = fmt.Sprintf("%x", p.digest)
	res.PodTarget = &PodExecTarget{Namespace: req.Namespace, Pod: req.Pod, PodUID: string(pod.UID), Container: req.Container}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == req.Container {
			res.PodTarget.ContainerID = status.ContainerID
		}
	}
	res.DurationMs = time.Since(started).Milliseconds()
	if strings.Contains(stdout, "[TRUNCATED]") || strings.Contains(stderr, "[TRUNCATED]") {
		res.Truncated = true
	}
	if runCtx.Err() != nil {
		res.ExitCode = 124
		res.TimedOut = true
		return res, fmt.Errorf("TIMEOUT: Pod exec stream cancelled; remote termination is unconfirmed")
	}
	return res, runErr
}

type execCapture struct {
	sync.Mutex
	remaining int
	truncated bool
}
type execOutput struct {
	shared *execCapture
	b      strings.Builder
}

func (w *execOutput) Write(b []byte) (int, error) {
	w.shared.Lock()
	defer w.shared.Unlock()
	n := len(b)
	take := min(n, w.shared.remaining)
	w.b.Write(b[:take])
	w.shared.remaining -= take
	if take < n {
		w.shared.truncated = true
	}
	return n, nil
}

func (r *Runner) streamPodExec(ctx context.Context, req cmdplan.PodExecRequest, argv []string, maxBytes int) (string, string, int, error) {
	if err := ctx.Err(); err != nil {
		return "", "", 1, err
	}
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return "", "", 1, err
		}
	}
	// A test seam injects the transport only; all approval and policy guards run.
	shared := &execCapture{remaining: maxBytes}
	out := &execOutput{shared: shared}
	errOut := &execOutput{shared: shared}
	var err error
	if r.podStream != nil {
		err = r.podStream(ctx, req, argv, out, errOut)
	} else {
		url := r.cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(req.Namespace).Name(req.Pod).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: req.Container, Command: argv, Stdout: true, Stderr: true, Stdin: false, TTY: false}, scheme.ParameterCodec).URL()
		var stream remotecommand.Executor
		transport, upgrader, transportErr := spdy.RoundTripperFor(r.restConfig)
		err = transportErr
		if err == nil {
			stream, err = remotecommand.NewSPDYExecutorRejectRedirects(transport, upgrader, http.MethodPost, url)
		}
		if err == nil {
			err = stream.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: out, Stderr: errOut})
		}
	}
	code := 0
	if err != nil {
		if exit, ok := err.(utilexec.ExitError); ok {
			code = exit.ExitStatus()
			err = nil
		} else {
			code = 1
		}
	}
	stdout, stderr := out.b.String(), errOut.b.String()
	if shared.truncated {
		stdout += "\n[TRUNCATED]"
	}
	return stdout, stderr, code, err
}

var _ io.Writer = (*execOutput)(nil)
