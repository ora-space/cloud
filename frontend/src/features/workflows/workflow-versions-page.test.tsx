import { fireEvent, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { Workflow as CloudWorkflow, WorkflowSnapshot } from '@/api/generated.schemas'
import { WorkflowVersionsPage } from '@/features/workflows/workflow-versions-page'
import {
  installCloudSpaceHandlers,
  installWorkflowHandlers,
  TEST_TENANT_ID,
  workflowFixture,
  workflowSnapshotFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderRoutes } from '@/test/render'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
const WORKSPACE_SLUG = 'cloud-dev'
const VERSIONS_PATH = `/w/${WORKSPACE_SLUG}/workflows/${WORKFLOW_ID}/versions`
const EDITOR_PATH = `/w/${WORKSPACE_SLUG}/workflows/${WORKFLOW_ID}`

/**
 * Mounts the versions tab and a stand-in for the editor it rolls back into, behind real route
 * matches so the page reads its workspace slug from the URL and its links are the app's own.
 */
function renderVersionsPage(snapshots: WorkflowSnapshot[], restored?: CloudWorkflow) {
  installCloudSpaceHandlers('admin')
  installWorkflowHandlers([workflowFixture({ id: WORKFLOW_ID, version: 2 })])
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/snapshots`, () =>
      HttpResponse.json({ items: snapshots, nextCursor: '' }),
    ),
    http.put(
      `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/snapshots/:snapshotId/restore`,
      () => HttpResponse.json(restored ?? workflowFixture({ id: WORKFLOW_ID, version: 3 })),
    ),
  )
  return renderRoutes(
    [
      {
        path: '/w/:workspaceSlug/workflows/:workflowId/versions',
        element: <WorkflowVersionsPage slug={TEST_TENANT_ID} />,
      },
      { path: '/w/:workspaceSlug/workflows/:workflowId', element: <p>编辑器</p> },
    ],
    VERSIONS_PATH,
  )
}

describe('WorkflowVersionsPage', () => {
  it('marks the versions tab as the current one', async () => {
    const { router } = renderVersionsPage([workflowSnapshotFixture(1)])

    const tabs = await screen.findByRole('navigation', { name: '工作流的分区' })
    const current = tabs.querySelector('[aria-current="page"]')
    expect(current?.textContent).toBe('版本')
    // Every tab is a real route, which is what makes the three surfaces shareable and bookmarkable.
    expect(router.state.location.pathname).toBe(VERSIONS_PATH)
  })

  it('lists the published versions with a rollback action each', async () => {
    renderVersionsPage([
      workflowSnapshotFixture(1, { name: '首发' }),
      workflowSnapshotFixture(2, { name: '第二版' }),
    ])

    expect(await screen.findByRole('button', { name: '回滚到版本 2' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '回滚到版本 1' })).toBeInTheDocument()
  })

  it('returns to the editor once a rollback replaced the live document', async () => {
    const { router } = renderVersionsPage([workflowSnapshotFixture(1, { name: '首发' })])

    fireEvent.click(await screen.findByRole('button', { name: '回滚到版本 1' }))
    fireEvent.click(await screen.findByRole('button', { name: '回滚' }))

    // The restored graph is what the editor now edits, so the page hands the member to it.
    await waitFor(() => expect(router.state.location.pathname).toBe(EDITOR_PATH))
    expect(await screen.findByText('编辑器')).toBeInTheDocument()
  })
})
