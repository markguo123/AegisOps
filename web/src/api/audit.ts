/**
 * P4.7 审计中心 API 客户端（与后端 `internal/server/audit.go`、
 * `internal/service/audit_query.go` 一致）。
 *
 * 三条口径写在这里，避免页面各写一套：
 *  1. 鉴权全在后端：列表与详情都要求 audit.read，前端菜单只是显隐；普通用户
 *     直调接口会拿到 403。
 *  2. 只渲染后端给的字段白名单，不请求、不猜测存储层 Payload。
 *  3. 游标分页是不透明字符串：只回传，不解析。
 */
import { apiGet } from './request'
import type { AuditDetailResult, AuditListResult } from './types'

/** 审计列表查询条件（后端 parseAuditFilter 的 query 参数）。 */
export interface AuditListOptions {
  /** RFC3339 时间；空串表示不限。 */
  from?: string
  to?: string
  eventType?: string
  userId?: string
  clusterId?: string
  sessionId?: string
  status?: string
  action?: string
  /** 默认 50、上限 100（后端同样收敛）。 */
  limit?: number
  cursor?: string
}

/**
 * 事件类型目录：与后端 `internal/store/audit_event.go` 的事件常量一一对应。
 * 新增类型必须同时加进后端 EventTypeForAction 与 EventType 常量，否则页面只能
 * 显示原始英文串。
 */
export const AUDIT_EVENT_TYPES: ReadonlyArray<{ value: string; label: string }> = [
  { value: 'LOGIN', label: '登录' },
  { value: 'LOGOUT', label: '登出' },
  { value: 'SESSION', label: '会话' },
  { value: 'APPROVAL', label: '人工确认' },
  { value: 'GRANT', label: '会话授权' },
  { value: 'TOOL_CALL', label: '工具调用' },
  { value: 'RESOURCE_MUTATION', label: '资源变更' },
  { value: 'K8S_ACCESS', label: 'Kubernetes 访问' },
  { value: 'CONFIG_CHANGE', label: '配置变更' },
  { value: 'SKILL_RUN', label: 'Skill 运行' },
  { value: 'CLUSTER_CHANGE', label: '集群变更' },
  { value: 'REPORT', label: '报告' },
  { value: 'SECRET_READ', label: 'Secret 访问' },
  { value: 'USER_INPUT', label: '用户输入' },
  { value: 'LLM_CALL', label: '模型调用' },
  { value: 'OTHER', label: '其他' },
]

const EVENT_TYPE_LABELS: Record<string, string> = Object.fromEntries(
  AUDIT_EVENT_TYPES.map((item) => [item.value, item.label]),
)

export type AuditTone = 'info' | 'success' | 'warning' | 'danger' | 'mute'

/** 事件类型 → 中文短名；未知类型原样显示，不吞信息。 */
export function auditEventLabel(type: string): string {
  return EVENT_TYPE_LABELS[type] ?? (type || '未记录')
}

/** 事件类型 → 胶囊色调：变更类偏警示，读/登录类偏中性。 */
export function auditEventTone(type: string): AuditTone {
  switch (type) {
    case 'RESOURCE_MUTATION':
    case 'CLUSTER_CHANGE':
      return 'warning'
    case 'APPROVAL':
    case 'GRANT':
      return 'info'
    case 'SECRET_READ':
    case 'REPORT':
      return 'mute'
    case 'CONFIG_CHANGE':
    case 'SKILL_RUN':
      return 'info'
    default:
      return 'mute'
  }
}

/** 结果状态 → 中文短名。空串是「历史字段未记录」，不能显示成成功或失败。 */
export function auditStatusLabel(status?: string): string {
  switch (status) {
    case 'success':
      return '成功'
    case 'error':
      return '失败'
    case 'pending':
      return '待处理'
    case 'denied':
      return '已拒绝'
    case 'unknown':
      return '未知'
    case 'not_executed':
      return '未执行'
    default:
      return status ? status : '未记录'
  }
}

export function auditStatusTone(status?: string): AuditTone {
  switch (status) {
    case 'success':
      return 'success'
    case 'error':
    case 'denied':
      return 'danger'
    case 'pending':
      return 'warning'
    default:
      return 'mute'
  }
}

/** 决策值（approved/rejected/no/...）→ 中文。未知值原样显示。 */
export function auditDecisionLabel(decision?: string): string {
  switch ((decision ?? '').toLowerCase()) {
    case 'approved':
    case 'approve':
    case 'yes':
      return '已批准'
    case 'rejected':
    case 'reject':
    case 'no':
    case 'denied':
      return '已拒绝'
    case 'skipped':
      return '已跳过'
    default:
      return decision ?? ''
  }
}

/** 风险等级 → 中文。 */
export function auditRiskLabel(risk?: string): string {
  switch ((risk ?? '').toLowerCase()) {
    case 'high':
      return '高风险'
    case 'medium':
      return '中风险'
    case 'low':
      return '低风险'
    case 'safe':
      return '安全'
    default:
      return risk ?? ''
  }
}

/** 列表查询：过滤条件与游标都原样交给后端，前端不做任何权限或归属推断。 */
export async function listAuditEvents(opts: AuditListOptions = {}): Promise<AuditListResult> {
  const env = await apiGet<AuditListResult>('/audit/events', undefined, {
    from: opts.from,
    to: opts.to,
    eventType: opts.eventType,
    userId: opts.userId,
    clusterId: opts.clusterId,
    sessionId: opts.sessionId,
    status: opts.status,
    action: opts.action,
    limit: opts.limit,
    cursor: opts.cursor,
  })
  return env.data ?? { items: [], hasMore: false }
}

/** 详情：单条事件 + 关联时间线。越权与不存在同形 404，页面只负责诚实提示。 */
export async function getAuditEvent(id: number | string): Promise<AuditDetailResult> {
  const env = await apiGet<AuditDetailResult>(`/audit/events/${encodeURIComponent(String(id))}`)
  if (!env.data) {
    throw new Error('审计详情为空')
  }
  return env.data
}

/** 把查询失败翻译成可展示的提示：审计读取与 Chat 运行互相独立。 */
export function describeAuditError(action: string, error: unknown): string {
  const message = error instanceof Error ? error.message : String(error)
  if (message.includes('not configured')) {
    return `${action}失败：审计中心未启用（平台未配置数据库）`
  }
  if (message.includes('permission denied') || message.includes('forbidden')) {
    return `${action}失败：需要 audit.read 权限`
  }
  return `${action}失败：${message}`
}
