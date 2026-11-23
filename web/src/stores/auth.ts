import { computed, ref } from 'vue'
import { defineStore } from 'pinia'

import { authApi, type AuthUser as ApiAuthUser, type AuthUserSource } from '@/api'
import { clearSession, readStoredUser, saveSession } from '@/api/request'

/** 平台固定两角色；未知角色名按最小权限 user 处理。 */
export type Role = 'admin' | 'user'

/** 前端会话视图：仅暴露展示所需字段，不含任何敏感信息。 */
export interface AuthUser {
  userId: string
  username: string
  displayName: string
  /** 账号来源；缺省表示升级前存下的本地会话。 */
  source?: AuthUserSource
  role: Role
}

function normalizeRole(name?: string | null): Role {
  const key = name?.trim().toLowerCase()
  if (key === 'admin' || key === 'user') {
    return key
  }
  return 'user'
}

function toAuthUser(apiUser: ApiAuthUser, roleName?: string): AuthUser {
  return {
    userId: apiUser.id,
    username: apiUser.username,
    displayName: apiUser.displayName || apiUser.username,
    source: apiUser.source,
    role: normalizeRole(roleName ?? apiUser.roleId),
  }
}

/**
 * 账号来源为 oidc 的账号是企业在 IdP 侧的账号，平台里那个 `oidc-<内部ID>` 用户名不是
 * 任何人输入的登录名，对外展示 IdP 回传的登录名；本地口令账号（含已绑定企业身份的
 * local+oidc）一律展示真实登录名，displayName 只在编辑与用户列表里作备注。
 */
const SSO_ACCOUNT_PREFIX = 'oidc-'

function isSSOAccount(current: AuthUser): boolean {
  // source 由 /auth/me 与登录响应给出；老会话里没有它才退回建档前缀判断。
  if (current.source) {
    return current.source === 'oidc'
  }
  return current.username.startsWith(SSO_ACCOUNT_PREFIX)
}

export const useAuthStore = defineStore('auth', () => {
  localStorage.removeItem('aegisops_token')
  const user = ref<AuthUser | null>(readStoredUser<AuthUser>())

  const isAuthenticated = computed(() => user.value !== null)
  const role = computed<Role>(() => user.value?.role ?? 'user')
  const loginName = computed<string>(() => {
    const current = user.value
    if (!current) {
      return ''
    }
    const name = current.displayName?.trim()
    if (isSSOAccount(current) && name) {
      return name
    }
    return current.username
  })

  async function login(username: string, password: string): Promise<void> {
    const session = await authApi.login({ username, password })
    user.value = toAuthUser(session.user)
    saveSession(user.value)
    await refreshMe()
  }

  async function refreshMe(): Promise<void> {
    const me = await authApi.me()
    user.value = toAuthUser(me.user, me.role.name)
    saveSession(user.value)
  }

  async function restore(): Promise<void> {
    try { await refreshMe() } catch { clearSession(); user.value = null }
  }

  async function logout(): Promise<void> {
    await authApi.logout()
    clearSession()
    user.value = null
  }

  return { user, role, isAuthenticated, loginName, login, logout, restore }
})
