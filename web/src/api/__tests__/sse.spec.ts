import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  ChatStreamError,
  fetchChatStream,
  normalizeChatFrame,
  type ChatFrame,
  type ChatSnapshot,
} from '@/api/chat'
import { TOKEN_KEY } from '@/api/request'

function sseResponse(frames: unknown[]): Response {
  const body = frames.map((frame) => `data: ${JSON.stringify(frame)}\n\n`).join('')
  return new Response(body, {
    status: 200,
    headers: { 'Content-Type': 'text/event-stream' },
  })
}

const snapshot: ChatSnapshot = {
  protocol_version: 2,
  session_id: 's1',
  cursor: 9,
  replay_floor: 0,
  turns: [{ turn_id: 't1', session_id: 's1', status: 'done', usage: { prompt_tokens: 1, completion_tokens: 2, total_tokens: 3 }, revision: 9 }],
  items: [],
}

describe('normalizeChatFrame（协议 v2）', () => {
  it('识别规范快照并保留游标与 Turn 用量', () => {
    const frame = normalizeChatFrame({ event_type: 'stream.snapshot', data: snapshot }) as Extract<
      ChatFrame,
      { kind: 'snapshot' }
    >
    expect(frame.kind).toBe('snapshot')
    expect(frame.snapshot.cursor).toBe(9)
    expect(frame.snapshot.turns[0].usage.total_tokens).toBe(3)
  })

  it('识别续传失效与终止控制帧', () => {
    const reset = normalizeChatFrame({
      event_type: 'stream.reset_required',
      data: { reason: 'cursor_out_of_replay_window' },
    })
    expect(reset).toMatchObject({ kind: 'reset_required', reason: 'cursor_out_of_replay_window' })
    const fatal = normalizeChatFrame({
      event_type: 'stream.fatal',
      data: { code: 'PERSISTENCE_UNAVAILABLE', message: '输出未能可靠保存', retryable: true },
    })
    expect(fatal).toMatchObject({ kind: 'fatal', code: 'PERSISTENCE_UNAVAILABLE', retryable: true })
  })

  it('语义事件按 seq 判定；旧 camelCase 帧不再进入展示流', () => {
    const unified = normalizeChatFrame({
      protocol_version: 2,
      session_id: 's1',
      event_type: 'tool.updated',
      seq: 3,
      data: { tool: { tool_call_id: 'tc' } },
    })
    expect(unified?.kind).toBe('event')
    expect(normalizeChatFrame({ type: 'progress', text: '旧事件' })?.kind).toBe('unknown')
    expect(normalizeChatFrame('nope')).toBeNull()
  })
})

describe('fetchChatStream（SSE 传输 v2）', () => {
  beforeEach(() => {
    localStorage.clear()
  })

  it('显式携带 protocol=2 与续传游标，按序回调快照与事件', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok-1')
    const frames: ChatFrame[] = []
    const fetchMock = vi.fn(async () =>
      sseResponse([
        { event_type: 'stream.snapshot', data: snapshot },
        {
          protocol_version: 2,
          session_id: 's1',
          turn_id: 't1',
          event_type: 'message.delta',
          seq: 10,
          timestamp: new Date().toISOString(),
          data: { message_id: 'm1', part_id: 'p1', index: 0, delta: '你好' },
        },
      ]),
    )
    vi.stubGlobal('fetch', fetchMock)

    await fetchChatStream('s1', {
      after: 4,
      signal: new AbortController().signal,
      onFrame: (frame) => frames.push(frame),
    })

    expect(frames.map((frame) => frame.kind)).toEqual(['snapshot', 'event'])
    const [url] = fetchMock.mock.calls[0] as unknown as [string]
    expect(url).toContain('/api/v1/sessions/s1/stream?')
    expect(url).toContain('protocol=2')
    expect(url).toContain('after=4')
  })

  it('非 2xx 抛 ChatStreamError 并保留状态码（426 供升级提示）', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 426 })))
    await expect(
      fetchChatStream('s1', { signal: new AbortController().signal, onFrame: () => {} }),
    ).rejects.toMatchObject({ status: 426 })
    await expect(
      fetchChatStream('s1', { signal: new AbortController().signal, onFrame: () => {} }),
    ).rejects.toBeInstanceOf(ChatStreamError)
  })
})
