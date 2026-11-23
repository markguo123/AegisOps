package cluster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Connector 解析 kubeconfig 并验证可连接性（P3.2；测试隔离点）。
type Connector interface {
	Inspect(ctx context.Context, kubeconfig []byte) (*Info, error)
}

// RemoteConnector 连接真实集群的默认 Connector。
type RemoteConnector struct {
	// Timeout 覆盖单次连测的 rest 超时；零值用 30s。
	Timeout time.Duration
}

// Inspect 先解析 kubeconfig（结构/上下文/集群），再实际连接一次拿服务器版本与就绪状态。
// 任何解析或连接失败都返回带 ErrInvalid 包装的明确错误，供 CREATE/TEST 接口使用。
func (rc *RemoteConnector) Inspect(ctx context.Context, data []byte) (*Info, error) {
	info, err := ParseKubeconfig(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	cfg, err := buildRESTConfig(data, rc.Timeout)
	if err != nil {
		return nil, fmt.Errorf("%w: build rest config: %v", ErrInvalid, err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%w: create clientset: %v", ErrInvalid, err)
	}
	version, err := cs.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("%w: connect failed: %v", ErrInvalid, err)
	}
	info.Version = version.GitVersion
	info.Health = probeReadyz(ctx, cs.Discovery().RESTClient(), rc.Timeout)
	// 稳定身份依据：kube-system namespace UID。读取它只需要 get namespace，
	// 不需要 cluster-admin；RBAC 不允许时留空，由调用方按“无法验证”处理。
	if ns, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{}); err == nil {
		info.Identity = string(ns.UID)
	}
	return info, nil
}

// ParseContexts 只解析 kubeconfig 的 context 列表，不连测、不落库、不保留明文。
func ParseContexts(data []byte) (*ContextsView, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("kubeconfig is empty")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if err := validatePortable(cfg); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(cfg.Contexts))
	for name := range cfg.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("kubeconfig has no context")
	}
	current := cfg.CurrentContext
	if current == "" {
		current = names[0]
	}
	return &ContextsView{Current: current, Contexts: names}, nil
}

// selectContext 把用户显式选择的 context 固定为 kubeconfig 的 current-context。
// 空选择沿用文件里的 current-context；两者都没有时按名称排序取第一个，
// 保证行为确定而不是依赖 map 遍历顺序。返回写回后的完整 kubeconfig。
func selectContext(data []byte, name string) ([]byte, string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, "", errors.New("kubeconfig is empty")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	if err := validatePortable(cfg); err != nil {
		return nil, "", err
	}
	selected := strings.TrimSpace(name)
	if selected == "" {
		selected = cfg.CurrentContext
	}
	if selected == "" {
		names := make([]string, 0, len(cfg.Contexts))
		for n := range cfg.Contexts {
			names = append(names, n)
		}
		sort.Strings(names)
		if len(names) == 0 {
			return nil, "", errors.New("kubeconfig has no context")
		}
		selected = names[0]
	}
	kc := cfg.Contexts[selected]
	if kc == nil {
		return nil, "", fmt.Errorf("kubeconfig context %q not found", selected)
	}
	if cc := cfg.Clusters[kc.Cluster]; cc == nil || cc.Server == "" {
		return nil, "", fmt.Errorf("kubeconfig context %q has no cluster server", selected)
	}
	cfg.CurrentContext = selected
	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return nil, "", fmt.Errorf("write selected kubeconfig: %w", err)
	}
	return out, selected, nil
}

// CheckSummary 把连测错误归类成可展示的原因，不保存原始错误（其中可能含
// 集群地址或认证细节）。只有有证据的类别才细分，其余统一为“连接失败”。
func CheckSummary(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "x509"), strings.Contains(msg, "tls"),
		strings.Contains(msg, "certificate"), strings.Contains(msg, "unknown authority"):
		return "TLS/CA 校验失败"
	case strings.Contains(msg, "unauthorized"), strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "401"), strings.Contains(msg, "403"):
		return "认证或授权失败"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "dial tcp"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no route to host"), strings.Contains(msg, "network is unreachable"):
		return "网络不可达或超时"
	default:
		return "连接失败"
	}
}

const checkSuccessSummary = "API Server 可达；凭据有效性及业务资源权限尚未完全验证"

// ParseKubeconfig 校验 kubeconfig 结构并提取当前 context/cluster server（不含凭据）。
// 与内网只读门面共用 clientcmd：同一解析口径，错误路径可读。
func ParseKubeconfig(data []byte) (*Info, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("kubeconfig is empty")
	}
	cfg, err := clientcmd.Load(data)
	if err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	if err := validatePortable(cfg); err != nil {
		return nil, err
	}
	ctxName := cfg.CurrentContext
	if ctxName == "" {
		for name := range cfg.Contexts {
			ctxName = name
			break
		}
	}
	if ctxName == "" {
		return nil, errors.New("kubeconfig has no context")
	}
	kc := cfg.Contexts[ctxName]
	if kc == nil {
		return nil, fmt.Errorf("kubeconfig context %q not found", ctxName)
	}
	cc := cfg.Clusters[kc.Cluster]
	if cc == nil {
		return nil, fmt.Errorf("kubeconfig context %q has no cluster", ctxName)
	}
	if cc.Server == "" {
		return nil, fmt.Errorf("kubeconfig cluster %q has no server", kc.Cluster)
	}
	return &Info{Name: ctxName, Context: ctxName, Server: cc.Server}, nil
}

func buildRESTConfig(data []byte, timeout time.Duration) (*rest.Config, error) {
	cfg, err := clientcmd.NewClientConfigFromBytes(data)
	if err != nil {
		return nil, err
	}
	rc, err := cfg.ClientConfig()
	if err != nil {
		return nil, err
	}
	if timeout > 0 {
		rc.Timeout = timeout
	}
	return rc, nil
}

func probeReadyz(ctx context.Context, rc rest.Interface, timeout time.Duration) string {
	req := rc.Get().AbsPath("/readyz")
	if timeout > 0 {
		req = req.Timeout(timeout)
	}
	result := req.Do(ctx)
	var code int
	result = result.StatusCode(&code)
	if result.Error() != nil {
		return "unknown"
	}
	if code == http.StatusOK {
		return "ok"
	}
	return "unknown"
}
