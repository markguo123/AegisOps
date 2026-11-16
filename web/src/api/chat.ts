/**
 * Chat 展示协议 v2 客户端（方案 25 / ADR-055）。
 *
 * 单一契约：规范展示对象（Turn/Message/Part/ToolCall/Interaction/Notice）、
 * 语义事件与连接控制帧。传输层只负责 SSE 帧解析与游标，归并全部由纯 reducer
 * 完成（历史快照与实时事件共用同一份状态）。
 */

export const CHAT_PROTOCOL_VERSION = 2

export interface ChatTokenUsage {
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
}

export interface ChatPart {
  part_id: string
  type: 'text' | 'reasoning'
  text?: string
  /** part 内已提交的连续增量下标（0 起）。 */
  last_index: number
}

export type ChatPurpose = 'pending' | 'commentary' | 'final'

export interface ChatMessage {
  message_id: string
  turn_id: string
  invocation_id: string
  role: string
  purpose: ChatPurpose
  status: 'streaming' | 'completed' | 'failed' | 'interrupted'
  parts: ChatPart[]
  item_order: number
  revision: number
  error?: string
}

export type ChatToolStatus =
  | 'started'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'denied'
  | 'unknown'

export interface ChatToolCall {
  tool_call_id: string
  turn_id: string
  step_id?: string
  message_id?: string
  name: string
  params?: string
  status: ChatToolStatus
  ok?: boolean
  result?: string
  summary?: string
  display_type?: string
  duration_ms?: number
  risk?: string
  command?: string
  stdout?: string
  stderr?: string
  exit_code?: number
  error_code?: string
  error?: string
  data?: unknown
  item_order: number
  revision: number
  started_at?: string
  finished_at?: string
}

export type ChatInteractionKind = 'question' | 'approval' | 'secret_read'

export type ChatInteractionStatus =
  | 'pending'
  | 'answered'
  | 'approved'
  | 'rejected'
  | 'skipped'
  | 'timed_out'
  | 'canceled'
  | 'invalidated'

export interface ChatChoiceOption {
  label: string
  value: string
  impact?: string
  recommended?: boolean
}

export interface ChatInteraction {
  interaction_id: string
  turn_id: string
  tool_call_id?: string
  kind: ChatInteractionKind
  status: ChatInteractionStatus
  revision: number
  expires_at?: string
  title?: string
  prompt?: string
  tool?: string
  operation?: string
  risk?: string
  summary?: string
  command?: string
  options?: ChatChoiceOption[]
  target?: {
    cluster?: string
    namespace?: string
    group?: string
    version?: string
    kind?: string
    name?: string
  }
  changes?: Array<{
    path: string
    operation?: string
    before?: unknown
    after?: unknown
    description?: string
  }>
  checks?: Array<{ name: string; status: string; message?: string }>
  verification?: string[]
  secret?: boolean
  answer?: string
  item_order: number
  created_at?: string
}

export interface ChatNotice {
  notice_id: string
  turn_id?: string
  severity: 'info' | 'warning' | 'error'
  code: string
  message: string
  related_id?: string
  item_order: number
  revision: number
  created_at?: string
}

export type ChatItem =
  | { type: 'message'; item_order: number; message: ChatMessage }
  | { type: 'tool'; item_order: number; tool: ChatToolCall }
  | { type: 'interaction'; item_order: number; interaction: ChatInteraction }
  | { type: 'notice'; item_order: number; notice: ChatNotice }

export interface ChatTurn {
  turn_id: string
  session_id: string
  input?: string
  status: 'pending' | 'running' | 'waiting' | 'done' | 'failed' | 'canceled'
  wait_reason?: 'question' | 'approval' | 'secret'
  activity?: 'checking' | 'generating' | 'executing' | 'verifying'
  error?: string
  usage: ChatTokenUsage
  started_at?: string
  finished_at?: string
  revision: number
}

export interface ChatSnapshot {
  protocol_version: number
  session_id: string
  cursor: number
  replay_floor: number
  turns: ChatTurn[]
  items: ChatItem[]
}

export type ChatEventType =
  | 'turn.status'
  | 'message.started'
  | 'message.delta'
  | 'message.completed'
  | 'message.failed'
  | 'tool.updated'
  | 'interaction.updated'
  | 'notice.created'
  | 'usage.updated'

export interface ChatEvent {
  protocol_version: number
  session_id: string
  turn_id?: string
  seq: number
  event_type: ChatEventType
  timestamp: string
  data: unknown
}

export interface ChatDeltaData {
  message_id: string
  part_id: string
  revision: number
  index: number
  delta: string
}

