import { createApp } from 'vue'
import { createPinia } from 'pinia'
import ElementPlus from 'element-plus'
import zhCn from 'element-plus/es/locale/lang/zh-cn'
import 'element-plus/dist/index.css'
import 'element-plus/theme-chalk/dark/css-vars.css'

import App from './App.vue'
import { useAuthStore } from './stores/auth'
import { initTheme } from './composables/useTheme'
import request, { ApiRequestError } from './api/request'

import './styles/index.css'
// P7.6：聊天页科技感视觉体系（令牌与登录页同源，覆盖 index.css 的底色）
import './styles/tech-theme.css'

// P7.1：主题由 index.html 内联脚本提前设置（防首屏闪烁），此处绑定系统偏好监听并幂等兜底。
initTheme()

const app = createApp(App)
const pinia = createPinia()

app.use(pinia)

app.use(ElementPlus, { locale: zhCn })

async function bootstrap() {
  // Portal 只向站点首页透传 token。在初始化路由前取出并清理地址栏。
  const entry = new URL(window.location.href)
  if (entry.pathname === '/' && entry.searchParams.has('token')) {
    const tokens = entry.searchParams.getAll('token')
    entry.searchParams.delete('token')
    entry.searchParams.delete('loginTime')
    window.history.replaceState(null, '', entry.pathname + entry.search + entry.hash)
    try {
      if (tokens.length !== 1 || !tokens[0]) throw new Error('rejected')
      const { data } = await request<{ portalHomeUrl?: string }>('/auth/providers', {
        redirectOnUnauthorized: false,
      })
      if (!data?.portalHomeUrl) throw new Error('disabled')
      const portalOrigin = new URL(data.portalHomeUrl).origin
      const referrerOrigin = document.referrer ? new URL(document.referrer).origin : ''
      // An arbitrary link containing someone else's token must not silently
      // replace the browser's login session.
      if (referrerOrigin !== portalOrigin && !window.confirm('是否使用此链接中的效能平台身份登录 AegisOps？')) {
        throw new Error('cancelled')
      }
      await request('/login/parentToken', {
        method: 'POST',
        body: { token: tokens[0] },
        redirectOnUnauthorized: false,
      })
      window.history.replaceState(null, '', '/app/chat')
    } catch (err) {
      const message = err instanceof Error ? err.message : ''
      const knownReasons = ['disabled', 'rejected', 'unavailable', 'changed', 'rate-limit', 'cancelled']
      const reason = knownReasons.includes(message) ? message : err instanceof ApiRequestError ? 'unavailable' : 'rejected'
      window.history.replaceState(null, '', '/login?portalError=' + encodeURIComponent(reason))
    }
  }
  await useAuthStore(pinia).restore()
  const { default: router } = await import('./router')
  app.use(router)
  app.mount('#app')
}

void bootstrap()
