import type { ChatTokenUsage } from '@/api/chat'
import type { ToolUiData } from './ToolCallCard.vue'
import type { ApprovalUiData } from './ApprovalCard.vue'

/**
 * 回合（Turn）展示模型：由 Chat reducer 的规范对象派生，供 TurnBlock 渲染。
 * 历史与实时共用同一份实体，不再有 hist:/live 两套 ID。
 */

export interface ReasoningUiData {
  key: string
  text: string
  streaming: boolean
  done: boolean
}

export interface ReportUiData {
  key: string
  text: string
  streaming: boolean
  done: boolean
  /** final = 最终回答；commentary = 过程说明（不冒充结论）。 */
  purpose?: 'final' | 'commentary'
}

export interface QuestionUiData {
  key: string
  interactionId: string
  sessionId: string
  turnId: string
  prompt: string
  options: Array<{ id: string; label: string; impact?: string; recommended?: boolean }>
  status: string
  expiresAt?: string
  answer?: string
}

export interface NoticeUiData {
  key: string
  severity: 'info' | 'warning' | 'error'
  code: string
  message: string
}

export type TurnItem =
  | { type: 'reasoning'; data: ReasoningUiData }
  | { type: 'tool'; data: ToolUiData }
  | { type: 'approval'; data: ApprovalUiData }
  | { type: 'question'; data: QuestionUiData }
  | { type: 'notice'; data: NoticeUiData }
  | { type: 'report'; data: ReportUiData }

export type TurnStatus =
  | 'pending'
  | 'running'
  | 'waiting'
  | 'completed'
  | 'failed'
  | 'canceled'

export interface TurnUiData {
  sessionId?: string
  key: string
  seq: number
  input: string
  status: TurnStatus
  /** 等待原因与真实 activity（只由事实产生）。 */
  waitReason?: string
  activity?: string
  error?: string
  token?: ChatTokenUsage | null
  startedAt?: string
  completedAt?: string
  items: TurnItem[]
}

/** 单回合耗时（ms）。 */
export function turnDuration(turn: TurnUiData): number | null {
  const start = turn.startedAt ? new Date(turn.startedAt).getTime() : NaN
  const end = turn.completedAt ? new Date(turn.completedAt).getTime() : NaN
  if (Number.isNaN(start)) {
    return null
  }
  if (Number.isNaN(end)) {
    return Date.now() - start
  }
  return Math.max(0, end - start)
}

export function formatDuration(ms: number | null): string {
  if (ms === null || ms === undefined) {
    return ''
  }
  if (ms < 1000) {
    return `${Math.round(ms)}ms`
  }
  return `${(ms / 1000).toFixed(1)}s`
}
