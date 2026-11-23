import { describe, expect, it } from 'vitest'

import type { ChatEvent, ChatSnapshot } from '@/api/chat'
import {
  activeTurn,
  activityLabel,
  applyChatEvent,
  applySnapshot,
  emptyChatState,
  itemsOfTurn,
  pendingInteraction,
  totalTokenUsage,
  waitLabel,
} from '@/chat/store'

function snapshot(): ChatSnapshot {
  return {
    protocol_version: 2,
    session_id: 's1',
    cursor: 2,
    replay_floor: 0,
    turns: [
      {
        turn_id: 't1',
        session_id: 's1',
        input: '检查 nginx',
        status: 'running',
        activity: 'generating',
        usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 },
        revision: 2,
      },
    ],
    items: [
      {
        type: 'message',
        item_order: 1,
        message: {
          message_id: 'm1',
          turn_id: 't1',
          invocation_id: 'i1',
          role: 'assistant',
          purpose: 'pending',
          status: 'streaming',
          parts: [{ part_id: 'p1', type: 'text', last_index: 1 }],
          item_order: 1,
          revision: 2,
        },
      },
    ],
  }
}

function event(seq: number, type: ChatEvent['event_type'], data: unknown, turnID = 't1'): ChatEvent {
  return {
    protocol_version: 2,
    session_id: 's1',
    turn_id: turnID,
    seq,
    event_type: type,
    timestamp: '2026-11-16T08:00:00Z',
    data,
  }
}

describe('Chat reducer（历史与实时同源）', () => {
  it('快照原子安装：Turn 状态、条目与游标一次性生效', () => {
    const state = applySnapshot(snapshot())
    expect(state.cursor).toBe(2)
    expect(state.turnOrder).toEqual(['t1'])
    expect(itemsOfTurn(state, 't1')).toHaveLength(1)
    expect(totalTokenUsage(state).total_tokens).toBe(15)
  })

  it('重复 seq 忽略；连续 seq 追加到同一 part', () => {
    let state = applySnapshot(snapshot())
    const first = applyChatEvent(
      state,
      event(3, 'message.delta', { message_id: 'm1', part_id: 'p1', index: 1, delta: '已确认' }),
    )
    expect(first.needSync).toBe(false)
    state = first.state
    const duplicate = applyChatEvent(state, event(3, 'message.delta', { message_id: 'm1', part_id: 'p1', index: 1, delta: '已确认' }))
    expect(duplicate.state.cursor).toBe(3)
    const item = itemsOfTurn(duplicate.state, 't1')[0]
    if (item.type === 'message') {
      expect(item.message.parts[0].text).toBe('已确认')
      expect(item.message.parts[0].last_index).toBe(2)
    }
  })

  it('seq 跳跃要求重新同步，不向错误对象追加内容', () => {
    const state = applySnapshot(snapshot())
    const result = applyChatEvent(
      state,
      event(9, 'message.delta', { message_id: 'm1', part_id: 'p1', index: 1, delta: '缺口' }),
    )
    expect(result.needSync).toBe(true)
    expect(result.state.cursor).toBe(2)
  })

  it('part 下标不连续时同样要求重新同步', () => {
    const state = applySnapshot(snapshot())
    const result = applyChatEvent(
      state,
      event(3, 'message.delta', { message_id: 'm1', part_id: 'p1', index: 5, delta: '跳号' }),
    )
    expect(result.needSync).toBe(true)
  })

  it('message.completed 用完整对象替换同一消息，不产生第二块', () => {
    let state = applySnapshot(snapshot())
    state = applyChatEvent(
      state,
      event(3, 'message.delta', { message_id: 'm1', part_id: 'p1', index: 1, delta: '暂存' }),
    ).state
    state = applyChatEvent(
      state,
      event(4, 'message.completed', {
        message: {
          message_id: 'm1',
          turn_id: 't1',
          invocation_id: 'i1',
          role: 'assistant',
          purpose: 'final',
          status: 'completed',
          parts: [{ part_id: 'p1', type: 'text', text: '最终全文', last_index: 2 }],
          item_order: 1,
          revision: 4,
        },
      }),
    ).state
    const messages = state.items.filter((item) => item.type === 'message')
    expect(messages).toHaveLength(1)
    if (messages[0].type === 'message') {
      expect(messages[0].message.parts[0].text).toBe('最终全文')
      expect(messages[0].message.purpose).toBe('final')
    }
  })

  it('工具按 tool_call_id 更新同一张卡；交互按 interaction_id 原位更新', () => {
    let state = applySnapshot({ ...snapshot(), items: [] })
    const tool = {
      tool_call_id: 'tc1',
      turn_id: 't1',
      name: 'get_pods',
      status: 'started',
      item_order: 10,
      revision: 3,
    }
    state = applyChatEvent(state, event(3, 'tool.updated', { tool })).state
    state = applyChatEvent(
      state,
      event(4, 'tool.updated', { tool: { ...tool, status: 'succeeded', ok: true } }),
    ).state
    const tools = state.items.filter((item) => item.type === 'tool')
    expect(tools).toHaveLength(1)
    if (tools[0].type === 'tool') {
      expect(tools[0].tool.status).toBe('succeeded')
    }

    state = applyChatEvent(
      state,
      event(5, 'interaction.updated', {
        interaction: {
          interaction_id: 'a1',
          turn_id: 't1',
          kind: 'approval',
          status: 'pending',
          item_order: 11,
          revision: 5,
        },
      }),
    ).state
    expect(pendingInteraction(state)?.interaction_id).toBe('a1')
    state = applyChatEvent(
      state,
      event(6, 'interaction.updated', {
        interaction: {
          interaction_id: 'a1',
          turn_id: 't1',
          kind: 'approval',
          status: 'approved',
          item_order: 11,
          revision: 6,
        },
      }),
    ).state
    expect(pendingInteraction(state)).toBeNull()
    expect(state.items.filter((item) => item.type === 'interaction')).toHaveLength(1)
  })

  it('waiting 状态只由 turn.status 表达，终态后不再有活动 Turn', () => {
    let state = applySnapshot({ ...snapshot(), cursor: 0, turns: [], items: [] })
    state = applyChatEvent(
      state,
      event(1, 'turn.status', { turn_id: 't1', status: 'waiting', wait_reason: 'question' }),
    ).state
    expect(waitLabel(activeTurn(state))).toBe('等待你选择')
    expect(activityLabel(activeTurn(state))).toBe('等待你选择')
    state = applyChatEvent(
      state,
      event(2, 'turn.status', { turn_id: 't1', status: 'done' }),
    ).state
    expect(activeTurn(state)).toBeNull()
    expect(activityLabel(state.turns.t1)).toBe('完成')
  })

  it('空状态没有活动 Turn，也没有待答交互', () => {
    const state = emptyChatState()
    expect(activeTurn(state)).toBeNull()
    expect(pendingInteraction(state)).toBeNull()
  })
})
