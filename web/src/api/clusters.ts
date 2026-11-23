/**
 * P3.2/P3.3/P12 集群管理 API 客户端（与后端 `internal/server/cluster.go` 和
 * `cluster_workspace.go` 一致）。
 *
 * 安全约定：kubeconfig 只在 create/test/contexts/update 请求体的内存中出现一次，
 * 客户端本地不缓存明文；列表 / 状态 / 工作台响应只含受管元数据。
 */
import { ApiRequestError, apiDelete, apiGet, apiPatch, apiPost } from './request'
import type {
  Cluster,
  ClusterContexts,
  ClusterCreateRequest,
  ClusterInfo,
  ClusterStatusView,
  ClusterTestRequest,
  ClusterUpdateRequest,
  ClusterWorkspace,
} from './types'

/** 受管集群列表（不含凭据）。 */
export async function listClusters(): Promise<Cluster[]> {
  const env = await apiGet<Cluster[]>('/clusters')
  return env.data ?? []
}

/** 多集群工作台：只返回当前用户有权访问的目标及其检查状态/授权范围。 */
export async function clusterWorkspace(): Promise<ClusterWorkspace> {
  const env = await apiGet<ClusterWorkspace>('/clusters/workspace')
  if (!env.data) {
    throw new Error('集群工作台数据为空')
  }
  return env.data
}

/** 当前集群实时状态（版本 / Context / 健康/断连）。 */
export async function currentCluster(): Promise<ClusterStatusView> {
  const env = await apiGet<ClusterStatusView>('/clusters/current')
  if (!env.data) {
    throw new Error('当前集群状态为空')
  }
  return env.data
}

/** 只解析 kubeconfig 的 context 列表；不连测、不落库、不保留明文。 */
export async function clusterContexts(kubeconfig: string): Promise<ClusterContexts> {
  const env = await apiPost<ClusterContexts>('/clusters/contexts', { kubeconfig })
  if (!env.data) {
    throw new Error('kubeconfig context 列表为空')
  }
  return env.data
}

/** 仅连测，不落库、不保留明文。 */
export async function testCluster(body: ClusterTestRequest): Promise<ClusterInfo> {
  const env = await apiPost<ClusterInfo>('/clusters/test', body)
  if (!env.data) {
    throw new Error('连接测试结果为空')
  }
  return env.data
}

/** 上传 / 粘贴 kubeconfig 并加密落库。 */
export async function createCluster(body: ClusterCreateRequest): Promise<Cluster> {
  const env = await apiPost<Cluster>('/clusters', body)
  if (!env.data) {
    throw new Error('创建集群结果为空')
  }
  return env.data
}

/** 原位替换同一逻辑集群的凭据（保留 ClusterID）。 */
export async function updateCluster(
  id: string,
  body: ClusterUpdateRequest,
): Promise<ClusterStatusView> {
  const env = await apiPatch<ClusterStatusView>(`/clusters/${encodeURIComponent(id)}`, body)
  if (!env.data) {
    throw new Error('更新集群凭据结果为空')
  }
  return env.data
}

/** 管理员对指定目标重新连测并刷新检查时间/状态。 */
export async function checkCluster(id: string): Promise<ClusterStatusView> {
  const env = await apiPost<ClusterStatusView>(`/clusters/${encodeURIComponent(id)}/check`)
  if (!env.data) {
    throw new Error('连测结果为空')
  }
  return env.data
}

/** 删除受管集群与其加密凭据。 */
export async function deleteCluster(id: string): Promise<void> {
  const env = await apiDelete<Record<string, string>>(`/clusters/${encodeURIComponent(id)}`)
  if (env.error) {
    throw new Error(env.error.message)
  }
}

/** 统一错误文案，供页面直接展示（不透出请求体内容）。 */
export function describeClusterError(method: string, error: unknown): string {
  if (error instanceof ApiRequestError && error.status === 501) {
    return '集群管理能力未启用，请联系平台管理员配置凭据加密密钥后重试'
  }
  if (error instanceof Error && error.message) {
    return error.message
  }
  return `${method}失败，请稍后重试`
}

// 保持 DefaultExport 便于 tree-shaking 之外的使用方统一 import 默认值。
export default {
  listClusters,
  clusterWorkspace,
  currentCluster,
  clusterContexts,
  testCluster,
  createCluster,
  updateCluster,
  checkCluster,
  deleteCluster,
}
