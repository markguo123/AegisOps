// Package cluster 提供四期平台化的集群管理能力（P3.1-P3.3）：
// 受管集群元数据、与集群分离的加密凭据、kubeconfig 解析/校验/连接测试与状态刷新。
//
// 安全约定（AGENTS.md LLM Safety）：kubeconfig 明文只在本包内临时出现；落库前
// 必须经 Cipher 加密，读接口永不回文明文，凭据值不进入 LLM/Session/Audit/日志。
package cluster

import (
	"errors"

	"github.com/k8s-ai/k8s-ai/internal/store"
)

// 服务层统一错误（供 API Handler 做状态码映射）。
var (
	ErrInvalid  = errors.New("cluster: invalid request")
	ErrExists   = errors.New("cluster: already exists")
	ErrNotFound = errors.New("cluster: not found")
	// ErrIdentityUnverified 表示无法证明候选凭据仍指向原逻辑集群（旧记录没有
	// 身份依据，或候选凭据 RBAC 读不到身份对象）。原位替换必须拒绝。
	ErrIdentityUnverified = errors.New("cluster: target identity cannot be verified")
	// ErrIdentityMismatch 表示候选凭据明确指向另一个集群。禁止静默继承 ClusterID。
	ErrIdentityMismatch = errors.New("cluster: target identity changed")
	// ErrServerChanged 表示候选凭据的 API Server 地址与已登记值不同，需要管理员
	// 单独确认旧/新目标后才能继续。
	ErrServerChanged = errors.New("cluster: api server changed")
)

// Status 是受管集群的连接状态。
type Status string

const (
	StatusUnknown      Status = "unknown"
	StatusConnected    Status = "connected"
	StatusDisconnected Status = "disconnected"
	StatusError        Status = "error"
)

// Info 是一次 kubeconfig 解析/连测得到的集群信息（P3.2/P3.3）。
type Info struct {
	Name    string `json:"name,omitempty"`
	Context string `json:"context,omitempty"`
	Server  string `json:"server,omitempty"`
	Version string `json:"version,omitempty"`
	Health  string `json:"health,omitempty"`
	// Identity 是连测时读取到的稳定集群身份（kube-system namespace UID）。
	// 读取不到时为空，调用方必须按“无法证明是同一集群”处理，不能回落到
	// 名称/地址相同就当作同一集群（方案 22 §8.2）。完整 UID 不随 API 返回。
	Identity string `json:"-"`
}

// CreateRequest 是 POST /api/v1/clusters 的请求体。
type CreateRequest struct {
	Name       string `json:"name,omitempty"`
	Context    string `json:"context,omitempty"`
	Kubeconfig string `json:"kubeconfig"`
}

// TestRequest 是 POST /api/v1/clusters/test 的请求体。
type TestRequest struct {
	Context    string `json:"context,omitempty"`
	Kubeconfig string `json:"kubeconfig"`
}

// ContextsRequest 是 POST /api/v1/clusters/contexts 的请求体：只解析
// kubeconfig 列出可选 context，不连测、不落库、不保留明文。
type ContextsRequest struct {
	Kubeconfig string `json:"kubeconfig"`
}

// ContextsView 是 kubeconfig 中可供显式选择的 context 列表。
type ContextsView struct {
	Current  string   `json:"current,omitempty"`
	Contexts []string `json:"contexts"`
}

// UpdateRequest 是 PATCH /api/v1/clusters/{id} 的请求体：管理员原位替换
// 同一逻辑集群的凭据。服务端先解析并连测候选凭据，再按已记录的集群身份
// 核对一致性；失败保留旧值。
type UpdateRequest struct {
	Name                string `json:"name,omitempty"`
	Context             string `json:"context,omitempty"`
	Kubeconfig          string `json:"kubeconfig"`
	ConfirmServerChange bool   `json:"confirmServerChange,omitempty"`
}

// StatusView 是 GET /api/v1/clusters/current 的响应载体。
type StatusView struct {
	Cluster *store.Cluster `json:"cluster"`
	Info    Info           `json:"info"`
}
