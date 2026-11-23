import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import type { ChatToolCall, ChatTurn } from '@/api/chat'
import TaskStatusBar from '@/components/chat/TaskStatusBar.vue'

const turn: ChatTurn = {
  turn_id: 'turn',
  session_id: 'session',
  input: '检查 Pod',
  status: 'running',
  usage: { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 },
  revision: 1,
}

const firstTool: ChatToolCall = {
  tool_call_id: 'one',
  turn_id: turn.turn_id,
  name: 'get_pods',
  status: 'running',
  item_order: 1,
  revision: 1,
}

describe('TaskStatusBar', () => {
  it('悬停展开，并使用原列表项实时显示工具终态', async () => {
    const wrapper = mount(TaskStatusBar, {
      props: { turn, tools: [firstTool], pending: null, stopping: false },
    })
    expect(wrapper.get('button').text()).toContain('执行中 · get_pods')
    await wrapper.trigger('mouseenter')
    const row = wrapper.get('li').element
    await wrapper.setProps({
      turn: { ...turn, status: 'done' },
      tools: [{ ...firstTool, status: 'succeeded', summary: 'pods=4' }],
    })
    expect(wrapper.get('li').element).toBe(row)
    expect(wrapper.get('li').text()).toContain('pods=4')
    expect(wrapper.get('button').text()).toContain('已完成 · 1 项任务')
    await wrapper.trigger('mouseleave')
    expect(wrapper.find('ul').exists()).toBe(false)
  })

  it('审批优先显示，写入结果未知时保留警示', async () => {
    const wrapper = mount(TaskStatusBar, {
      props: {
        turn: { ...turn, status: 'waiting', wait_reason: 'approval' },
        tools: [firstTool],
        pending: {
          interaction_id: 'approval',
          turn_id: turn.turn_id,
          tool_call_id: firstTool.tool_call_id,
          kind: 'approval',
          status: 'pending',
          item_order: 2,
          revision: 1,
        },
        stopping: false,
      },
    })
    await wrapper.get('button').trigger('click')
    expect(wrapper.get('li').text()).toContain('等待人工确认')
    await wrapper.setProps({
      turn: { ...turn, status: 'done' },
      tools: [{ ...firstTool, status: 'unknown' }],
      pending: null,
    })
    expect(wrapper.get('button').text()).toContain('有任务结果未知')
    expect(wrapper.get('li').text()).toContain('结果未知')
    await wrapper.trigger('keydown', { key: 'Escape' })
    expect(wrapper.find('ul').exists()).toBe(false)
  })
})
