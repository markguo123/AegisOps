package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/k8s-ai/k8s-ai/internal/cmdplan"
	"github.com/k8s-ai/k8s-ai/internal/k8sgateway"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

type kubectlPermitKey struct{}
type proxyConfigKey struct{}
type kubectlPermit struct {
	runner     *Runner
	digest     [32]byte
	authorized atomic.Bool
	used       atomic.Bool
	readOnly   bool
}

// ProxiedKubectl reports a complete platform execution channel, not user rights.
func (r *Runner) ProxiedKubectl() bool {
	return r.targetBound && r.restConfig != nil && r.requestCheck != nil
}

func (r *Runner) PrepareKubectl(ctx context.Context, command string) (context.Context, error) {
	if !r.ProxiedKubectl() {
		return ctx, nil
	}
	if err := validateProxyCommand(command); err != nil {
		return ctx, err
	}
	if r.targetCheck != nil {
		if err := r.targetCheck(ctx); err != nil {
			return ctx, err
		}
	}
	p := &kubectlPermit{runner: r, digest: sha256.Sum256([]byte(command)), readOnly: cmdplan.Analyze(command).ProvablyReadOnly}
	return context.WithValue(ctx, kubectlPermitKey{}, p), nil
}

func (r *Runner) AuthorizeKubectl(ctx context.Context, command string) (context.Context, error) {
	if !r.ProxiedKubectl() {
		return ctx, nil
	}
	p, ok := ctx.Value(kubectlPermitKey{}).(*kubectlPermit)
	if !ok || p.runner != r || p.digest != sha256.Sum256([]byte(command)) || !p.authorized.CompareAndSwap(false, true) {
		return ctx, fmt.Errorf("POLICY_DENIED: missing current kubectl execution permit")
	}
	return ctx, nil
}

// validateProxyCommand constrains local execution mechanics only. Kubernetes
// resources/actions are authorized from actual requests, never from this list.
func validateProxyCommand(command string) error {
	if cmdplan.Analyze(command).ProvablyReadOnly {
		return nil
	}
	pc, err := ParseCommandDetail(command)
	if err != nil {
		return fmt.Errorf("UNSUPPORTED: %w", err)
	}
	if pc.IsShell {
		return fmt.Errorf("TARGET_ISOLATION_DENIED: real shell is unavailable in platform mode")
	}
	if err := validateBoundTargetArgv(pc.Argv); err != nil {
		return err
	}
	for _, a := range pc.Argv[1:] {
		if strings.Contains(a, "template-file") || strings.HasPrefix(a, "-v") || strings.HasPrefix(a, "--v") || strings.HasPrefix(a, "--log") || (strings.HasPrefix(a, "-f") && a != "-f") || (strings.HasPrefix(a, "-k") && a != "-k") {
			return fmt.Errorf("POLICY_DENIED: local file or diagnostic logging flags are unavailable")
		}
		k, _, _ := strings.Cut(a, "=")
		switch k {
		case "-f", "--filename", "-k", "--kustomize", "--template", "--template-file", "--kuberc", "--profile", "--profile-output", "--cache-dir", "--output-watch-events":
			return fmt.Errorf("POLICY_DENIED: local files, external inputs and executable extensions are unavailable")
		}
	}
	return nil
}

func (r *Runner) commandEnv(ctx context.Context) []string {
	env := r.childEnv()
	config, ok := ctx.Value(proxyConfigKey{}).(string)
	if !ok {
		return env
	}
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, "KUBECONFIG=") {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, "KUBECONFIG="+config)
}

func (r *Runner) startKubectlProxy(ctx context.Context, command string) (context.Context, func(), error) {
	p, ok := ctx.Value(kubectlPermitKey{}).(*kubectlPermit)
	if !ok || p.runner != r || p.digest != sha256.Sum256([]byte(command)) || !p.authorized.Load() || !p.used.CompareAndSwap(false, true) {
		return ctx, nil, fmt.Errorf("POLICY_DENIED: kubectl requires a matching single-use execution permit")
	}
	if err := validateProxyCommand(command); err != nil {
		return ctx, nil, err
	}
	gateway := r.gateway
	targetID := r.targetID
	if targetID == "" {
		targetID = r.restConfig.Host
	}
	if gateway == nil {
		gateway = k8sgateway.NewInProcess(func(_ context.Context, target string) (k8sgateway.BoundTarget, error) {
			if target != targetID {
				return k8sgateway.BoundTarget{}, fmt.Errorf("TARGET_ISOLATION_DENIED: target mismatch")
			}
			return k8sgateway.BoundTarget{Config: r.restConfig, Authorize: r.requestCheck, Validate: r.targetCheck}, nil
		})
	}
	lease, err := gateway.Open(ctx, k8sgateway.OpenRequest{TargetID: targetID, ExecutionID: rand.Text(), ReadOnly: p.readOnly})
	if err != nil {
		return ctx, nil, err
	}
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["bound"] = &clientcmdapi.Cluster{Server: lease.Endpoint, CertificateAuthorityData: lease.CAData}
	cfg.AuthInfos["channel"] = &clientcmdapi.AuthInfo{Token: lease.Token}
	cfg.Contexts["bound"] = &clientcmdapi.Context{Cluster: "bound", AuthInfo: "channel"}
	cfg.CurrentContext = "bound"
	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		lease.Close()
		return ctx, nil, err
	}
	file, remove, err := TemporaryKubeconfig(raw)
	clear(raw)
	if err != nil {
		lease.Close()
		return ctx, nil, err
	}
	cleanup := func() { lease.Close(); remove() }
	return context.WithValue(ctx, proxyConfigKey{}, file), cleanup, nil
}
