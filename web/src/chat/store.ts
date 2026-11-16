/**
 * Chat 展示 reducer（方案 25 §10.2）：历史快照与实时事件更新同一份规范对象。
 *
 * 纯函数、无 Vue 依赖：`applySnapshot` 原子安装首屏，`applyChatEvent` 按 seq
 * 严格递增归并；重复 seq 忽略，seq 跳跃/part.index 不连续时要求重新同步，
 * 绝不把内容追加到错误对象。组件不重扫全量事件，只读这里的规范状态。
 */

import type {
  ChatEvent,
  ChatInteraction,
  ChatItem,
  ChatMessage,
  ChatNotice,
  ChatSnapshot,
  ChatTokenUsage,
  ChatToolCall,
  ChatTurn,
} from '@/api/chat'

export interface ChatState {
  protocolVersion: number
  cursor: number
  replayFloor: number
  turns: Record<string, ChatTurn>
  turnOrder: string[]
  items: ChatItem[]
  /** 游标出现缺口，下一次必须重取快照。 */
  needSync: boolean
  fatal: { code: string; message: string } | null
}

export function emptyChatState(): ChatState {
  return {
    protocolVersion: 2,
    cursor: 0,
    replayFloor: 0,
    turns: {},
    turnOrder: [],
    items: [],
    needSync: false,
    fatal: null,
  }
}

const EMPTY_USAGE: ChatTokenUsage = { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 }

/** 原子安装规范快照（历史首屏 / 重同步）。 */
export function applySnapshot(snap: ChatSnapshot): ChatState {
  const turns: Record<string, ChatTurn> = {}
  const order: string[] = []
  for (const turn of snap.turns ?? []) {
    turns[turn.turn_id] = { ...turn, usage: turn.usage ?? { ...EMPTY_USAGE } }
    order.push(turn.turn_id)
  }
  return {
    protocolVersion: snap.protocol_version ?? 2,
    cursor: snap.cursor ?? 0,
    replayFloor: snap.replay_floor ?? 0,
    turns,
    turnOrder: order,
    items: [...(snap.items ?? [])].sort((a, b) => a.item_order - b.item_order),
    needSync: false,
    fatal: null,
  }
}

export interface ApplyResult {
  state: ChatState
  needSync: boolean
}

function touchTurn(
  state: ChatState,
  turnID: string,
  patch: Partial<ChatTurn>,
): ChatState {
  if (!turnID) {
    return state
  }
  const existing = state.turns[turnID]
  const turns = { ...state.turns }
  let order = state.turnOrder
  if (!existing) {
    order = [...state.turnOrder, turnID]
    turns[turnID] = {
      turn_id: turnID,
      session_id: '',
      status: 'running',
      usage: { ...EMPTY_USAGE },
      revision: 0,
      ...patch,
    } as ChatTurn
  } else {
    turns[turnID] = { ...existing, ...patch }
  }
  return { ...state, turns, turnOrder: order }
}

function itemKey(item: ChatItem): string {
  switch (item.type) {
    case 'message':
      return `m:${item.message.message_id}`
    case 'tool':
      return `t:${item.tool.tool_call_id}`
    case 'interaction':
      return `i:${item.interaction.interaction_id}`
    default:
      return `n:${item.notice.notice_id}`
  }
}

function upsertItem(state: ChatState, item: ChatItem): ChatState {
  const key = itemKey(item)
  const items = [...state.items]
  const index = items.findIndex((candidate) => itemKey(candidate) === key)
  if (index >= 0) {
    items[index] = item
  } else {
    items.push(item)
    items.sort((a, b) => a.item_order - b.item_order)
  }
  return { ...state, items }
}

function updateMessage(
  state: ChatState,
  messageID: string,
  update: (message: ChatMessage) => ChatMessage,
): ChatState {
  const items = [...state.items]
  const index = items.findIndex(
    (item) => item.type === 'message' && item.message.message_id === messageID,
  )
  if (index < 0) {
    return state
  }
  const item = items[index]
  if (item.type !== 'message') {
    return state
  }
  items[index] = { type: 'message', item_order: item.item_order, message: update({ ...item.message }) }
  return { ...state, items }
}

