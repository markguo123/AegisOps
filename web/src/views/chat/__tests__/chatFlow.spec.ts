import { createPinia, setActivePinia } from 'pinia'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createMemoryHistory, createRouter } from 'vue-router'

import { TOKEN_KEY, USER_KEY } from '@/api/request'
import ChatView from '@/views/chat/ChatView.vue'
import ComposerBox from '@/components/chat/ComposerBox.vue'
import ConversationList from '@/components/chat/ConversationList.vue'
import type { ChatSnapshot } from '@/api/chat'

/**
 * 前端全链路冒烟（协议 v2，后端以 fetch 打桩）：
 * 登录态 → 会话列表 → 选择会话并订阅 v2 流 → 发送 → 审批/问题卡回传 → 停止。
 */

const SESSION = {
  ownerId: 'u1',
  clusterId: 'c1',
  id: 's1',
  name: '诊断 default 命名空间部署',
  state: 'running',
  model: 'qwen-plus',
  messageCount: 2,
  lastModified: new Date().toISOString(),
  token: { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 },
}

interface Call {
  url: string
  body: unknown
  headers?: HeadersInit
}

let calls: Call[] = []

function snapshot(cursor = 1, usage = { prompt_tokens: 11, completion_tokens: 7, total_tokens: 18 }): ChatSnapshot {
  return {
    protocol_version: 2,
    session_id: 's1',
    cursor,
    replay_floor: 0,
    turns: [
      {
        turn_id: 't0',
        session_id: 's1',
        input: '诊断 default 命名空间部署',
        status: 'done',
        usage,
        revision: cursor,
      },
    ],
    items: [],
  }
}

function sseStream(frames: unknown[], initial: ChatSnapshot = snapshot()): Response {
  const encoder = new TextEncoder()
  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      controller.enqueue(
        encoder.encode(
          `id: s1:${initial.cursor}\nevent: stream.snapshot\ndata: ${JSON.stringify({
            event_type: 'stream.snapshot',
            session_id: 's1',
            data: initial,
          })}\n\n`,
        ),
      )
      for (const frame of frames) {
        await new Promise((resolve) => setTimeout(resolve, 5))
        controller.enqueue(encoder.encode(`data: ${JSON.stringify(frame)}\n\n`))
      }
    },
  })
  return new Response(stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
}

function installFetch(
  frames: unknown[],
  skills: unknown[] = [],
  sessions: unknown[] = [SESSION],
  scope: unknown = {
    namespaces: ['mysql-dev'],
    clusterScoped: false,
    isAdmin: false,
    canWrite: true,
    canDestroy: false,
  },
  accessClusters?: unknown[],
  initial?: ChatSnapshot,
  models: unknown[] = [],
): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = String(input)
      const body = init.body ? JSON.parse(String(init.body)) : null
      calls.push({ url, body, headers: init.headers })
      if (url === '/api/v1/skills' && (init.method ?? 'GET') === 'GET') {
        return new Response(JSON.stringify({ data: skills }), { status: 200 })
      }
      if (url === '/api/v1/auth/access') {
        return new Response(
          JSON.stringify({
            request_id: 'r',
            timestamp: new Date().toISOString(),
            data: accessClusters ?? [
              {
                id: 'c1',
                name: 'prod',
                access: {
                  rules: [
                    {
                      scopeType: 'namespace',
                      namespaces: (scope as { namespaces: string[] }).namespaces,
                      actions: ['read'],
                    },
                  ],
                },
              },
            ],
          }),
          { status: 200 },
        )
      }
      if (url === '/api/v1/sessions' && (init.method ?? 'GET') === 'GET') {
        return new Response(JSON.stringify(sessions), { status: 200 })
      }
      if (url === '/api/v1/sessions/models') {
        return new Response(JSON.stringify(models), { status: 200 })
      }
      if (url === '/api/v1/sessions' && init.method === 'POST') {
        return new Response(JSON.stringify(SESSION), { status: 200 })
      }
      if (url.includes('/stream')) {
        return sseStream(frames, initial ?? snapshot())
      }
      if (url.endsWith('/send-message')) {
        return new Response(JSON.stringify({ sessionId: 's1', turnId: 't1', protocolVersion: 2 }), {
          status: 202,
        })
      }
      if (url.endsWith('/approve') || url.endsWith('/cancel') || url.endsWith('/question')) {
        return new Response(JSON.stringify({ sessionId: 's1' }), { status: 200 })
      }
      return new Response('{}', { status: 200 })
    }),
  )
}

