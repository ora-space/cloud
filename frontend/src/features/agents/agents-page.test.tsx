import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { db } from '@/mocks/data/store'
import { renderWithProviders } from '@/test/render'
import { AgentsPage } from './agents-page'

describe('Agent management entry', () => {
  it('offers real plugin and task routes without showing seeded Agents', () => {
    renderWithProviders(<AgentsPage slug="real-workspace" />)
    expect(screen.getByRole('heading', { name: '智能体管理暂不可用' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '管理插件' })).toHaveAttribute(
      'href',
      '/w/real-workspace/plugins',
    )
    expect(screen.getByRole('link', { name: '前往任务' })).toHaveAttribute(
      'href',
      '/w/real-workspace/issues',
    )
    for (const agent of db.agents) {
      expect(screen.queryByText(agent.name)).not.toBeInTheDocument()
    }
  })
})