function findMessage(state: ChatState, messageID: string): ChatMessage | null {
  for (const item of state.items) {
    if (item.type === 'message' && item.message.message_id === messageID) {
      return item.message
    }
  }
  return null
}

interface DeltaData {
  message_id: string
  part_id: string
  revision: number
  index: number
  delta: string
}

/** 归并一条实时事件；返回新的规范状态与是否需要重新同步。 */
export function applyChatEvent(state: ChatState, event: ChatEvent): ApplyResult {
  if (event.seq <= state.cursor) {
    return { state, needSync: false }
  }
  if (event.seq !== state.cursor + 1) {
    return { state: { ...state, needSync: true }, needSync: true }
  }
  const data = (event.data ?? {}) as Record<string, unknown>
  const turnID = event.turn_id ?? ''
  let next: ChatState = { ...state, cursor: event.seq }
  switch (event.event_type) {
    case 'turn.status': {
      const status = (data.status as ChatTurn['status']) ?? undefined
      next = touchTurn(next, String(data.turn_id ?? turnID), {
        status,
        wait_reason: (data.wait_reason as ChatTurn['wait_reason']) ?? undefined,
        activity: (data.activity as ChatTurn['activity']) ?? undefined,
        input: typeof data.input === 'string' ? data.input : undefined,
        error: typeof data.error === 'string' ? data.error : undefined,
        started_at: typeof data.started_at === 'string' ? data.started_at : undefined,
        finished_at: typeof data.finished_at === 'string' ? data.finished_at : undefined,
        usage: (data.usage as ChatTokenUsage) ?? undefined,
        revision: event.seq,
      })
      break
    }
    case 'message.started':
    case 'message.completed':
    case 'message.failed': {
      const message = data.message as ChatMessage | undefined
      if (!message) {
        break
      }
      const normalized: ChatMessage = { ...message }
      if (normalized.item_order === 0) {
        // 快照里 item_order 由服务端分配；实时事件里的对象仍可能为 0，
        // 用消息在事件流中的位置兜底，保证稳定排序。
        const last = next.items.length ? next.items[next.items.length - 1].item_order : 0
        normalized.item_order = last + 1
      }
      next = upsertItem(next, {
        type: 'message',
        item_order: normalized.item_order,
        message: normalized,
      })
      break
    }
    case 'message.delta': {
      const delta = data as unknown as DeltaData
      const message = findMessage(next, delta.message_id)
      if (!message) {
        return { state: { ...next, needSync: true }, needSync: true }
      }
      const partIndex = message.parts.findIndex((part) => part.part_id === delta.part_id)
      if (partIndex < 0) {
        return { state: { ...next, needSync: true }, needSync: true }
      }
      const part = message.parts[partIndex]
      if (delta.index !== part.last_index) {
        return { state: { ...next, needSync: true }, needSync: true }
      }
      const parts = [...message.parts]
      parts[partIndex] = {
        ...part,
        text: `${part.text ?? ''}${delta.delta}`,
        last_index: part.last_index + 1,
      }
      next = updateMessage(next, delta.message_id, (current) => ({ ...current, parts }))
      break
    }
    case 'tool.updated': {
      const tool = data.tool as ChatToolCall | undefined
      if (tool) {
        next = upsertItem(next, { type: 'tool', item_order: tool.item_order, tool: { ...tool } })
      }
      break
    }
    case 'interaction.updated': {
      const interaction = data.interaction as ChatInteraction | undefined
      if (interaction) {
        next = upsertItem(next, {
          type: 'interaction',
          item_order: interaction.item_order,
          interaction: { ...interaction },
        })
      }
      break
    }
    case 'notice.created': {
      const notice = data.notice as ChatNotice | undefined
      if (notice) {
        next = upsertItem(next, { type: 'notice', item_order: notice.item_order, notice: { ...notice } })
      }
      break
    }
    case 'usage.updated': {
      next = touchTurn(next, String(data.turn_id ?? turnID), {
        usage: (data.usage as ChatTokenUsage) ?? undefined,
      })
      break
    }
    default:
      break
  }
  return { state: next, needSync: false }
}