function postCallsSuffix(suffix: string): Call[] {
  return calls.filter((call) => call.url.endsWith(suffix))
}

async function mountChat(frames: unknown[] = []) {
  installFetch(frames)
  const wrapper = mount(ChatView, { attachTo: document.body })
  await vi.waitFor(() => expect(wrapper.text()).toContain(SESSION.name))
  await wrapper.find('.session-item').trigger('click')
  await vi.waitFor(() => expect(wrapper.find('textarea').exists()).toBe(true))
  await vi.waitFor(() => expect(wrapper.text()).toContain('Token 用量'))
  return wrapper
}

enableAutoUnmount(afterEach)

describe('ChatView 全链路冒烟（协议 v2）', () => {
  beforeEach(() => {
    calls = []
    localStorage.clear()
    localStorage.setItem(TOKEN_KEY, 'tok-1')
    localStorage.setItem(USER_KEY, JSON.stringify({ userId: 'u1', username: 'admin', role: 'admin' }))
    sessionStorage.clear()
    setActivePinia(createPinia())
  })

  it('工作台新建参数只消费一次，重新挂载不重复创建', async () => {
    installFetch([])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/chat', component: ChatView }],
    })
    await router.push('/chat?cluster=c1&new=1')
    await router.isReady()
    const first = mount(ChatView, { global: { plugins: [router] } })
    await vi.waitFor(() =>
      expect(calls.filter((c) => c.url === '/api/v1/sessions' && c.body).length).toBe(1),
    )
    expect(router.currentRoute.value.query.new).toBeUndefined()
    first.unmount()
    mount(ChatView, { global: { plugins: [router] } })
    await vi.waitFor(() => expect(router.currentRoute.value.query.cluster).toBeUndefined())
    expect(calls.filter((c) => c.url === '/api/v1/sessions' && c.body)).toHaveLength(1)
  })

  it('审计中心会话入口：?session= 直接订阅该会话且参数只消费一次', async () => {
    installFetch([])
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [{ path: '/chat', component: ChatView }],
    })
    await router.push('/chat?session=s1')
    await router.isReady()
    mount(ChatView, { global: { plugins: [router] } })
    await vi.waitFor(() =>
      expect(calls.some((c) => c.url.includes('/api/v1/sessions/s1/stream'))).toBe(true),
    )
    expect(router.currentRoute.value.query.session).toBeUndefined()
  })

  it('多个集群时点击新建后选择目标，不改变原会话', async () => {
    installFetch([], [], [SESSION], undefined, [
      { id: 'c1', name: '生产云', access: { rules: [] } },
      { id: 'c2', name: '验收云', access: { rules: [] } },
    ])
    const wrapper = mount(ChatView, { attachTo: document.body })
    await vi.waitFor(() => expect(wrapper.find('.session-item').exists()).toBe(true))
    wrapper.getComponent(ConversationList).vm.$emit('create')
    await vi.waitFor(() => expect(document.body.textContent).toContain('选择问诊集群'))
    const option = [...document.querySelectorAll<HTMLButtonElement>('.cluster-dialog-option')].find(
      (el) => el.textContent?.includes('验收云'),
    )
    expect(option).toBeTruthy()
    option!.click()
    await vi.waitFor(() =>
      expect(
        calls.some(
          (c) => c.url === '/api/v1/sessions' && (c.body as { clusterId?: string })?.clusterId === 'c2',
        ),
      ).toBe(true),
    )
  })

  it('加载会话列表并从规范快照展示会话级 Token', async () => {
    const wrapper = await mountChat()
    expect(wrapper.text()).toContain('2 条')
    expect(wrapper.text()).toContain('18')
    expect(wrapper.text()).toContain('Token 用量')
  })

  it('新会话展示服务端授权范围，明确 namespace 与集群级资源边界', async () => {
    const emptySession = { ...SESSION, id: 'empty', name: '新会话', messageCount: 0, token: null }
    installFetch([], [], [emptySession], undefined, undefined, snapshot(0, { prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 }))
    const wrapper = mount(ChatView, { attachTo: document.body })
    await vi.waitFor(() => expect(wrapper.find('.session-item').exists()).toBe(true))
    await wrapper.find('.session-item').trigger('click')
    await vi.waitFor(() => expect(wrapper.text()).toContain('授权集群：prod'))
    expect(wrapper.text()).toContain('mysql-dev')
    expect(wrapper.text()).toContain('集群对象与命名空间分别授权')
  })

  it('v2 流渲染审批卡，运行中不允许重复发送，审批回传字段正确', async () => {
    const wrapper = await mountChat([
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't1',
        event_type: 'turn.status',
        seq: 2,
        timestamp: new Date().toISOString(),
        data: { turn_id: 't1', status: 'running', input: '修复它', activity: 'checking' },
      },
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't1',
        event_type: 'interaction.updated',
        seq: 3,
        timestamp: new Date().toISOString(),
        data: {
          interaction: {
            interaction_id: 'int-once',
            turn_id: 't1',
            kind: 'approval',
            status: 'pending',
            title: '审批请求',
            prompt: '确认执行 dry-run 后的 apply？',
            options: [{ label: '确认执行', value: 'yes' }],
            tool: 'create_resource',
            risk: 'HIGH',
            item_order: 3,
            revision: 3,
          },
        },
      },
    ])

    await vi.waitFor(() => expect(wrapper.text()).toContain('审批请求'))

    expect(wrapper.findAll('button').some((btn) => btn.text() === '发送')).toBe(false)

    const approve = wrapper.findAll('button').find((btn) => btn.text().includes('确认执行'))!
    await approve.trigger('click')
    await vi.waitFor(() => expect(postCallsSuffix('/approve')).toHaveLength(1))
    expect(postCallsSuffix('/approve')[0].body).toEqual({
      choice: 'yes',
      approvalId: 'int-once',
      expectedRevision: 3,
    })
  })

  it('v2 流渲染结构化问题卡，提交走 question 端点', async () => {
    const wrapper = await mountChat([
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't1',
        event_type: 'turn.status',
        seq: 2,
        timestamp: new Date().toISOString(),
        data: { turn_id: 't1', status: 'waiting', wait_reason: 'question', input: '检查 nginx' },
      },
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't1',
        event_type: 'interaction.updated',
        seq: 3,
        timestamp: new Date().toISOString(),
        data: {
          interaction: {
            interaction_id: 'q1',
            turn_id: 't1',
            kind: 'question',
            status: 'pending',
            prompt: '保留 PVC 还是切换默认页？',
            options: [{ label: '保留 PVC 补齐内容', value: 'keep' }],
            item_order: 3,
            revision: 3,
          },
        },
      },
    ])
    await vi.waitFor(() => expect(wrapper.text()).toContain('保留 PVC 补齐内容'))
    expect(wrapper.text()).toContain('等待你选择')

    await wrapper.get('.question-option input').setValue(true)
    await wrapper.findAll('.question-actions button')[1].trigger('click')
    await vi.waitFor(() => expect(postCallsSuffix('/question')).toHaveLength(1))
    expect(postCallsSuffix('/question')[0].body).toMatchObject({
      id: 'q1',
      session_id: 's1',
      turn_id: 't1',
      status: 'selected',
      option_id: 'keep',
    })
  })

  it('将所选目录模型和幂等键一起提交', async () => {
    installFetch([], [], [SESSION], undefined, undefined, undefined, [
      { id: 'qwen-plus', context_window: 65536 },
      { id: 'private-128k', context_window: 131072 },
    ])
    const wrapper = mount(ChatView, { attachTo: document.body })
    await vi.waitFor(() => expect(wrapper.find('.session-item').exists()).toBe(true))
    await wrapper.find('.session-item').trigger('click')
    const composer = wrapper.findComponent(ComposerBox)
    await vi.waitFor(() => expect(composer.props('models')).toContain('private-128k'))
    composer.vm.$emit('select-model', 'private-128k')
    await wrapper.find('textarea').setValue('检查资源')
    const send = wrapper.findAll('button').find(button => button.text() === '发送')!
    await send.trigger('click')
    await vi.waitFor(() => expect(postCallsSuffix('/send-message')).toHaveLength(1))
    const [request] = postCallsSuffix('/send-message')
    expect(request.body).toEqual({ query: '检查资源', modelId: 'private-128k' })
    expect(new Headers(request.headers).get('Idempotency-Key')).toBeTruthy()
  })

  it('运行中的 Turn 驱动停止按钮，回传 cancel', async () => {
    const wrapper = await mountChat([
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't2',
        event_type: 'turn.status',
        seq: 2,
        timestamp: new Date().toISOString(),
        data: { turn_id: 't2', status: 'running', input: '检查集群', activity: 'generating' },
      },
    ])
    await vi.waitFor(() => expect(wrapper.text()).toContain('检查集群'))

    const stop = wrapper.findAll('button').find((btn) => btn.text().includes('停止'))!
    await stop.trigger('click')
    await vi.waitFor(() => expect(postCallsSuffix('/cancel')).toHaveLength(1))
  })

  it('分别展示会话累计 Token 和上次请求的网关计量', async () => {
    const wrapper = await mountChat([
      {
        protocol_version: 2,
        session_id: 's1',
        turn_id: 't3',
        event_type: 'turn.status',
        seq: 2,
        timestamp: new Date().toISOString(),
        data: {
          turn_id: 't3',
          status: 'done',
          input: '再查一次',
          usage: { prompt_tokens: 16, completion_tokens: 12, total_tokens: 28 },
          context_budget: { window: 128000, output_reserve: 4096, input_estimate: 90000, input_actual: 30976 },
        },
      },
    ])
    await vi.waitFor(() => {
      expect(wrapper.text()).toContain('Token 用量46')
      expect(wrapper.find('.chat-budget-button').text()).toContain('约 25%')
    })
  })

  it('P3.8.4：@技能名 从查询剥离并随 skills 字段提交', async () => {
    const skill = {
      name: 'k8s-mysql-troubleshoot',
      spec: {
        description: 'Kubernetes 上 MySQL 故障排查方法论',
        name: 'k8s-mysql-troubleshoot',
        type: 'procedural',
        domain: 'database',
        version: '1.0.0',
        kinds: [],
        risk: { default: 'medium' },
        source: 'custom',
      },
      source: 'custom',
      enabled: true,
      hitCount: 0,
      createdAt: '',
      updatedAt: '',
    }
    installFetch([], [skill])
    const wrapper = mount(ChatView, { attachTo: document.body })
    await vi.waitFor(() => expect(wrapper.text()).toContain(SESSION.name))
    await wrapper.find('.session-item').trigger('click')
    await vi.waitFor(() => expect(wrapper.text()).toContain('Token 用量'))
    const composer = wrapper.findComponent(ComposerBox)
    await vi.waitFor(() => expect(composer.vm.availableSkills.length).toBe(1))

    await wrapper.find('textarea').setValue('@k8s-mysql-troubleshoot 看下 default 的 mysql 数据')
    await vi.waitFor(() => expect(wrapper.find('.mention-popup').exists()).toBe(false))
    const send = wrapper.findAll('button').find((btn) => btn.text() === '发送')!
    await send.trigger('click')
    await vi.waitFor(() => expect(postCallsSuffix('/send-message')).toHaveLength(1))
    expect(postCallsSuffix('/send-message')[0].body).toEqual({
      query: '看下 default 的 mysql 数据',
      skills: ['k8s-mysql-troubleshoot'],
    })
  })
})
