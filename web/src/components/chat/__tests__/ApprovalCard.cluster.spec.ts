import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'

import ApprovalCard, { type ApprovalUiData } from '@/components/chat/ApprovalCard.vue'

function approval(): ApprovalUiData {
  return {
    key: 'a-cluster',
    prompt: 'apply deployment',
    options: [{ label: 'confirm', value: 'yes' }],
    secret: false,
    decision: 'pending',
    tool: 'create_resource',
    risk: 'MEDIUM',
    summary: 'apply',
    resource: { kind: 'Deployment', name: 'web', namespace: 'team' },
    target: {
      cluster: 'cluster-b-id',
      namespace: 'team',
      kind: 'Deployment',
      name: 'web',
    },
  }
}

describe('ApprovalCard target cluster（P12.4）', () => {
  it('shows the bound cluster name instead of only the opaque ClusterID', () => {
    const wrapper = mount(ApprovalCard, {
      props: { approval: approval(), clusterLabel: '生产 B' },
    })
    const text = wrapper.text()
    expect(text).toContain('生产 B')
    expect(text).toContain('集群：')
  })

  it('falls back to the stable ClusterID when the display name is unknown', () => {
    const wrapper = mount(ApprovalCard, { props: { approval: approval() } })
    expect(wrapper.text()).toContain('cluster-b-id')
  })
})
