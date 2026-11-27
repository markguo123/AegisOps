import { describe, expect, it } from 'vitest'

import type { ChatMessage } from '@/api/sessions'
import { buildSessionMarkdown, markdownFileName } from '../exportMarkdown'

const messages: ChatMessage[] = [
  { role: 'user', type: 'text', content: 'web 为什么不稳', timestamp: '2026-10-15T10:00:00Z' },
  { role: 'assistant', type: 'reasoning', content: '先看 Pod 列表', timestamp: '2026-10-15T10:00:01Z' },
  {
    role: 'tool',
    type: 'tool-call-response',
    name: 'get_pods',
    ok: true,
    content: '12 个 Pod，其中 1 个 CrashLoopBackOff',
    timestamp: '2026-10-15T10:00:02Z',
  },
  { role: 'assistant', type: 'text', content: '## 结论\n\nnginx 崩溃重启', timestamp: '2026-10-15T10:00:03Z' },
]

describe('buildSessionMarkdown（P7.5 导出）', () => {
  it('导出脱敏终端事实和错误，移除数据定界标签', () => {
    const md = buildSessionMarkdown([{ role: 'tool', type: 'tool-call-response', name: 'kubectl', ok: false, summary: '访问被拒绝', command: 'kubectl get pods -n other', stderr: 'forbidden', errorCode: 'SCOPE_DENIED', exitCode: 1, content: '<untrusted_k8s_data>\n访问被拒绝\n</untrusted_k8s_data>', timestamp: '2026-11-17T10:00:00Z', arguments: { token: 'never-export' }, data: { ModelOnly: 'never-export' } }], { id: 's' })
    expect(md).toContain('访问被拒绝')
    expect(md).toContain('kubectl get pods -n other')
    expect(md).toContain('SCOPE_DENIED')
    expect(md).toContain('forbidden')
    expect(md).not.toContain('untrusted_k8s_data')
    expect(md).not.toContain('never-export')
  })
  it('按 提问 → 思考 → 工具 → 报告 的顺序导出，并带上会话元信息', () => {
    const md = buildSessionMarkdown(messages, {
      id: '20261015-c851',
      name: 'default 体检',
      model: 'qwen-plus',
      exportedAt: new Date('2026-10-15T12:00:00Z'),
    })

    const order = ['# default 体检', '## 提问', '## 思考过程', '## 工具调用 · get_pods', '## 最终报告']
    let cursor = -1
    for (const marker of order) {
      const at = md.indexOf(marker)
      expect(at, `缺少 ${marker}`).toBeGreaterThan(-1)
      expect(at, `${marker} 顺序错误`).toBeGreaterThan(cursor)
      cursor = at
    }
    expect(md).toContain('- 会话 ID：`20261015-c851`')
    expect(md).toContain('- 模型：`qwen-plus`')
    expect(md).toContain('12 个 Pod，其中 1 个 CrashLoopBackOff')
  })

  it('工具失败时标注状态，且不导出原始参数/数据字段', () => {
    const md = buildSessionMarkdown(
      [
        {
          role: 'tool',
          type: 'tool-call-response',
          name: 'kubectl',
          ok: false,
          content: 'forbidden',
          arguments: { token: 'should-not-appear' },
          data: { secret: 'should-not-appear' },
          timestamp: '2026-10-15T10:00:02Z',
        },
      ],
      { id: 's1' },
    )
    expect(md).toContain('- 状态：失败')
    expect(md).not.toContain('should-not-appear')
  })
})

it('exports a partial result with its contract and safe evidence', () => {
  const md = buildSessionMarkdown([{
    role: 'tool',
    type: 'tool-call-response',
    name: 'get_resource',
    ok: true,
    resultStatus: 'partial',
    contractVersion: '4.0.5',
    content: '{"kind":"Pod","eventsError":"RBAC_DENIED"}',
    data: { ModelOnly: 'never-export' },
    timestamp: '2026-11-27T07:00:00Z',
  }], { id: 's1' })
  expect(md).toContain('- 状态：部分结果')
  expect(md).toContain('4.0.5')
  expect(md).toContain('RBAC_DENIED')
  expect(md).not.toContain('never-export')
})

describe('markdownFileName', () => {
  it('生成 aegisops-<会话名>-<时间>.md 并过滤非法字符', () => {
    expect(markdownFileName('default 体检 / prod:01', new Date('2026-10-15T09:05:00'))).toBe(
      'aegisops-default-体检-prod-01-20261015-0905.md',
    )
    expect(markdownFileName('', new Date('2026-10-15T09:05:00'))).toBe('aegisops-session-20261015-0905.md')
  })
})
