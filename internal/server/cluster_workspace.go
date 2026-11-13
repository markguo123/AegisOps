package server

import (
	"net/http"
	"sort"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/authz"
	"github.com/k8s-ai/k8s-ai/internal/cluster"
	"github.com/k8s-ai/k8s-ai/internal/permission"
	"github.com/k8s-ai/k8s-ai/internal/store"
)

// clusterCheckStaleAfter 是“最近连测结果仍可视为近期状态”的窗口。超过窗口的
// 成功结果只能标成“已过期”，不能显示为实时健康（方案 22 §9）。
const clusterCheckStaleAfter = 24 * time.Hour

// clusterWorkspace 是多集群工作台的读模型（P12.4）。所有字段来自服务端可信
// 数据：集群元数据来自注册表，可用范围来自当前用户授权，检查时间来自最近一次
// 连测；前端传参不参与可见性判定。
type clusterWorkspace struct {
	DataSource  string                 `json:"dataSource"`
	GeneratedAt time.Time              `json:"generatedAt"`
	Summary     clusterWorkspaceStats  `json:"summary"`
	Items       []clusterWorkspaceItem `json:"items"`
}

type clusterWorkspaceStats struct {
	Total     int `json:"total"`
	Connected int `json:"connected"`
	Failed    int `json:"failed"`
	Unchecked int `json:"unchecked"`
}

type clusterWorkspaceItem struct {
	Cluster *store.Cluster `json:"cluster"`
	// CheckState 是稳定的展示口径：ok | failed | unchecked | stale。
	CheckState string `json:"checkState"`
	// IdentityRecorded 表示该集群是否已记录可验证身份；未记录时原位凭据更新
	// 会被拒绝，需要重新接入。
	IdentityRecorded bool `json:"identityRecorded"`
	// Access 是当前用户在该集群的有效授权范围（只读展示，不参与执行判定）。
	Access []authz.AccessRule `json:"access"`
	// 操作可见性由权限位过滤；后端每个请求仍逐次核权。
	CanManage      bool `json:"canManage"`
	CanChat        bool `json:"canChat"`
	CanScan        bool `json:"canScan"`
	CanViewReports bool `json:"canViewReports"`
}

// handleClusterWorkspace GET /api/v1/clusters/workspace：集群工作台。
//
// 普通用户只看到 EffectiveAccess 覆盖的集群；管理员看到全部受管集群。
// 工作台从不把“连接成功”表述成“具备全部业务权限”，也从不把过期结果标成实时健康。
func (s *Server) handleClusterWorkspace(w http.ResponseWriter, r *http.Request) {
	if s.clusterSvc == nil {
		writeError(w, r, http.StatusNotImplemented, codeInternal, clusterUnavailableMessage)
		return
	}
	u := UserFrom(r.Context())
	if u == nil {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, "authentication required")
		return
	}
	role := RoleFrom(r.Context())
	canManage := role != nil && permission.HasPermission(role.Permissions, permission.ClusterManage)
	canScan := role != nil && permission.HasPermission(role.Permissions, permission.ScanExecute)

	var accessible []authz.AccessibleCluster
	if s.platform != nil {
		list, err := s.platform.Grants.AccessibleClusters(r.Context(), u.ID)
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "access unavailable")
			return
		}
		accessible = list
	} else {
		// 无平台授权面的终端直用形态：保留旧行为，列出全部已登记集群。
		list, err := s.clusterSvc.List(r.Context())
		if err != nil {
			writeError(w, r, http.StatusInternalServerError, codeInternal, "list clusters failed")
			return
		}
		for _, c := range list {
			accessible = append(accessible, authz.AccessibleCluster{ID: c.ID, Name: c.Name, Status: c.Status})
		}
	}

	now := time.Now().UTC()
	out := clusterWorkspace{
		DataSource:  "平台受管集群注册表；检查时间为最近一次连测",
		GeneratedAt: now,
		Items:       make([]clusterWorkspaceItem, 0, len(accessible)),
	}
	for _, a := range accessible {
		c, err := s.clusterSvc.Get(r.Context(), a.ID)
		if err != nil {
			// 已删除/无凭据的目标不出现在工作台；不要用一个残缺条目冒充可用目标。
			continue
		}
		state := clusterCheckState(c, now)
		item := clusterWorkspaceItem{
			Cluster:          c,
			CheckState:       state,
			IdentityRecorded: c.Identity != "",
			CanManage:        canManage,
			CanChat:          true,
			CanScan:          canScan,
			CanViewReports:   true,
			Access:           []authz.AccessRule{},
		}
		if a.Access != nil {
			item.Access = a.Access.Rules
		}
		if s.platform != nil && len(item.Access) == 0 {
			// 无任何有效授权的目标只作为不可用条目出现；入口全部关闭，
			// 由后端逐请求再次拒绝，前端隐藏不是安全机制。
			item.CanChat = false
			item.CanScan = false
			item.CanViewReports = false
		}
		out.Items = append(out.Items, item)
		switch state {
		case "ok", "stale":
			out.Summary.Connected++
		case "failed":
			out.Summary.Failed++
		default:
			out.Summary.Unchecked++
		}
	}
	out.Summary.Total = len(out.Items)
	sort.SliceStable(out.Items, func(i, j int) bool {
		return out.Items[i].Cluster.Name < out.Items[j].Cluster.Name
	})
	writeOK(w, r, http.StatusOK, out)
}

// clusterCheckState 把状态与检查时间翻译成工作台口径。状态本身是最近一次连测
// 的结论；没有检查时间的历史数据只能显示“未检查”，成功超过窗口则显示“已过期”。
func clusterCheckState(c *store.Cluster, now time.Time) string {
	if c.CheckedAt == nil {
		return "unchecked"
	}
	switch c.Status {
	case string(cluster.StatusConnected):
		if now.Sub(*c.CheckedAt) > clusterCheckStaleAfter {
			return "stale"
		}
		return "ok"
	case string(cluster.StatusDisconnected), string(cluster.StatusError):
		return "failed"
	default:
		return "unchecked"
	}
}
