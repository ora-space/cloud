import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { WorkflowsPage } from '@/features/workflows/workflows-page'
import {
  installCloudSpaceHandlers,
  installWorkflowHandlers,
  TEST_TENANT_ID,
  workflowFixture,
} from '@/test/cloud-handlers'
import { renderAtRoute } from '@/test/render'

/**
 * Mounts the list for the tenant `CloudScope` would resolve in the app, behind
 * a real route match so `useParams` supplies the workspace slug used for links.
 */
function renderList() {
  installCloudSpaceHandlers('admin')
  return renderAtRoute(
    '/w/:workspaceSlug/workflows',
    <WorkflowsPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/workflows',
  )
}

describe('WorkflowsPage', () => {
  it('lists the tenant workflows with their node counts', async () => {
    installWorkflowHandlers([
      workflowFixture({
        name: '安全审查',
        graph: {
          nodes: [
            { id: 'start', position: { x: 0, y: 0 }, data: { kind: 'start' } },
            { id: 'output', position: { x: 200, y: 0 }, data: { kind: 'output' } },
          ],
          edges: [],
        },
      }),
      workflowFixture({ id: '55555555-5555-5555-5555-555555555555', name: '发布流程' }),
    ])
    renderList()

    expect(await screen.findByText('安全审查')).toBeInTheDocument()
    expect(screen.getByText('发布流程')).toBeInTheDocument()
    // The node count is read out of the stored graph document, not a column.
    expect(screen.getByText('2 个节点')).toBeInTheDocument()
  })

  it('shows the empty state rather than an empty list', async () => {
    installWorkflowHandlers([])
    renderList()

    expect(await screen.findByText('还没有工作流')).toBeInTheDocument()
  })

  it('labels each row with its newest run and links straight into the run list', async () => {
    installWorkflowHandlers([
      workflowFixture({
        name: '安全审查',
        latestRun: {
          id: '77777777-7777-7777-7777-777777777777',
          name: '巡检运行',
          status: 'failed',
          snapshotId: '88888888-8888-8888-8888-888888888888',
          createdAt: '2026-10-24T02:00:00.000Z',
          startedAt: '2026-10-24T02:00:01.000Z',
          finishedAt: '2026-10-24T02:00:09.000Z',
        },
      }),
      workflowFixture({ id: '55555555-5555-5555-5555-555555555555', name: '发布流程' }),
    ])
    renderList()

    // The last run is part of the workflow row, so "did it work last time" is answerable without
    // opening the editor or the history.
    expect(await screen.findByText('失败')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: '打开安全审查最近一次运行' })).toHaveAttribute(
      'href',
      '/w/cloud-dev/workflows/44444444-4444-4444-4444-444444444444/runs/77777777-7777-7777-7777-777777777777',
    )
    // A workflow that never ran says so instead of rendering a status for nothing.
    expect(screen.getByText('从未运行')).toBeInTheDocument()
    // Both rows offer the run list without going through the editor first.
    const runsLinks = screen.getAllByRole('link', { name: '运行记录' })
    expect(runsLinks.map((link) => link.getAttribute('href'))).toEqual([
      '/w/cloud-dev/workflows/44444444-4444-4444-4444-444444444444/runs',
      '/w/cloud-dev/workflows/55555555-5555-5555-5555-555555555555/runs',
    ])
  })

  it('filters rows and reports a search that matches nothing', async () => {
    installWorkflowHandlers([
      workflowFixture({ name: '安全审查' }),
      workflowFixture({ id: '55555555-5555-5555-5555-555555555555', name: '发布流程' }),
    ])
    const user = userEvent.setup()
    renderList()
    await screen.findByText('安全审查')

    await user.type(screen.getByLabelText('搜索工作流'), '发布')

    await waitFor(() => {
      expect(screen.queryByText('安全审查')).not.toBeInTheDocument()
    })
    expect(screen.getByText('发布流程')).toBeInTheDocument()

    await user.type(screen.getByLabelText('搜索工作流'), 'zzz')
    expect(await screen.findByText('没有匹配的工作流')).toBeInTheDocument()
  })
})
