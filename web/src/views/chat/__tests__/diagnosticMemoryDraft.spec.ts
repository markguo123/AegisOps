import { describe, expect, it } from 'vitest'
import { parseDiagnosticMemoryDraft } from '../diagnosticMemoryDraft'

const conclusion = [
  '已对齐发布和报错时间。',
  '### 可保留的排查经验',
  '主命名空间：payment',
  '关联命名空间：edge-system',
  '工作负载：payment-api',
  '经验：按时间对齐网关报错与旧 Pod 退出。',
].join('\n')

describe('parseDiagnosticMemoryDraft', () => {
  it('extracts the proposed scope and note from a completed conclusion', () => {
    expect(parseDiagnosticMemoryDraft(conclusion)).toEqual({
      namespace: 'payment',
      relatedNamespaces: ['edge-system'],
      workload: 'payment-api',
      text: '按时间对齐网关报错与旧 Pod 退出。',
    })
  })

  it('rejects missing scope, wildcard namespaces and extra trailing text', () => {
    expect(parseDiagnosticMemoryDraft(conclusion.replace('payment-api', ''))).toBeUndefined()
    expect(parseDiagnosticMemoryDraft(conclusion.replace('edge-system', '*'))).toBeUndefined()
    expect(parseDiagnosticMemoryDraft(conclusion + '\n附加操作')).toBeUndefined()
  })
})
