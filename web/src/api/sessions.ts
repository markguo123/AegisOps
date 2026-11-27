/**
 * 平台会话端点客户端。
 * 这些端点由平台 server 直接提供（`GET|POST /api/v1/sessions`、
 * `POST /api/v1/sessions/{id}/rename`、`DELETE /api/v1/sessions/{id}`），
 * 所有请求携带当前登录 Token，并由服务端执行 owner 校验。
 */

import type { AgentToken } from './sse'
import { addCsrfHeader } from './request'

/** 列表项，与服务端 handleListSessions 返回字段一一对应。 */
export interface SessionSummary {
  clusterId?:string
  ownerId?:string
  /** 管理员跨用户列表时的 owner 展示名；缺失时用 ownerId 兜底。 */
  ownerName?:string
  id: string
  name: string
  state: string
  /** 会话使用的模型名（composer 只读展示）。 */
  model?: string
  messageCount: number
  lastModified: string
  /** P7.5 置顶状态（置顶会话排最前）。 */
  pinned?: boolean
  /** P2.13 会话级 Token 用量合计；未持久化时缺省。 */
  token?: AgentToken | null
}

/** 会话内一条消息，对齐 `internal/session/session.go` 的 Message。 */
export interface ChatMessage {
  contractVersion?: string
  resultStatus?: string
  role: string
  type: string
  name?: string
  toolCallId?: string
  arguments?: unknown
  data?: unknown
  ok?: boolean
  /** P7.4（ADR-034）：工具结果同源契约，刷新后与 chat 共用同一份头部信息。 */
  summary?: string
  displayType?: string
  durationMs?: number
  risk?: string
  command?: string
  stdout?: string
  stderr?: string
  exitCode?: number
  errorCode?: string
  content: string
  timestamp: string
}

/** 完整 Session，对齐 `internal/session/session.go` 的 Session。 */
export interface SessionDetail {
  id: string
  name?: string
  cluster?: string
  namespace?: string
  model?: string
  state?: string
  messages?: ChatMessage[]
  approvedWrites?: string[]
  declinedWrites?: Record<string, number>
  createdAt?: string
  lastModified?: string
}

async function sessionFetch<T>(url: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  headers.set('Accept', 'application/json')
  if (init.body !== undefined) {
    headers.set('Content-Type', 'application/json')
  }
  // 会话端点在四期平台进程下同样受鉴权保护：带 Bearer 令牌，
  // 三期 webui（无鉴权）会忽略该头，行为不变。
  if (init.method && init.method !== 'GET') addCsrfHeader(headers)
  const response = await fetch(url, { ...init, headers })
  const text = await response.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text) as unknown
    } catch {
      data = null
    }
  }
  if (!response.ok) {
    const message =
      (data &&
        typeof data === 'object' &&
        'error' in data &&
        typeof (data as { error: unknown }).error === 'string'
        ? (data as { error: string }).error
        : undefined) ?? `request failed with status ${response.status}`
    throw new Error(message)
  }
  return data as T
}

export async function listSessions(): Promise<SessionSummary[]> {
  return sessionFetch<SessionSummary[]>('/api/v1/sessions')
}

