/**
 * 企业单点登录入口的位置与样式来源（源码级断言，口径同 loginTechScope.spec.ts）。
 *
 * 两个回归点：
 * 1. 位置：单点登录必须在「接入系统」之后，不能压在账号输入框上面；
 * 2. 样式：链接不许再借用 .btn —— .btn 靠 width/height 撑开胶囊，而 <a> 默认
 *    inline，宽高不生效，渐变只会画在文字那一小段上。
 */
import { readFileSync } from 'node:fs'
import path from 'node:path'

import { describe, expect, it } from 'vitest'

const vuePath = path.resolve(process.cwd(), 'src/views/LoginView.vue')
const source = readFileSync(vuePath, 'utf8')

describe('登录页企业单点登录入口', () => {
  it('排在接入系统按钮之后', () => {
    const submit = source.indexOf('type="submit"')
    const sso = source.indexOf('/api/v1/auth/oidc/start')
    expect(submit).toBeGreaterThan(0)
    expect(sso).toBeGreaterThan(submit)
  })

  it('用自己的次级按钮样式，不借用主按钮 .btn', () => {
    expect(source).not.toMatch(/class="btn"[^>]*auth\/oidc\/start/)
    expect(source).toMatch(/\.sso-btn\s*\{[^}]*display:\s*flex/)
  })
})
