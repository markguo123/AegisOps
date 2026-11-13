import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import ConversationList from '@/components/chat/ConversationList.vue'
import type { SessionSummary } from '@/api/sessions'

/**
 * P12.4 会话归属展示：
 *  - 自己的会话保持“N 条”信息；
 *  - 管理员看到的他人会话在第二行显示 @owner，不新增第三行；
 *  - 搜索可以按 owner 名匹配。
 */

const now = new Date().toISOString()

const sessions: SessionSummary[] = [
  {
    id: 's-own',
    name: '我的会话',
    ownerId: 'u1',
    state: 'completed',
    messageCount: 7,
    lastModified: now,
    clusterId: 'c1',
  },
  {
    id: 's-other',
    name: '同事的会话',
    ownerId: 'u2',
    ownerName: '张三',
    state: 'completed',
    messageCount: 3,
    lastModified: now,
    clusterId: 'c1',
  },
]

function mountList() {
  return mount(ConversationList, {
    props: {
      sessions,
      activeId: null,
      loading: false,
      currentUserId: 'u1',
      clusterNames: { c1: '生产 B' },
    },
  })
}

describe('ConversationList owner（P12.4）', () => {
  it('他人会话显示 @owner，自己的会话仍显示消息条数', () => {
    const wrapper = mountList()
    const text = wrapper.text()
    expect(text).toContain('@张三')
    expect(text).toContain('7 条')
    expect(text).not.toContain('3 条')
  })

  it('搜索支持 owner 名称', async () => {
    const wrapper = mountList()
    const search = wrapper.find('input[placeholder="搜索会话"]')
    await search.setValue('张三')
    expect(wrapper.text()).toContain('同事的会话')
    expect(wrapper.text()).not.toContain('我的会话')
  })
})
