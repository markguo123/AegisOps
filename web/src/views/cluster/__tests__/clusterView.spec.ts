import { createPinia, setActivePinia } from 'pinia'
import { flushPromises, mount } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { USER_KEY } from '@/api/request'
import ClusterView from '@/views/cluster/ClusterView.vue'

/**
 * P12.4 集群工作台回归：
 *  1. 统计与列表来自 workspace 端点，展示检查时间/状态，不把“未检查”画成健康；
 *  2. 搜索按名称/Context 过滤；
 *  3. 无 cluster.manage 的角色看不到接入/更新/删除，但保留自己有权目标的问诊入口；
 *  4. 凭据明文不进入页面文本。
 */

const WORKSPACE = {
  dataSource: '平台受管集群注册表；检查时间为最近一次连测',
  generatedAt: '2026-11-13T05:00:00Z',
  summary: { total: 2, connected: 1, failed: 1, unchecked: 0 },
  items: [
    {
      cluster: {
        id: 'cluster-b',
        name: '生产 B',
        context: 'prod-b',
        server: 'https://b.example:6443',
        version: 'v1.30.2',
        status: 'connected',
        checkedAt: '2026-11-13T04:50:00Z',
        checkSummary: '连测成功：API Server 可达且凭据有效；不代表已具备全部业务 API 权限',
        createdAt: '2026-11-01T00:00:00Z',
        updatedAt: '2026-11-13T04:50:00Z',
      },
      checkState: 'ok',
      identityRecorded: true,
      access: [{ scopeType: 'namespace', namespaces: ['team'], actions: ['read', 'logs'] }],
      canManage: false,
      canChat: true,
      canScan: false,
      canViewReports: true,
    },
    {
      cluster: {
        id: 'cluster-c',
        name: '灾备 C',
        context: 'dr-c',
        server: 'https://c.example:6443',
        version: '',
        status: 'disconnected',
        checkedAt: null,
        checkSummary: '网络不可达或超时',
        createdAt: '2026-11-02T00:00:00Z',
        updatedAt: '2026-11-12T00:00:00Z',
      },
      checkState: 'unchecked',
      identityRecorded: false,
      access: [{ scopeType: 'namespace', namespaces: ['team'], actions: ['read'] }],
      canManage: false,
      canChat: true,
      canScan: false,
      canViewReports: true,
    },
  ],
}

function envelope(data: unknown): Response {
  return new Response(JSON.stringify({ request_id: 'req-test', timestamp: new Date().toISOString(), data }), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

async function mountWorkspace(): Promise<ReturnType<typeof mount>> {
  localStorage.setItem(
    USER_KEY,
    JSON.stringify({ userId: 'u1', username: 'viewer', displayName: 'viewer', role: 'user' }),
  )
  const pinia = createPinia()
  setActivePinia(pinia)
  const wrapper = mount(ClusterView, { global: { plugins: [pinia] } })
  await flushPromises()
  return wrapper
}

describe('ClusterView（P12.4 多集群工作台）', () => {
  beforeEach(() => {
    localStorage.clear()
    vi.stubGlobal(
      'fetch',
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input)
        if (url.includes('/clusters/workspace')) {
          return envelope(WORKSPACE)
        }
        throw new Error(`unexpected fetch: ${url}`)
      }),
    )
  })

  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('展示统计、检查时间与状态，不把未检查画成健康', async () => {
    const wrapper = await mountWorkspace()
    const text = wrapper.text()
    expect(text).toContain('生产 B')
    expect(text).toContain('灾备 C')
    expect(text).toContain('连测成功')
    expect(text).toContain('未检查')
    expect(text).toContain('网络不可达或超时')
    expect(text).toContain('检查于')
    expect(text).not.toContain('SECRET-TOKEN')
  })

  it('按名称/Context 搜索过滤目标', async () => {
    const wrapper = await mountWorkspace()
    const search = wrapper.find('input[placeholder="搜索名称 / Context / Server"]')
    expect(search.exists()).toBe(true)
    await search.setValue('prod-b')
    expect(wrapper.text()).toContain('生产 B')
    expect(wrapper.text()).not.toContain('灾备 C')
  })

  it('无 cluster.manage 的角色不出现接入/更新/删除入口', async () => {
    const wrapper = await mountWorkspace()
    // 抽屉标题也会渲染在 DOM 里，因此按“按钮文本”判断入口显隐。
    const buttonTexts = wrapper.findAll('button').map((button) => button.text())
    expect(buttonTexts.some((text) => text.includes('接入集群'))).toBe(false)
    expect(buttonTexts).not.toContain('更新凭据')
    expect(buttonTexts).not.toContain('删除')
    // 仍有自己有权目标的问诊/报告入口。
    expect(buttonTexts).toContain('问诊')
    expect(buttonTexts).toContain('报告')
  })
})