export type ChatFrame =
  | { kind: 'snapshot'; snapshot: ChatSnapshot }
  | { kind: 'event'; event: ChatEvent }
  | { kind: 'reset_required'; reason: string }
  | { kind: 'fatal'; code: string; message: string; retryable: boolean }
  | { kind: 'unknown'; payload: Record<string, unknown> }

/** 单条 SSE data JSON → 归一帧；无法识别时返回 unknown（不静默丢弃）。 */
export function normalizeChatFrame(payload: unknown): ChatFrame | null {
  if (!payload || typeof payload !== 'object') {
    return null
  }
  const record = payload as Record<string, unknown>
  const type = record.event_type
  if (type === 'stream.snapshot') {
    return { kind: 'snapshot', snapshot: record.data as ChatSnapshot }
  }
  if (type === 'stream.reset_required') {
    const data = (record.data ?? {}) as { reason?: string }
    return { kind: 'reset_required', reason: data.reason ?? 'cursor_invalid' }
  }
  if (type === 'stream.fatal') {
    const data = (record.data ?? {}) as { code?: string; message?: string; retryable?: boolean }
    return {
      kind: 'fatal',
      code: data.code ?? 'FATAL',
      message: data.message ?? '输出无法继续',
      retryable: data.retryable ?? false,
    }
  }
  if (typeof type === 'string' && typeof record.seq === 'number') {
    return { kind: 'event', event: record as unknown as ChatEvent }
  }
  return { kind: 'unknown', payload: record }
}

export interface ChatStreamOptions {

  onActivity?: () => void
  /** 续传游标；为空表示首屏（服务端发规范快照）。 */
  after?: number
  signal: AbortSignal
  onOpen?: () => void
  onFrame: (frame: ChatFrame) => void
}

function parseSseChunk(chunk: string): { data: string | null; id: string | null } {
  const data: string[] = []
  let id: string | null = null
  for (const line of chunk.split(/\r?\n/)) {
    if (line.startsWith('data:')) {
      data.push(line.replace(/^data:\s?/, ''))
    } else if (line.startsWith('id:')) {
      id = line.replace(/^id:\s?/, '').trim()
    }
  }
  return { data: data.length ? data.join('\n') : null, id }
}

async function* readSseFrames(response: Response, onActivity?: () => void): AsyncGenerator<{ data: string; id: string | null }> {
  const reader = response.body?.getReader()
  if (!reader) {
    return
  }
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) {
      break
    }
    onActivity?.()
    buffer += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n')
    let boundary = buffer.indexOf('\n\n')
    while (boundary >= 0) {
      const raw = buffer.slice(0, boundary)
      buffer = buffer.slice(boundary + 2)
      const parsed = parseSseChunk(raw)
      // 注释心跳（`: keep-alive`）没有 data，不推进游标。
      if (parsed.data !== null) {
        yield { data: parsed.data, id: parsed.id }
      }
      boundary = buffer.indexOf('\n\n')
    }
  }
  const tail = decoder.decode().replace(/\r\n/g, '\n')
  if (tail) {
    const parsed = parseSseChunk(tail)
    if (parsed.data !== null) {
      yield { data: parsed.data, id: parsed.id }
    }
  }
}

export class ChatStreamError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
    this.name = 'ChatStreamError'
  }
}

/** 打开会话 SSE（协议 v2）。流正常结束/取消时返回；HTTP 错误抛 ChatStreamError。 */
export async function fetchChatStream(
  sessionId: string,
  options: ChatStreamOptions,
): Promise<void> {
  const params = new URLSearchParams({ protocol: String(CHAT_PROTOCOL_VERSION) })
  if (options.after && options.after > 0) {
    params.set('after', String(options.after))
  }
  const response = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/stream?${params.toString()}`,
    { signal: options.signal, headers: { Accept: 'text/event-stream' } },
  )
  if (!response.ok) {
    throw new ChatStreamError(`stream request failed with status ${response.status}`, response.status)
  }
  options.onOpen?.()
  for await (const frame of readSseFrames(response, options.onActivity)) {
    if (options.signal.aborted) {
      return
    }
    let payload: unknown
    try {
      payload = JSON.parse(frame.data) as unknown
    } catch {
      continue
    }
    const normalized = normalizeChatFrame(payload)
    if (normalized) {
      options.onFrame(normalized)
    }
  }
}

/** 读取规范时间线（历史首屏）。 */
export async function fetchTimeline(
  sessionId: string,
  turnLimit = 30,
): Promise<ChatSnapshot> {
  const response = await fetch(
    `/api/v1/sessions/${encodeURIComponent(sessionId)}/timeline?turn_limit=${turnLimit}`,
    { headers: { Accept: 'application/json' } },
  )
  if (!response.ok) {
    throw new ChatStreamError(`timeline request failed with status ${response.status}`, response.status)
  }
  return (await response.json()) as ChatSnapshot
}
