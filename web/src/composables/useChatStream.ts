/**
 * Chat SSE v2 传输与生命周期（方案 25 §9/§10）。
 *
 * - 切换会话先中止旧连接并原子安装新会话快照；重连不清空已展示内容；
 * - 显式携带游标续传，重复 seq 忽略，缺口触发重新同步；
 * - 45s 无帧进入重连，指数退避；401/403/404/426 停止无效重试；
 * - 展示状态只由 reducer 归并，组件不解析事件。
 */

import { computed, onUnmounted, ref } from 'vue'

import type { ChatFrame } from '@/api/chat'
import { ChatStreamError, fetchChatStream } from '@/api/chat'
import {
  activeTurn,
  applyChatEvent,
  applyFatal,
  applySnapshot,
  emptyChatState,
  pendingInteraction,
  totalTokenUsage,
  type ChatState,
} from '@/chat/store'

export type StreamStatus = 'idle' | 'connecting' | 'open' | 'reconnecting' | 'closed' | 'fatal'

export const CHAT_IDLE_TIMEOUT_MS = 45_000

function backoff(attempt: number): number {
  const exp = Math.min(attempt - 1, 4)
  return Math.min(8_000, 1_000 * 2 ** exp) + Math.round(Math.random() * 400)
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms))
}

export function useChatStream() {
  const status = ref<StreamStatus>('idle')
  const sessionId = ref<string | null>(null)
  const error = ref('')
  const reconnectAttempt = ref(0)
  const lastFrameAt = ref('')
  const state = ref<ChatState>(emptyChatState())

  const turns = computed(() =>
    state.value.turnOrder
      .map((id) => state.value.turns[id])
      .filter((turn): turn is NonNullable<typeof turn> => Boolean(turn)),
  )
  const items = computed(() => state.value.items)
  const active = computed(() => activeTurn(state.value))
  const pending = computed(() => pendingInteraction(state.value))
  const token = computed(() => totalTokenUsage(state.value))
  const running = computed(() => active.value !== null)

  let controller: AbortController | null = null
  let generation = 0
  let runningLoop = false
  let disposed = false
  let idleTimer: number | null = null
  // 缺口触发的重新订阅（同一时刻只允许一个挂起请求）。
  let resyncPending = false

  function reset(): void {
    status.value = 'idle'
    error.value = ''
    reconnectAttempt.value = 0
    lastFrameAt.value = ''
    state.value = emptyChatState()
  }

  function armIdleTimer(id: string, gen: number): void {
    if (idleTimer !== null) {
      window.clearTimeout(idleTimer)
    }
    idleTimer = window.setTimeout(() => {
      if (sessionId.value !== id || generation !== gen) {
        return
      }
      // 长时间没有任何帧：主动重连，服务端会按游标补齐。
      void reconnect()
    }, CHAT_IDLE_TIMEOUT_MS)
  }

  function clearIdleTimer(): void {
    if (idleTimer !== null) {
      window.clearTimeout(idleTimer)
      idleTimer = null
    }
  }

  function handleFrame(frame: ChatFrame, id: string, gen: number): void {
    if (sessionId.value !== id || generation !== gen) {
      return
    }
    lastFrameAt.value = new Date().toISOString()
    switch (frame.kind) {
      case 'snapshot':
        state.value = applySnapshot(frame.snapshot)
        return
      case 'reset_required':
        // 续传范围失效：服务端随后会发规范快照；这里清空游标等待安装。
        state.value = { ...state.value, cursor: 0, needSync: false }
        return
      case 'fatal':
        state.value = applyFatal(state.value, frame.code, frame.message)
        status.value = 'fatal'
        error.value = frame.message
        return
      case 'event': {
        const result = applyChatEvent(state.value, frame.event)
        state.value = result.state
        if (result.needSync) {
          // 缺口：中止当前连接并从最后已应用游标重新订阅（异步，避免在帧回调里重入）。
          if (!resyncPending) {
            resyncPending = true
            queueMicrotask(() => {
              void resync().finally(() => {
                resyncPending = false
              })
            })
          }
        }
        return
      }
      default:
        return
    }
  }

  async function runLoop(id: string, gen: number): Promise<void> {
    let attempt = 0
    while (runningLoop && sessionId.value === id && generation === gen) {
      if (state.value.fatal) {
        return
      }
      controller = new AbortController()
      const signal = controller.signal
      armIdleTimer(id, gen)
      try {
        await fetchChatStream(id, {
          after: state.value.cursor,
          signal,
          onOpen: () => {
            status.value = 'open'
            error.value = ''
          },
          onFrame: (frame) => handleFrame(frame, id, gen),
          onActivity: () => {
            if (generation === gen && sessionId.value === id) armIdleTimer(id, gen)
          },
        })
        if (sessionId.value !== id || generation !== gen || signal.aborted) {
          return
        }
        // 服务端正常关闭连接（进程重启/网络）：带游标重连。
        attempt += 1
        reconnectAttempt.value = attempt
        status.value = 'reconnecting'
        error.value = '连接中断，正在自动重连；已展示内容会保留'
        await delay(backoff(attempt))
      } catch (err) {
        if (disposed || sessionId.value !== id || generation !== gen) {
          return
        }
        if (signal.aborted) {
          return
        }
        if (err instanceof ChatStreamError) {
          if (err.status === 401) {
            status.value = 'closed'
            error.value = '登录已失效，请重新登录'
            return
          }
          if (err.status === 403 || err.status === 404) {
            status.value = 'closed'
            error.value = '会话不可访问或已删除'
            return
          }
          if (err.status === 426) {
            status.value = 'closed'
            error.value = '客户端版本过旧，请刷新页面以升级到 Chat 协议 v2'
            return
          }
        }
        attempt += 1
        reconnectAttempt.value = attempt
        status.value = 'reconnecting'
        error.value = err instanceof Error ? err.message : '连接失败，正在自动重连'
        await delay(backoff(attempt))
      } finally {
        if (generation === gen) clearIdleTimer()
      }
    }
    if (status.value === 'reconnecting') {
      status.value = 'closed'
    }
  }

  async function connect(id: string): Promise<void> {
    clearIdleTimer()
    runningLoop = false
    resyncPending = false
    controller?.abort()
    controller = null
    generation += 1
    const gen = generation
    sessionId.value = id
    reset()
    status.value = 'connecting'
    runningLoop = true
    await runLoop(id, gen)
  }

  /** 手动重连：保留当前展示内容与游标，重新订阅。 */
  async function reconnect(): Promise<void> {
    const id = sessionId.value
    if (!id) {
      return
    }
    controller?.abort()
    controller = null
    runningLoop = false
    resyncPending = false
    generation += 1
    const gen = generation
    status.value = 'connecting'
    reconnectAttempt.value = 0
    error.value = ''
    runningLoop = true
    await runLoop(id, gen)
  }

  /** 主动重取规范快照（fatal 后由用户触发）。 */
  async function resync(): Promise<void> {
    state.value = { ...state.value, cursor: 0, fatal: null, needSync: false }
    await reconnect()
  }

  function stop(): void {
    runningLoop = false
    controller?.abort()
    controller = null
    clearIdleTimer()
    reset()
    sessionId.value = null
  }

  onUnmounted(() => {
    disposed = true
    stop()
  })

  return {
    status,
    sessionId,
    error,
    reconnectAttempt,
    lastFrameAt,
    state,
    turns,
    items,
    active,
    pending,
    token,
    running,
    connect,
    reconnect,
    resync,
    stop,
  }
}

export default useChatStream
