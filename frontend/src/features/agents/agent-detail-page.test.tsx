import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { renderAtRoute } from '@/test/render'
import { AgentDetailPage } from './agent-detail-page'

describe('old Agent detail links', () => {
  it('shows the same unavailable explanation without looking up the supplied id', () => {
    renderAtRoute(
      '/w/:workspaceSlug/agents/:agentId',
      <AgentDetailPage slug="real-workspace" />,
      '/w/real-workspace/agents/retired-demo-agent',
    )
    expect(screen.getByRole('heading', { name: '智能体管理暂不可用' })).toBeInTheDocument()
    expect(screen.queryByText('retired-demo-agent')).not.toBeInTheDocument()
    expect(screen.queryByText(/已分配任务/)).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: '管理插件' })).toHaveAttribute(
      'href',
      '/w/real-workspace/plugins',
    )
  })
})
