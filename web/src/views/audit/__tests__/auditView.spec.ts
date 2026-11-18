import { createMemoryHistory, createRouter } from 'vue-router'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, describe, expect, it, vi } from 'vitest'

import AuditView from '@/views/audit/AuditView.vue'

/**
 * P4.7 审计中心页面回归：页面只把筛选条件下推、只渲染后端字段白名单，
 * 详情才展开同一次操作的时间线。这里盯住三件事：
 *  1. 列表按后端给的顺序渲染，事件类型/结果/操作者是中文可读标签；
 *  2. 点开一行才请求详情，时间线按后端顺序展示，不在前端猜关联；
 *  3. 空列表给可执行空态，而不是一片空白。
 */

const Blank = { template: '<div />' }

const EVENTS = [
  {
    id: 10,
    timestamp: '2026-11-18T10:00:00Z',
    eventType: 'TOOL_CALL',
    action: 'tool-call.request',
    userId: 'u1',
    userName: '张三',
    clusterId: 'ecb4e9b95451d31a0d6c2ab7edce4f04',
    clusterName: '研究院验收云',
    resourceKind: 'Pod',
    resourceName: 'arc/api-0',
    parameterSummary: '{"command":"kubectl get pods"}',
    status: 'success',
  },
  {
    id: 9,
    timestamp: '2026-11-18T09:59:00Z',
    eventType: 'APPROVAL',
    action: 'approval.decision',
    userId: 'u1',
    userName: '张三',
    clusterId: 'ecb4e9b95451d31a0d6c2ab7edce4f04',
    clusterName: '研究院验收云',
    decision: 'approved',
    status: 'success',
  },
]

const DETAIL = {
  event: EVENTS[0],
  timeline: [
    { ...EVENTS[0], id: 8 },
    { ...EVENTS[0], id: 10 },
  ],
  timelineTruncated: false,
}

function envelope<T>(data: T): Response {
  return new Response(
    JSON.stringify({ request_id: 'req-audit', timestamp: new Date().toISOString(), data }),
    { status: 200, headers: { 'Content-Type': 'application/json' } },
  )
}

function installFetch(items: unknown[] = EVENTS): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input)
    const detailMatch = /^\/api\/v1\/audit\/events\/(\d+)$/.exec(url)
    if (detailMatch) {
      const id = Number(detailMatch[1])
      return envelope(id === 10 ? DETAIL : { event: EVENTS[1], timeline: [EVENTS[1]], timelineTruncated: false })
    }
    if (url.startsWith('/api/v1/audit/events')) {
      return envelope({ items, nextCursor: '', hasMore: false })
    }
    throw new Error('unexpected request: ' + url)
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function mountView() {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/app/audit', component: Blank },
      { path: '/app/chat', component: Blank },
    ],
  })
  await router.push('/app/audit')
  await router.isReady()
  const wrapper = mount(AuditView, { global: { plugins: [router] } })
  await flushPromises()
  return { wrapper, router }
}

afterEach(() => {
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('审计中心页面', () => {
  it('渲染事件流水：时间、操作者、类型、对象与结果都给中文标签', async () => {
    const fetchMock = installFetch()
    const { wrapper } = await mountView()

    const [url] = fetchMock.mock.calls[0] as [string]
    expect(url).toContain('/api/v1/audit/events?')
    expect(url).toContain('limit=50')

    const text = wrapper.text()
    expect(text).toContain('审计中心')
    expect(text).toContain('张三')
    expect(text).toContain('工具调用')
    expect(text).toContain('人工确认')
    expect(text).toContain('Pod/arc/api-0')
    expect(text).toContain('成功')
    // 集群列显示名称而不是长 ID；ID 只放在 title 里供悬停核对。
    expect(text).toContain('研究院验收云')
    expect(text).not.toContain('ecb4e9b95451d31a0d6c2ab7edce4f04')
    // 事件流水顺序与后端一致，不在前端重排。
    expect(wrapper.findAll('.event-row')).toHaveLength(2)
  })

  it('点开一行才请求详情，并展示时间线与已脱敏摘要', async () => {
    const fetchMock = installFetch()
    const { wrapper } = await mountView()

    await wrapper.findAll('.event-row')[0].trigger('click')
    await flushPromises()

    const detailCall = fetchMock.mock.calls.find(([url]) => String(url).includes('/audit/events/10'))
    expect(detailCall).toBeTruthy()

    // el-drawer 默认渲染在组件内（未 append-to-body）：从 wrapper 断言可见内容。
    await vi.waitFor(() => expect(wrapper.text()).toContain('审计详情'))
    const text = wrapper.text()
    expect(text).toContain('关联时间线')
    expect(text).toContain('参数摘要')
    expect(text).toContain('kubectl get pods')
  })

  it('无匹配事件时给出可执行空态', async () => {
    installFetch([])
    const { wrapper } = await mountView()
    expect(wrapper.text()).toContain('没有匹配的审计事件')
    expect(wrapper.findAll('.event-row')).toHaveLength(0)
  })
})
