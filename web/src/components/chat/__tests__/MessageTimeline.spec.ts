import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import MessageTimeline from '@/components/chat/MessageTimeline.vue'
import type { ChatInteraction, ChatItem, ChatMessage, ChatToolCall, ChatTurn } from '@/api/chat'

const FULL_REPORT = [
  '## default 命名空间体检报告',
  '',
  '**结论**：发现 3 个异常项',
  '',
  '1. 优先处理 nginx-7d9f8',
].join('\n')

let order = 0

function turn(patch: Partial<ChatTurn> = {}): ChatTurn {
  return {
    turn_id: 't1',
    session_id: 's1',
    input: '体检 default 命名空间',
    status: 'running',
    usage: { prompt_tokens: 3, completion_tokens: 2, total_tokens: 5 },
    revision: 1,
    ...patch,
  }
}

function message(patch: Partial<ChatMessage> & { parts?: ChatMessage['parts'] } = {}): ChatMessage {
  return {
    message_id: 'm1',
    turn_id: 't1',
    invocation_id: 'i1',
    role: 'assistant',
    purpose: 'final',
    status: 'completed',
    parts: [{ part_id: 'p1', type: 'text', text: FULL_REPORT, last_index: 1 }],
    item_order: ++order,
    revision: 1,
    ...patch,
  }
}

function tool(patch: Partial<ChatToolCall> = {}): ChatToolCall {
  return {
    tool_call_id: 'tc1',
    turn_id: 't1',
    name: 'get_pods',
    params: '{"namespace":"default"}',
    status: 'succeeded',
    ok: true,
    result: '1 个 Pod',
    summary: '成功 · 1 pods · 320ms',
    display_type: 'table',
    duration_ms: 320,
    risk: 'SAFE',
    item_order: ++order,
    revision: 2,
    ...patch,
  }
}

function approval(patch: Partial<ChatInteraction> = {}): ChatInteraction {
  return {
    interaction_id: 'a1',
    turn_id: 't1',
    kind: 'approval',
    status: 'pending',
    title: '审批请求',
    prompt: '确认执行 dry-run 后的 apply？',
    options: [{ label: '确认并执行', value: 'yes' }],
    tool: 'create_resource',
    risk: 'HIGH',
    item_order: ++order,
    revision: 3,
    ...patch,
  }
}

function items(...list: ChatItem[]): ChatItem[] {
  return list
}

function mountTimeline(props: {
  turns?: ChatTurn[]
  items?: ChatItem[]
  canDecide?: boolean
  canBranch?: boolean
}) {
  return mount(MessageTimeline, {
    props: { turns: props.turns ?? [turn()], items: props.items ?? [], canBranch: props.canBranch, ...(props.canDecide === undefined ? {} : { canDecide: props.canDecide }) },
  })
}

