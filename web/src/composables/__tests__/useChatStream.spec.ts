import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { defineComponent, nextTick } from 'vue'

import { useChatStream } from '@/composables/useChatStream'
import type { ChatSnapshot } from '@/api/chat'

describe('Chat connection watchdog', () => {
  it('heartbeats keep the connection alive beyond 45 seconds', async () => {
    vi.useFakeTimers()
    let push!: ReadableStreamDefaultController<Uint8Array>
    const fetchMock = vi.fn(async (_url: string, init: RequestInit = {}) => {
      return new Response(new ReadableStream<Uint8Array>({
        start(controller) {
          push = controller
          init.signal?.addEventListener('abort', () => controller.close(), { once: true })
          controller.enqueue(new TextEncoder().encode(': keep-alive\n\n'))
        },
      }))
    })
    vi.stubGlobal('fetch', fetchMock)
    const { stream, unmount } = mountStream()
    try {
      void stream.connect('s1')
      await vi.advanceTimersByTimeAsync(1)
      for (let i = 0; i < 5; i += 1) {
        await vi.advanceTimersByTimeAsync(15_000)
        push.enqueue(new TextEncoder().encode(': keep-alive\n\n'))
        await vi.advanceTimersByTimeAsync(1)
      }
      expect(fetchMock).toHaveBeenCalledTimes(1)
      expect(stream.status.value).toBe('open')
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })

  it('a silent connection opens a replacement after the watchdog expires', async () => {
    vi.useFakeTimers()
    const fetchMock = vi.fn(async (_url: string, init: RequestInit = {}) => {
      return new Response(new ReadableStream<Uint8Array>({
        start(controller) {
          init.signal?.addEventListener('abort', () => controller.close(), { once: true })
        },
      }))
    })
    vi.stubGlobal('fetch', fetchMock)
    const { stream, unmount } = mountStream()
    try {
      void stream.connect('s1')
      await vi.advanceTimersByTimeAsync(45_001)
      expect(fetchMock).toHaveBeenCalledTimes(2)
      expect(stream.status.value).toBe('open')
    } finally {
      unmount()
      vi.useRealTimers()
    }
  })
})

/** 在真实组件实例内使用组合式函数（对齐运行时生命周期，避免裸调用告警）。 */
function mountStream() {
  let stream!: ReturnType<typeof useChatStream>
  const wrapper = mount(
    defineComponent({
      setup() {
        stream = useChatStream()
        return () => null
      },
    }),
  )
  return { stream, unmount: () => wrapper.unmount() }
}

function sseResponse(frames: unknown[], closeAfter = true): Response {
  const body = frames.map((frame) => `data: ${JSON.stringify(frame)}\n\n`).join('')
  if (closeAfter) {
    return new Response(body, { status: 200 })
  }
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(body))
      // 保持连接打开：模拟运行中的会话流。
    },
  })
  return new Response(stream, { status: 200 })
}

function snapshot(cursor = 0): ChatSnapshot {
  return {
    protocol_version: 2,
    session_id: 's1',
    cursor,
    replay_floor: 0,
    turns: [
      {
        turn_id: 't1',
        session_id: 's1',
        input: '诊断',
        status: 'running',
        usage: { prompt_tokens: 1, completion_tokens: 0, total_tokens: 1 },
        revision: cursor,
      },
    ],
    items: [],
  }
}

function deltaEvent(seq: number, delta: string, index = 0): unknown {
  return {
    protocol_version: 2,
    session_id: 's1',
    turn_id: 't1',
    event_type: 'message.delta',
    seq,
    timestamp: new Date().toISOString(),
    data: { message_id: 'm1', part_id: 'p1', index, delta },
  }
}

