import { mount, flushPromises } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import SecretReveal from '../SecretReveal.vue'
import { revealSecret } from '@/api/sessions'
vi.mock('@/api/sessions', () => ({ revealSecret: vi.fn() }))
describe('SecretReveal', () => {
  it('fetches only on explicit reveal and clears plaintext after the timeout', async () => {
    vi.useFakeTimers()
    vi.mocked(revealSecret).mockResolvedValue({
      values: { password: { value: 'test-password', encoding: 'utf8' } },
    })
    const wrapper = mount(SecretReveal, {
      props: { sessionId: 's', params: { namespace: 'team', name: 'db', keys: ['password'] } },
    })
    try {
      expect(revealSecret).not.toHaveBeenCalled()
      expect(wrapper.text()).not.toContain('test-password')
      await wrapper.find('button').trigger('click')
      await flushPromises()
      expect(wrapper.text()).toContain('test-password')
      expect(revealSecret).toHaveBeenCalledWith(
        's',
        { namespace: 'team', name: 'db', keys: ['password'] },
        expect.any(AbortSignal),
      )
      await vi.advanceTimersByTimeAsync(60_001)
      expect(wrapper.text()).not.toContain('test-password')
    } finally {
      wrapper.unmount()
      vi.useRealTimers()
      vi.clearAllMocks()
    }
  })
  it('hides values on blur and surfaces permission errors', async () => {
    vi.mocked(revealSecret).mockResolvedValueOnce({
      values: { password: { value: 'test-password', encoding: 'utf8' } },
    })
    const wrapper = mount(SecretReveal, {
      props: { sessionId: 's', params: { namespace: 'team', name: 'db', keys: ['password'] } },
    })
    await wrapper.find('button').trigger('click')
    await flushPromises()
    window.dispatchEvent(new Event('blur'))
    await flushPromises()
    expect(wrapper.text()).not.toContain('test-password')
    vi.mocked(revealSecret).mockRejectedValueOnce(new Error('你没有权限读取该 Secret'))
    await wrapper.find('button').trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('你没有权限')
    wrapper.unmount()
    vi.clearAllMocks()
  })
})