describe('MessageTimeline（规范对象渲染）', () => {
  it('只允许从已完成的最终回复发出分叉请求', async () => {
    const wrapper = mountTimeline({
      turns: [turn({ status: 'waiting' })],
      items: items({ type: 'message', item_order: 1, message: message() }),
      canBranch: true,
    })
    const branchButton = 'button[aria-label="从这里开启新会话"]'
    expect(wrapper.find(branchButton).exists()).toBe(false)
    await wrapper.setProps({ turns: [turn({ status: 'done' })] })
    const action = wrapper.get(branchButton)
    expect(action.text()).toBe('')
    expect(wrapper.get('.report-block').find(branchButton).exists()).toBe(false)
    await action.trigger('click')
    expect(wrapper.emitted('branch')).toEqual([['t1']])
    await wrapper.setProps({ canBranch: false })
    expect(wrapper.find(branchButton).exists()).toBe(false)
  })

  it('操作行紧贴正文外侧，只有最新回复的操作行常显', () => {
    const wrapper = mountTimeline({
      turns: [turn({ status: 'done' }), turn({ turn_id: 't2', status: 'done' })],
      items: items(
        { type: 'message', item_order: 1, message: message() },
        { type: 'message', item_order: 2, message: message({ message_id: 'm2', turn_id: 't2' }) },
      ),
    })
    expect(wrapper.findAll('.turn-question + .message-action-bar')).toHaveLength(2)
    expect(wrapper.findAll('.report-block + .message-action-bar')).toHaveLength(2)
    expect(wrapper.findAll('.message-shell.latest > .message-action-bar')).toHaveLength(1)
    expect(wrapper.find('#turn-t2 .message-shell.latest > .report-block').exists()).toBe(true)
  })

  it('大纲只收录用户提问，点击时展开回合、定位并短暂高亮', async () => {
    vi.useFakeTimers()
    const wrapper = mountTimeline({
      turns: [turn(), turn({ turn_id: 't2', input: '检查 Pod 日志' }), turn({ turn_id: 't3', input: '' })],
      items: items({ type: 'message', item_order: 1, message: message() }),
    })
    const scroll = vi.fn()
    const originalScroll = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollIntoView')
    Object.defineProperty(Element.prototype, 'scrollIntoView', { configurable: true, value: scroll })
    try {
      const entries = wrapper.findAll('nav[aria-label="提问大纲"] button')
      expect(entries.map(entry => entry.attributes('aria-label'))).toEqual(['体检 default 命名空间', '检查 Pod 日志'])
      await wrapper.get('#turn-t2 .turn-toggle').trigger('click')
      expect(wrapper.find('#question-t2').exists()).toBe(false)
      await entries[1].trigger('mouseenter')
      expect(scroll).not.toHaveBeenCalled()
      await entries[1].trigger('click')
      await flushPromises()
      expect(wrapper.get('#question-t2').classes()).toContain('question-highlight')
      expect(scroll).toHaveBeenCalledWith({ behavior: 'smooth', block: 'center' })
      await vi.advanceTimersByTimeAsync(1600)
      expect(wrapper.get('#question-t2').classes()).not.toContain('question-highlight')
    } finally {
      wrapper.unmount()
      if (originalScroll) Object.defineProperty(Element.prototype, 'scrollIntoView', originalScroll)
      else Reflect.deleteProperty(Element.prototype, 'scrollIntoView')
      vi.useRealTimers()
    }
  })
  it('同一消息的多段正文只渲染一块报告，最终回答单独标识', () => {
    const wrapper = mountTimeline({
      items: items({ type: 'message', item_order: 1, message: message() }),
    })
    expect(wrapper.findAll('.turn')).toHaveLength(1)
    expect(wrapper.text()).toContain('体检 default 命名空间')
    expect(wrapper.findAll('.report-block')).toHaveLength(1)
    expect(wrapper.text()).toContain('诊断回复')
    expect(wrapper.get('.md-body').text().split('发现 3 个异常项')).toHaveLength(2)
  })

  it('过程说明与最终回答分别标注，过程正文不丢失', () => {
    const wrapper = mountTimeline({
      items: items(
        { type: 'message', item_order: 1, message: message({ message_id: 'm1', purpose: 'commentary', parts: [{ part_id: 'p1', type: 'text', text: '先检查探针与 PVC 挂载。', last_index: 1 }] }) },
        { type: 'message', item_order: 2, message: message({ message_id: 'm2', purpose: 'final' }) },
      ),
    })
    expect(wrapper.text()).toContain('分析过程')
    expect(wrapper.text()).toContain('先检查探针与 PVC 挂载。')
    expect(wrapper.text()).toContain('诊断回复')
  })

  it('工具对象按 tool_call_id 只渲染一张卡，并保留同源摘要/形态/耗时', () => {
    const wrapper = mountTimeline({ items: items({ type: 'tool', item_order: 1, tool: tool() }) })
    const cards = wrapper.findAllComponents({ name: 'ToolCallCard' })
    expect(cards).toHaveLength(1)
    const data = cards[0].props('tool') as {
      summary?: string
      displayType?: string
      duration?: number | null
      risk?: string
    }
    expect(data.summary).toBe('成功 · 1 pods · 320ms')
    expect(data.displayType).toBe('table')
    expect(data.duration).toBe(320)
    expect(data.risk).toBe('SAFE')
  })

  it('工具失败只标记工具卡，不把整轮判为失败', () => {
    const wrapper = mountTimeline({
      turns: [turn({ status: 'done' })],
      items: items({
        type: 'tool',
        item_order: 1,
        tool: tool({ status: 'failed', ok: false, error_code: 'RBAC_DENIED', error: 'forbidden' }),
      }),
    })
    const data = wrapper.findComponent({ name: 'ToolCallCard' }).props('tool') as { status: string }
    expect(data.status).toBe('error')
    expect(wrapper.text()).toContain('已完成')
  })

  it('审批交互渲染审批卡；已决策后按钮消失', async () => {
    const wrapper = mountTimeline({ items: items({ type: 'interaction', item_order: 1, interaction: approval() }) })
    expect(wrapper.text()).toContain('审批请求')
    expect(wrapper.findAll('button').some((btn) => btn.text().includes('确认执行'))).toBe(true)

    await wrapper.setProps({
      items: items({ type: 'interaction', item_order: 1, interaction: approval({ status: 'approved' }) }),
    })
    expect(wrapper.text()).toContain('已通过')
  })

  it('只读角色（canDecide=false）下审批操作区收起', () => {
    const wrapper = mountTimeline({
      canDecide: false,
      items: items({ type: 'interaction', item_order: 1, interaction: approval() }),
    })
    expect(wrapper.text()).toContain('无审批权限')
  })

  it('问题交互渲染结构化选项，并把答案回传交给上层', async () => {
    const wrapper = mountTimeline({
      items: items({
        type: 'interaction',
        item_order: 1,
        interaction: {
          interaction_id: 'q1',
          turn_id: 't1',
          kind: 'question',
          status: 'pending',
          prompt: '保留 PVC 还是切换默认页？',
          options: [
            { label: '保留 PVC 补齐内容', value: 'keep', impact: '保留原数据' },
            { label: '暂不修改', value: 'skip' },
          ],
          item_order: 1,
          revision: 2,
        },
      }),
    })
    expect(wrapper.text()).toContain('保留 PVC 补齐内容')
    await wrapper.get('.question-option input').setValue(true)
    await wrapper.findAll('.question-actions button')[1].trigger('click')
    expect(wrapper.emitted('answer')?.[0]?.[0]).toMatchObject({
      interactionId: 'q1',
      status: 'selected',
      optionId: 'keep',
    })
  })

  it('Turn 等待态来自 wait_reason，不被“已完成”覆盖', () => {
    const wrapper = mountTimeline({
      turns: [turn({ status: 'waiting', wait_reason: 'approval' })],
      items: items({ type: 'interaction', item_order: 1, interaction: approval() }),
    })
    expect(wrapper.text()).toContain('等待你审批')
    expect(wrapper.text()).not.toContain('已完成')
  })

  it('回合失败时展示错误态，不伪造完成', () => {
    const wrapper = mountTimeline({
      turns: [turn({ status: 'failed', error: '权限不足：namespaces "web" is forbidden' })],
    })
    expect(wrapper.text()).toContain('失败')
    expect(wrapper.text()).toContain('权限不足')
  })

  it('没有 reasoning part 时不渲染空的思考面板，也不报错', () => {
    const wrapper = mountTimeline({ items: items({ type: 'message', item_order: 1, message: message() }) })
    expect(wrapper.findAll('.reasoning')).toHaveLength(0)
  })

  it('思考与工具保持 item_order 顺序，不被合并', () => {
    const wrapper = mountTimeline({
      items: items(
        { type: 'message', item_order: 1, message: message({ message_id: 'm0', purpose: 'commentary', parts: [{ part_id: 'pr', type: 'reasoning', text: '先看 Pod 列表', last_index: 1 }] }) },
        { type: 'tool', item_order: 2, tool: tool() },
        { type: 'message', item_order: 3, message: message({ message_id: 'm1', parts: [{ part_id: 'pr2', type: 'reasoning', text: '汇总异常项', last_index: 1 }] }) },
        { type: 'message', item_order: 4, message: message({ message_id: 'm2' }) },
      ),
    })
    const kinds = wrapper.findAll('.turn-items .reasoning, .turn-items .toolcard, .turn-items .report-block').map((el) => {
      const cls = el.classes()
      if (cls.includes('reasoning')) return 'reasoning'
      if (cls.includes('toolcard')) return 'tool'
      if (cls.includes('report-block')) return 'report'
      return cls.join('|')
    })
    expect(kinds).toEqual(['reasoning', 'tool', 'reasoning', 'report'])
  })

  it('告警 notice 以用户可读文本展示，不冒充工具失败', () => {
    const wrapper = mountTimeline({
      items: items({
        type: 'notice',
        item_order: 1,
        notice: {
          notice_id: 'n1',
          turn_id: 't1',
          severity: 'error',
          code: 'PERSISTENCE_UNAVAILABLE',
          message: '输出未能可靠保存，请重新同步',
          item_order: 1,
          revision: 2,
        },
      }),
    })
    expect(wrapper.text()).toContain('输出未能可靠保存')
    expect(wrapper.findComponent({ name: 'ToolCallCard' }).exists()).toBe(false)
  })

  it('P7.2 上滑后暂停跟随并出现"回到底部"，点击后恢复', async () => {
    const wrapper = mountTimeline({ items: items({ type: 'message', item_order: 1, message: message() }) })
    const timeline = wrapper.get('.timeline')
    const el = timeline.element as HTMLElement
    Object.defineProperty(el, 'scrollHeight', { value: 1000, configurable: true })
    Object.defineProperty(el, 'clientHeight', { value: 400, configurable: true })
    Object.defineProperty(el, 'scrollTo', { value: () => {}, configurable: true })

    el.scrollTop = 0
    await timeline.trigger('scroll')
    expect(wrapper.find('.follow-latest').exists()).toBe(true)

    await wrapper.get('.follow-latest').trigger('click')
    expect(wrapper.find('.follow-latest').exists()).toBe(false)
  })
})
