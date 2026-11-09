import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { TOKEN_KEY, USER_KEY } from '@/api/request'

const authApi = {
  login: vi.fn(),
  logout: vi.fn(),
  me: vi.fn(),
}

vi.mock('@/api', () => ({ authApi }))

const { useAuthStore } = await import('@/stores/auth')

describe('auth store（P2.2/P2.3 真实认证接入）', () => {
  beforeEach(() => {
    localStorage.clear()
    setActivePinia(createPinia())
    authApi.login.mockReset()
    authApi.logout.mockReset()
    authApi.me.mockReset()
  })

  it('登录成功后写入令牌与用户，并以 /me 校正角色', async () => {
    authApi.login.mockResolvedValue({
      token: 'tok-1',
      user: { id: 'u1', username: 'admin', displayName: '管理员', roleId: 'role-admin' },
    })
    authApi.me.mockResolvedValue({
      user: { id: 'u1', username: 'admin', displayName:'管理员' },
      role: { name: 'admin', permissions: ['*'] },
    })

    const auth = useAuthStore()
    await auth.login('admin', 'pw')

    expect(auth.isAuthenticated).toBe(true)
    expect(auth.role).toBe('admin')
    expect(auth.user?.displayName).toBe('管理员')
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull()
  })

  it('未知角色名落到零权限 user，避免越权展示', async () => {
    authApi.login.mockResolvedValue({
      token: 'tok-2',
      user: { id: 'u2', username: 'ghost', displayName: '', roleId: 'mystery' },
    })
    authApi.me.mockResolvedValue({ user: { id: 'u2' }, role: { name: 'mystery' } })

    const auth = useAuthStore()
    await auth.login('ghost', 'pw')
    expect(auth.role).toBe('user')
  })

  it('restore 用本地会话 + /me 校正角色；无本地会话时清空', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok-3')
    localStorage.setItem(USER_KEY, JSON.stringify({ userId: 'u3', username: 'op', role: 'user' }))
    authApi.me.mockResolvedValue({ user: { id: 'u3' }, role: { name: 'user' } })

    const auth = useAuthStore()
    await auth.restore()
    expect(auth.isAuthenticated).toBe(true)
    expect(auth.role).toBe('user')

    localStorage.clear()
    setActivePinia(createPinia())
    authApi.me.mockRejectedValueOnce(new Error("expired"))
    const fresh = useAuthStore()
    await fresh.restore()
    expect(fresh.isAuthenticated).toBe(false)
    expect(fresh.user).toBeNull()
  })

  it('loginName：本地账号始终显示登录用户名，不受 displayName 影响', async () => {
    authApi.me.mockResolvedValue({
      user: { id: 'u-local', username: 'user-test', displayName: '测试同学' },
      role: { name: 'user', permissions: [] },
    })

    const auth = useAuthStore()
    await auth.restore()
    expect(auth.loginName).toBe('user-test')
  })

  it('loginName：企业 SSO 建档账号显示 IdP 登录名，不暴露 oidc- 随机串', async () => {
    authApi.me.mockResolvedValue({
      user: {
        id: 'u-oidc',
        username: 'oidc-HOLOYFOLN5ER7D6VHTNDII6KVH',
        displayName: 'p6070718',
        source: 'oidc',
      },
      role: { name: 'user', permissions: [] },
    })

    const auth = useAuthStore()
    await auth.restore()
    expect(auth.loginName).toBe('p6070718')
  })

  it('loginName：判来源看 source，不靠用户名长相', async () => {
    // 已绑定企业身份的本地账号：登录用的是自己的用户名，显示名只是备注。
    authApi.me.mockResolvedValue({
      user: { id: 'u-both', username: 'zhangsan', displayName: '张三', source: 'local+oidc' },
      role: { name: 'user', permissions: [] },
    })
    const bound = useAuthStore()
    await bound.restore()
    expect(bound.loginName).toBe('zhangsan')

    // 纯企业账号即便用户名不带 oidc- 前缀（管理员手工建的映射账号）也显示 IdP 登录名。
    setActivePinia(createPinia())
    authApi.me.mockResolvedValue({
      user: { id: 'u-sso', username: 'svc-build', displayName: 'ci-runner', source: 'oidc' },
      role: { name: 'user', permissions: [] },
    })
    const sso = useAuthStore()
    await sso.restore()
    expect(sso.loginName).toBe('ci-runner')
  })

  it('loginName：SSO 账号缺少 IdP 姓名时回落到用户名，未登录时为空', async () => {
    authApi.me.mockResolvedValue({
      user: { id: 'u-oidc-2', username: 'oidc-ABCDEFG', displayName: '', source: 'oidc' },
      role: { name: 'user', permissions: [] },
    })
    const auth = useAuthStore()
    await auth.restore()
    expect(auth.loginName).toBe('oidc-ABCDEFG')

    localStorage.clear()
    setActivePinia(createPinia())
    expect(useAuthStore().loginName).toBe('')
  })

  it('登出清理本地会话', async () => {
    localStorage.setItem(TOKEN_KEY, 'tok-4')
    authApi.logout.mockResolvedValue(undefined)

    const auth = useAuthStore()
    await auth.logout()
    expect(auth.isAuthenticated).toBe(false)
    expect(localStorage.getItem(TOKEN_KEY)).toBeNull()
  })
})