describe('useChatStream（协议 v2 生命周期）', () => {
  it('安装规范快照并按 seq 归并事件', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse(
          [
            { event_type: 'stream.snapshot', data: snapshot(1) },
            {
              protocol_version: 2,
              session_id: 's1',
              turn_id: 't1',
              event_type: 'message.started',
              seq: 2,
              timestamp: new Date().toISOString(),
              data: {
                message: {
                  message_id: 'm1',
                  turn_id: 't1',
                  invocation_id: 'i1',
                  role: 'assistant',
                  purpose: 'pending',
                  status: 'streaming',
                  parts: [{ part_id: 'p1', type: 'text', last_index: 0 }],
                  item_order: 1,
                  revision: 2,
                },
              },
            },
            deltaEvent(3, '你好'),
          ],
          false,
        ),
      ),
    )
    const { stream, unmount } = mountStream()
    void stream.connect('s1')
    await vi.waitFor(() => expect(stream.items.value).toHaveLength(1))

    expect(stream.status.value).toBe('open')
    expect(stream.turns.value[0].turn_id).toBe('t1')
    expect(stream.token.value.total_tokens).toBe(1)
    const item = stream.items.value[0]
    expect(item.type).toBe('message')
    if (item.type === 'message') {
      expect(item.message.parts[0].text).toBe('你好')
      expect(item.message.parts[0].last_index).toBe(1)
    }
    unmount()
  })

  it('重复 seq 忽略，不重复追加分片', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        sseResponse(
          [
            { event_type: 'stream.snapshot', data: { ...snapshot(1), items: [] } },
            {
              protocol_version: 2,
              session_id: 's1',
              turn_id: 't1',
              event_type: 'message.started',
              seq: 2,
              timestamp: new Date().toISOString(),
              data: {
                message: {
                  message_id: 'm1',
                  turn_id: 't1',
                  invocation_id: 'i1',
                  role: 'assistant',
                  purpose: 'pending',
                  status: 'streaming',
                  parts: [{ part_id: 'p1', type: 'text', last_index: 0 }],
                  item_order: 1,
                  revision: 2,
                },
              },
            },
            deltaEvent(3, '甲'),
            deltaEvent(3, '甲'),
          ],
          false,
        ),
      ),
    )
    const { stream, unmount } = mountStream()
    void stream.connect('s1')
    await vi.waitFor(() => expect(stream.items.value).toHaveLength(1))
    await nextTick()
    const item = stream.items.value[0]
    if (item.type === 'message') {
      expect(item.message.parts[0].text).toBe('甲')
    }
    unmount()
  })

  it('seq 缺口语义事件触发重新同步而不是追加到错误对象', async () => {
    let calls = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        calls += 1
        if (calls === 1) {
          return sseResponse(
            [{ event_type: 'stream.snapshot', data: snapshot(1) }, deltaEvent(9, '缺口的增量')],
            false,
          )
        }
        return sseResponse(
          [
            { event_type: 'stream.snapshot', data: snapshot(1) },
            {
              protocol_version: 2,
              session_id: 's1',
              turn_id: 't1',
              event_type: 'message.started',
              seq: 2,
              timestamp: new Date().toISOString(),
              data: {
                message: {
                  message_id: 'm1',
                  turn_id: 't1',
                  invocation_id: 'i1',
                  role: 'assistant',
                  purpose: 'pending',
                  status: 'streaming',
                  parts: [{ part_id: 'p1', type: 'text', last_index: 0 }],
                  item_order: 1,
                  revision: 2,
                },
              },
            },
            deltaEvent(3, '补齐的增量'),
          ],
          false,
        )
      }),
    )
    const { stream, unmount } = mountStream()
    void stream.connect('s1')
    await vi.waitFor(() => expect(calls).toBeGreaterThan(1))
    await vi.waitFor(() => expect(stream.items.value).toHaveLength(1))
    const item = stream.items.value[0]
    if (item.type === 'message') {
      expect(item.message.parts[0].text).toBe('补齐的增量')
    }
    unmount()
  })

  it('切换会话时中止旧连接并清空状态（不串会话）', async () => {
    const signals: AbortSignal[] = []
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init: RequestInit = {}) => {
        if (init.signal) {
          signals.push(init.signal)
        }
        return sseResponse([{ event_type: 'stream.snapshot', data: snapshot(0) }], false)
      }),
    )
    const { stream, unmount } = mountStream()
    void stream.connect('s1')
    await vi.waitFor(() => expect(signals.length).toBe(1))
    void stream.connect('s2')
    await nextTick()

    expect(signals[0].aborted).toBe(true)
    expect(stream.sessionId.value).toBe('s2')
    unmount()
  })

  it('429/500 进入重连退避，401 停止重试', async () => {
    vi.useFakeTimers()
    let calls = 0
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        calls += 1
        return new Response('', { status: 500 })
      }),
    )
    const { stream, unmount } = mountStream()
    void stream.connect('s1')
    await vi.waitFor(() => expect(calls).toBeGreaterThan(0))
    await vi.advanceTimersByTimeAsync(1500)

    expect(stream.status.value).toBe('reconnecting')
    expect(stream.reconnectAttempt.value).toBeGreaterThanOrEqual(1)
    stream.stop()

    calls = 0
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 401 })))
    void stream.connect('s1')
    await vi.waitFor(() => expect(stream.status.value).toBe('closed'))
    expect(stream.error.value).toContain('登录')
    stream.stop()
    vi.useRealTimers()
    unmount()
  })
})
