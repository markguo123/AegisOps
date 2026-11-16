import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import ProcessGroup from '../ProcessGroup.vue'

describe('ProcessGroup', () => {
  it('folds completed history and preserves explicit reader expansion', async () => {
    const wrapper = mount(ProcessGroup, {
      props: { items: [], completed: true, failed: false },
      slots: { default: '分析详情' },
    })
    expect(wrapper.text()).not.toContain('分析详情')
    await wrapper.find('button').trigger('click')
    expect(wrapper.text()).toContain('分析详情')
    await wrapper.setProps({ completed: false })
    await wrapper.setProps({ completed: true })
    expect(wrapper.text()).toContain('分析详情')
  })
  it('folds at completion while failed analysis stays visible', async () => {
    const wrapper = mount(ProcessGroup, {
      props: { items: [], completed: false, failed: false },
      slots: { default: '分析详情' },
    })
    expect(wrapper.text()).toContain('分析详情')
    await wrapper.setProps({ completed: true })
    expect(wrapper.text()).not.toContain('分析详情')
    await wrapper.setProps({ completed: false, failed: true })
    expect(wrapper.text()).toContain('分析已中断')
    expect(wrapper.text()).toContain('分析详情')
  })
})
