import { createPinia, setActivePinia } from 'pinia'
import { mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter } from 'vue-router'
import { describe, expect, it } from 'vitest'

import { TOKEN_KEY, USER_KEY } from '@/api/request'
import AppLayout from '@/layouts/AppLayout.vue'

const Blank = { template: '<div />' }

interface StoredUser {
  userId: string
  username: string
  displayName?: string
  source?: 'local' | 'oidc' | 'local+oidc'
  role: 'admin' | 'operator' | 'user'
}

async function mountLayout(role: 'admin' | 'operator' | 'user', stored?: StoredUser) {
  localStorage.clear()
  localStorage.setItem(TOKEN_KEY, 'tok-test')
  localStorage.setItem(USER_KEY, JSON.stringify(stored ?? { userId: `u-${role}`, username: role, role }))
  setActivePinia(createPinia())

  const router = createRouter({
    history: createMemoryHistory(),
    routes: ['chat', 'cluster', 'skill', 'audit', 'settings', 'users'].map((name) => ({
      path: `/app/${name}`,
      component: Blank,
    })),
  })
  await router.push('/app/chat')
  await router.isReady()
  return mount(AppLayout, { global: { plugins: [router] } })
}

describe('AppLayout 权限菜单', () => {
  it.each(['user', 'operator'] as const)('%s 不展示配置中心和用户权限', async (role) => {
    const wrapper = await mountLayout(role)
    const menu = wrapper.find('.app-menu').text()
    expect(menu).toContain('问诊')
    expect(menu).not.toContain('配置中心')
    expect(menu).not.toContain('用户与权限')
  })

  it('admin 展示完整平台管理菜单', async () => {
    const wrapper = await mountLayout('admin')
    const menu = wrapper.find('.app-menu').text()
    expect(menu).toContain('配置中心')
    expect(menu).toContain('用户与权限')
  })

  it('治理菜单只有审计中心：审批不再有独立入口', async () => {
    const wrapper = await mountLayout('admin')
    const menu = wrapper.find('.app-menu').text()
    expect(menu).toContain('审计中心')
    expect(menu).not.toContain('审批中心')
    // 审计中心名称必须是一个整体，不能还挂着短名「审计」。
    expect(menu).not.toMatch(/(^|\s)审计(\s|$)/)
  })
})

describe('AppLayout 右上角登录名', () => {
  it('本地账号显示登录用户名', async () => {
    const wrapper = await mountLayout('user', {
      userId: 'u-local',
      username: 'user-test',
      displayName: '测试同学',
      source: 'local',
      role: 'user',
    })
    const text = wrapper.find('.user-menu').text()
    expect(text).toContain('user-test')
    expect(text).not.toContain('测试同学')
  })

  it('企业 SSO 账号显示 IdP 登录名而非 oidc- 建档串', async () => {
    const wrapper = await mountLayout('user', {
      userId: 'u-oidc',
      username: 'oidc-HOLOYFOLN5ER7D6VHTNDII6KVH',
      displayName: 'p6070718',
      source: 'oidc',
      role: 'user',
    })
    const text = wrapper.find('.user-menu').text()
    expect(text).toContain('p6070718')
    expect(text).not.toContain('oidc-')
  })
})