export function applyFatal(state: ChatState, code: string, message: string): ChatState {
  return { ...state, fatal: { code, message } }
}

export function markNeedSync(state: ChatState): ChatState {
  return { ...state, needSync: true }
}

/** 某个 Turn 的规范条目（按服务端 item_order 稳定排序）。 */
export function itemsOfTurn(state: ChatState, turnID: string): ChatItem[] {
  return state.items.filter((item) => {
    switch (item.type) {
      case 'message':
        return item.message.turn_id === turnID
      case 'tool':
        return item.tool.turn_id === turnID
      case 'interaction':
        return item.interaction.turn_id === turnID
      default:
        return item.notice.turn_id === turnID
    }
  })
}

/** 当前活动 Turn（最后一个非终态回合）。 */
export function activeTurn(state: ChatState): ChatTurn | null {
  for (let i = state.turnOrder.length - 1; i >= 0; i -= 1) {
    const turn = state.turns[state.turnOrder[i]]
    if (turn && (turn.status === 'pending' || turn.status === 'running' || turn.status === 'waiting')) {
      return turn
    }
  }
  return null
}

/** 待处理交互（问题/审批/敏感读取）；同一时刻最多一个。 */
export function pendingInteraction(state: ChatState): ChatInteraction | null {
  for (let i = state.items.length - 1; i >= 0; i -= 1) {
    const item = state.items[i]
    if (item.type === 'interaction' && item.interaction.status === 'pending') {
      return item.interaction
    }
  }
  return null
}

/** 会话级 Token 用量：各 Turn 覆盖语义的累计。 */
export function totalTokenUsage(state: ChatState): ChatTokenUsage {
  let prompt = 0
  let completion = 0
  let total = 0
  for (const id of state.turnOrder) {
    const turn = state.turns[id]
    if (!turn) {
      continue
    }
    prompt += turn.usage?.prompt_tokens ?? 0
    completion += turn.usage?.completion_tokens ?? 0
    total += turn.usage?.total_tokens ?? 0
  }
  return { prompt_tokens: prompt, completion_tokens: completion, total_tokens: total }
}

/** 是否仍有运行中的 Turn（用于输入框停止按钮与“正在生成”状态）。 */
export function hasActiveTurn(state: ChatState): boolean {
  return activeTurn(state) !== null
}

/** 等待状态的可读文案；没有等待时不显示。 */
export function waitLabel(turn: ChatTurn | null): string {
  if (!turn || turn.status !== 'waiting') {
    return ''
  }
  switch (turn.wait_reason) {
    case 'question':
      return '等待你选择'
    case 'approval':
      return '等待你审批'
    case 'secret':
      return '等待敏感输入'
    default:
      return '等待你的确认'
  }
}

/** 运行状态徽标文案（只由真实 activity 产生，不按卡片推测）。 */
export function activityLabel(turn: ChatTurn | null): string {
  if (!turn) {
    return ''
  }
  if (turn.status === 'waiting') {
    return waitLabel(turn)
  }
  if (turn.status === 'done') {
    return '完成'
  }
  if (turn.status === 'failed') {
    return '失败'
  }
  if (turn.status === 'canceled') {
    return '已取消'
  }
  if (turn.status === 'pending') {
    return '已接收'
  }
  switch (turn.activity) {
    case 'checking':
      return '检查中'
    case 'generating':
      return '生成回复'
    case 'executing':
      return '执行中'
    case 'verifying':
      return '验证中'
    default:
      return '处理中'
  }
}
