import { afterEach, describe, expect, it, vi } from 'vitest'

import {
  AUDIT_EVENT_TYPES,
  auditDecisionLabel,
  auditEventLabel,
  auditEventTone,
  auditRiskLabel,
  auditStatusLabel,
  auditStatusTone,
  describeAuditError,
  getAuditEvent,
  listAuditEvents,
} from '@/api/audit'
import { TOKEN_KEY } from '@/api/request'

function envelopeResponse<T>(status: number, data?: T): Response {
  return new Response(
    JSON.stringify({ request_id: 'req-test', timestamp: new Date().toISOString(), data }),
    { status, headers: { 'Content-Type': 'application/json' } },
  )
}

function event(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    id: 7,
    timestamp: '2026-11-18T02:00:00Z',
    eventType: 'TOOL_CALL',
    action: 'tool-call.request',
    userName: '张三',
    userId: 'u1',
    clusterId: 'c1',
    resourceKind: 'Pod',
    resourceName: 'arc/api-0',
    parameterSummary: '{"command":"kubectl get pods"}',
    status: 'success',
    ...overrides,
  }
}

afterEach(() => {
  vi.unstubAllGlobals()
  localStorage.removeItem(TOKEN_KEY)
})

describe('audit API（P4.7 前端契约）', () => {
  it('listAuditEvents 只序列化非空筛选条件，并原样回传游标', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      envelopeResponse(200, { items: [event()], nextCursor: 'next-1', hasMore: true }),
    )
    vi.stubGlobal('fetch', fetchMock)
    localStorage.setItem(TOKEN_KEY, 'tok-audit')

    const result = await listAuditEvents({
      from: '2026-11-18T00:00:00.000Z',
      to: '2026-11-19T00:00:00.000Z',
      eventType: 'TOOL_CALL',
      userId: '',
      clusterId: 'c1',
      limit: 50,
      cursor: 'cursor-1',
    })

    const [url, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(url).toContain('/api/v1/audit/events?')
    const query = new URL(url, 'http://localhost').searchParams
    expect(query.get('from')).toBe('2026-11-18T00:00:00.000Z')
    expect(query.get('to')).toBe('2026-11-19T00:00:00.000Z')
    expect(query.get('eventType')).toBe('TOOL_CALL')
    expect(query.get('clusterId')).toBe('c1')
    expect(query.get('limit')).toBe('50')
    expect(query.get('cursor')).toBe('cursor-1')
    // 空字符串筛选不写入 query：未选择不是"筛选为空值"。
    expect(query.has('userId')).toBe(false)
    // 认证走 HttpOnly 会话 Cookie，前端不持有 Bearer Token（request.ts 不写 Authorization）。
    expect((init.headers as Headers).get('Authorization')).toBeNull()

    expect(result.items).toHaveLength(1)
    expect(result.hasMore).toBe(true)
    expect(result.nextCursor).toBe('next-1')
  })

  it('listAuditEvents 在 data 缺失时兜底为空列表', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(envelopeResponse(200, undefined)))
    await expect(listAuditEvents()).resolves.toEqual({ items: [], hasMore: false })
  })

  it('getAuditEvent 对 id 做 URL 编码并解析时间线', async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      envelopeResponse(200, {
        event: event(),
        timeline: [event({ id: 5 }), event({ id: 7 })],
        timelineTruncated: false,
      }),
    )
    vi.stubGlobal('fetch', fetchMock)

    const detail = await getAuditEvent('7/extra')
    const [url] = fetchMock.mock.calls[0] as [string]
    expect(url).toContain('/audit/events/7%2Fextra')
    expect(detail.event.id).toBe(7)
    expect(detail.timeline.map((row) => row.id)).toEqual([5, 7])
    expect(detail.timelineTruncated).toBe(false)
  })

  it('事件类型目录与后端常量同名，且每个类型都有中文短名与色调', () => {
    const values = AUDIT_EVENT_TYPES.map((item) => item.value)
    expect(values).toEqual(expect.arrayContaining(['LOGIN', 'APPROVAL', 'TOOL_CALL', 'RESOURCE_MUTATION', 'CONFIG_CHANGE', 'OTHER']))
    for (const item of AUDIT_EVENT_TYPES) {
      expect(item.label.length).toBeGreaterThan(0)
      expect(['info', 'success', 'warning', 'danger', 'mute']).toContain(auditEventTone(item.value))
      expect(auditEventLabel(item.value)).toBe(item.label)
    }
    // 未知类型原样显示，不吞信息。
    expect(auditEventLabel('CUSTOM_EVENT')).toBe('CUSTOM_EVENT')
  })

  it('状态/决策/风险翻译覆盖未记录与未知值', () => {
    expect(auditStatusLabel('success')).toBe('成功')
    expect(auditStatusLabel('error')).toBe('失败')
    expect(auditStatusLabel(undefined)).toBe('未记录')
    expect(auditStatusLabel('weird')).toBe('weird')
    expect(auditStatusTone('error')).toBe('danger')
    expect(auditStatusTone(undefined)).toBe('mute')
    expect(auditDecisionLabel('approved')).toBe('已批准')
    expect(auditDecisionLabel('no')).toBe('已拒绝')
    expect(auditRiskLabel('high')).toBe('高风险')
  })

  it('describeAuditError 把权限与未启用翻译成可执行提示', () => {
    expect(describeAuditError('加载审计事件', new Error('permission denied'))).toContain('audit.read')
    expect(describeAuditError('加载审计事件', new Error('audit query is not configured'))).toContain('未启用')
    expect(describeAuditError('加载审计事件', new Error('boom'))).toBe('加载审计事件失败：boom')
  })
})