/** 创建新会话；若传 id 则等同“恢复/获取”该会话。 */
export async function createSession(payload: { id?: string; name?: string; clusterId?:string } = {}): Promise<SessionDetail> {
  return sessionFetch<SessionDetail>('/api/v1/sessions', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function renameSession(id: string, name: string): Promise<{ id: string; name: string }> {
  return sessionFetch<{ id: string; name: string }>(
    `/api/v1/sessions/${encodeURIComponent(id)}/rename`,
    { method: 'POST', body: JSON.stringify({ name }) },
  )
}

/** 置顶/取消置顶会话（P7.5）。 */
export async function pinSession(
  id: string,
  pinned: boolean,
): Promise<{ id: string; pinned: boolean }> {
  return sessionFetch<{ id: string; pinned: boolean }>(
    `/api/v1/sessions/${encodeURIComponent(id)}/pin`,
    { method: 'POST', body: JSON.stringify({ pinned }) },
  )
}

/** 发起一轮 Chat（`POST /api/v1/sessions/{id}/send-message`），SSE 流随后推送事件。
 * Web 端不暴露 skipPermissions；审批跳过只能由部署侧自动化能力配置。
 * `idempotencyKey` 用于网络重试时复用同一个 turn，不创建第二轮。 */
export function listChatModels() {
  return sessionFetch<Array<{ id: string; name?: string; context_window: number }>>('/api/v1/sessions/models')
}

export interface BranchOrigin {
  sessionId: string
  turnId: string
}

export function getBranchOrigin(id: string) {
  return sessionFetch<BranchOrigin | null>(`/api/v1/sessions/${encodeURIComponent(id)}/branch`)
}

export function branchSession(id: string, turnId: string, requestKey: string) {
  return sessionFetch<{ sessionId: string }>(`/api/v1/sessions/${encodeURIComponent(id)}/branch`, {
    method: 'POST',
    headers: { 'Idempotency-Key': requestKey },
    body: JSON.stringify({ turnId }),
  })
}

export async function sendMessage(
  id: string,
  payload: { query: string; skills?: string[]; idempotencyKey?: string; modelId?: string },
): Promise<{ sessionId: string; turnId?: string; protocolVersion?: number; duplicate?: boolean }> {
  const body: Record<string, unknown> = { query: payload.query }
  if (payload.modelId) body.modelId = payload.modelId
  // P3.8.4：显式 @skill 指定的名称只在非空时随请求提交，后端强校验。
  if (payload.skills && payload.skills.length > 0) {
    body.skills = payload.skills
  }
  const headers: HeadersInit = {}
  if (payload.idempotencyKey) {
    ;(headers as Record<string, string>)['Idempotency-Key'] = payload.idempotencyKey
  }
  return sessionFetch<{ sessionId: string; turnId?: string; protocolVersion?: number; duplicate?: boolean }>(
    `/api/v1/sessions/${encodeURIComponent(id)}/send-message`,
    { method: 'POST', body: JSON.stringify(body), headers },
  )
}

/** 取消当前会话正在执行的 Chat 运行。 */
export async function cancelRun(id: string, signal?: AbortSignal): Promise<{ sessionId: string }> {
  return sessionFetch<{ sessionId: string }>(`/api/v1/sessions/${encodeURIComponent(id)}/cancel`, {
    method: 'POST',
    signal,
  })
}

/** 回传审批选择（webui `POST /api/v1/sessions/{id}/approve`）。choice 不写回响应体。 */
export async function approveRun(id: string, choice: string, approvalId?:string, expectedRevision?: number): Promise<{ sessionId: string }> {
  return sessionFetch<{ sessionId: string }>(
    `/api/v1/sessions/${encodeURIComponent(id)}/approve`,
    { method: 'POST', body: JSON.stringify({ choice,approvalId, expectedRevision }) },
  )
}

export async function deleteSession(id: string): Promise<{ deleted: string }> {
  return sessionFetch<{ deleted: string }>(`/api/v1/sessions/${encodeURIComponent(id)}`, {
    method: 'DELETE',
  })
}

/** 历史工具结果中的问题记录（只读回放，不再用于轮询）。 */
export interface UserQuestion {
  id: string
  session_id: string
  turn_id: string
  question: string
  options: { id: string; label: string; impact: string; recommended?: boolean }[]
  expires_at: string
}

/** 从规范 Interaction 提交结构化回答（问题卡与审批卡走各自既有端点）。 */
export function answerInteraction(
  sessionId: string,
  payload: { interactionId: string; turnId: string; status: string; optionId?: string; text?: string },
): Promise<unknown> {
  return sessionFetch(`/api/v1/sessions/${encodeURIComponent(sessionId)}/question`, {
    method: 'POST',
    body: JSON.stringify({
      id: payload.interactionId,
      session_id: sessionId,
      turn_id: payload.turnId,
      status: payload.status,
      option_id: payload.optionId,
      text: payload.text,
    }),
  })
}

/** Values are ephemeral and must never be written to the Chat store. */
export function revealSecret(sessionId: string, target: { namespace: string; name: string; keys: string[] }, signal: AbortSignal): Promise<{ values: Record<string, { value: string; encoding: string }> }> {
  return sessionFetch(`/api/v1/sessions/${encodeURIComponent(sessionId)}/secret/reveal`, {
    method: 'POST', body: JSON.stringify(target), signal, cache: 'no-store',
  })
}
