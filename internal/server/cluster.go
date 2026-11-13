package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// maxKubeconfigBody 限制 kubeconfig 提交体（1MB，防滥用且足够承载完整配置）。
const maxKubeconfigBody = 1 << 20

const clusterUnavailableMessage = "cluster service is not configured: set AEGISOPS_ENC_KEY to a valid 32-byte hex or base64 key"

// clusterService 是集群管理端点依赖的应用服务接口（cluster.Service 满足）。
// Handler 只依赖该接口，不直接触碰 Store 或 Cipher（需求 v1.2 §24）。
type clusterService interface {
	Create(ctx context.Context, req cluster.CreateRequest) (*store.Cluster, error)
	Test(ctx context.Context, req cluster.TestRequest) (*cluster.Info, error)
	Contexts(ctx context.Context, req cluster.ContextsRequest) (*cluster.ContextsView, error)
	Update(ctx context.Context, id string, req cluster.UpdateRequest) (*cluster.StatusView, error)
	List(ctx context.Context) ([]*store.Cluster, error)
	Get(ctx context.Context, id string) (*store.Cluster, error)
	Current(ctx context.Context) (*cluster.StatusView, error)
	Refresh(ctx context.Context, id string) (*cluster.StatusView, error)
	Delete(ctx context.Context, id string) error
}

// handleCreateCluster POST /api/v1/clusters：解析 + 连测 + 加密落库。
func (s *Server) handleCreateCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	var req cluster.CreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKubeconfigBody)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid body: "+err.Error())
		return
	}
	c, err := s.clusterSvc.Create(r.Context(), req)
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusCreated, c)
}

// handleTestCluster POST /api/v1/clusters/test：只解析 + 连测，不落库、不保留明文。
func (s *Server) handleTestCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	var req cluster.TestRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKubeconfigBody)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid body: "+err.Error())
		return
	}
	info, err := s.clusterSvc.Test(r.Context(), req)
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, info)
}

// handleClusterContexts POST /api/v1/clusters/contexts：只解析 kubeconfig
// 列出可选 context，供接入/更新流程显式选择目标；不连测、不落库。
func (s *Server) handleClusterContexts(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	var req cluster.ContextsRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKubeconfigBody)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid body: "+err.Error())
		return
	}
	view, err := s.clusterSvc.Contexts(r.Context(), req)
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, view)
}

// handleUpdateCluster PATCH /api/v1/clusters/{id}：管理员原位替换凭据。
// 服务层保证候选先连测、身份一致、失败保留旧值；响应不含任何凭据内容。
func (s *Server) handleUpdateCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	var req cluster.UpdateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxKubeconfigBody)).Decode(&req); err != nil {
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, "invalid body: "+err.Error())
		return
	}
	view, err := s.clusterSvc.Update(r.Context(), r.PathValue("id"), req)
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, view)
}

// handleListClusters GET /api/v1/clusters：受管集群元数据（不含凭据）。
func (s *Server) handleListClusters(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	list, err := s.clusterSvc.List(r.Context())
	if err != nil {
		slog.Warn("server: 集群列表读取失败", "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "list clusters failed")
		return
	}
	writeOK(w, r, http.StatusOK, list)
}

// handleGetCluster GET /api/v1/clusters/{id}：单个受管集群元数据。
func (s *Server) handleGetCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	c, err := s.clusterSvc.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, c)
}

// handleCurrentCluster GET /api/v1/clusters/current：刷新并返回"当前集群"实时状态。
func (s *Server) handleCurrentCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	view, err := s.clusterSvc.Current(r.Context())
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, view)
}

// handleCheckCluster POST /api/v1/clusters/{id}/check：管理员对指定目标连测，
// 刷新检查时间、状态与分类原因。连接成功不代表具备全部业务权限。
func (s *Server) handleCheckCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	view, err := s.clusterSvc.Refresh(r.Context(), r.PathValue("id"))
	if err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, view)
}

// handleDeleteCluster DELETE /api/v1/clusters/{id}。
func (s *Server) handleDeleteCluster(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	if err := s.clusterSvc.Delete(r.Context(), r.PathValue("id")); err != nil {
		writeClusterError(w, r, err)
		return
	}
	writeOK(w, r, http.StatusOK, map[string]string{"deleted": "ok"})
}

// writeClusterError 把集群服务错误映射为统一错误信封。
func writeClusterError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, cluster.ErrInvalid):
		writeError(w, r, http.StatusBadRequest, codeInvalidArgument, err.Error())
	case errors.Is(err, cluster.ErrIdentityUnverified), errors.Is(err, cluster.ErrIdentityMismatch),
		errors.Is(err, cluster.ErrServerChanged):
		// 凭据/目标一致性属于业务冲突，不是服务端故障：客户端必须看到可指导的
		// 原因并保留旧值。
		writeError(w, r, http.StatusConflict, codeConflict, err.Error())
	case errors.Is(err, cluster.ErrNotFound):
		writeError(w, r, http.StatusNotFound, codeNotFound, err.Error())
	case errors.Is(err, cluster.ErrExists):
		writeError(w, r, http.StatusConflict, codeConflict, err.Error())
	default:
		slog.Warn("server: 集群操作失败", "error", err)
		writeError(w, r, http.StatusInternalServerError, codeInternal, "cluster operation failed")
	}
}
