import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { WorkflowEditorPage } from '@/features/workflows/workflow-editor-page'
import { WorkflowRunsPage } from '@/features/workflows/workflow-runs-page'
import { WorkflowVersionsPage } from '@/features/workflows/workflow-versions-page'
import { isJsonRecord } from '@/features/workflows/runtime/json-record'
import {
  installCloudSpaceHandlers,
  installSpaceProjectHandlers,
  TEST_TENANT_ID,
  chooseProject,
  workflowFixture,
  workflowSnapshotFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderRoutes } from '@/test/render'
import { stubViewportGeometry } from '@/test/viewport'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
const BASE = `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}`
const PATH = `/w/cloud-dev/workflows/${WORKFLOW_ID}`

beforeEach(() => stubViewportGeometry())

/** Mounts the real editor with a rejected write and destinations for the blocked routes. */
function renderFailedSave(status: 404 | 409) {
  installCloudSpaceHandlers('admin')
  installSpaceProjectHandlers()
  let writes = 0
  let createdRuns = 0
  server.use(
    http.get(BASE, () => HttpResponse.json(workflowFixture({ id: WORKFLOW_ID }))),
    http.get(`${BASE}/snapshots`, () =>
      HttpResponse.json({ items: [workflowSnapshotFixture(1)], nextCursor: '' }),
    ),
    http.get(`${BASE}/runs`, () => HttpResponse.json({ items: [], nextCursor: '' })),
    http.put(BASE, () => {
      writes += 1
      return HttpResponse.json(
        { code: status === 404 ? 'not_found' : 'workflow_conflict' },
        { status },
      )
    }),
    http.post(`${BASE}/runs`, () => {
      createdRuns += 1
      return HttpResponse.json({ resource: { id: 'run-new', workflowId: WORKFLOW_ID } })
    }),
  )
  const { router } = renderRoutes(
    [
      {
        path: '/w/:workspaceSlug/workflows/:workflowId',
        element: (
          <CurrentSpaceProvider slug="cloud-dev">
            <WorkflowEditorPage slug={TEST_TENANT_ID} />
          </CurrentSpaceProvider>
        ),
      },
      { path: '/w/:workspaceSlug/workflows/:workflowId/runs', element: <p>运行记录已打开</p> },
      {
        path: '/w/:workspaceSlug/workflows/:workflowId/runs/:runId',
        element: <p>运行详情已打开</p>,
      },
      { path: '/w/:workspaceSlug/issues', element: <p>议题列表已打开</p> },
    ],
    PATH,
  )
  return { router, writes: () => writes, createdRuns: () => createdRuns }
}

/** Makes one unsaved edit and asks to open the run history. */
async function attemptHistoryNavigation(): Promise<void> {
  const palette = await screen.findByRole('toolbar', { name: '添加工作流节点' })
  fireEvent.click(within(palette).getByRole('button', { name: '输出' }))
  fireEvent.click(
    within(screen.getByRole('navigation', { name: '工作流的分区' })).getByRole('link', {
      name: '运行记录',
    }),
  )
}

describe('workflow draft navigation', () => {
  it.each([
    { entry: '运行记录标签', label: '运行记录', destination: 'runs' },
    { entry: '版本标签', label: '版本', destination: 'versions' },
    { entry: '运行记录工具栏', label: '查看这个工作流的历次运行记录', destination: 'runs' },
    { entry: '版本工具栏', label: '查看已发布的版本并回滚', destination: 'versions' },
  ])(
    'keeps the edited canvas through $entry until its save completes',
    async ({ entry, label, destination }) => {
      installCloudSpaceHandlers('admin')
      installSpaceProjectHandlers()
      let stored = workflowFixture({ id: WORKFLOW_ID })
      let releaseSave: (() => void) | undefined
      const saveGate = new Promise<void>((resolve) => {
        releaseSave = () => resolve()
      })
      let writes = 0
      server.use(
        http.get(BASE, () => HttpResponse.json(stored)),
        http.get(`${BASE}/snapshots`, () => HttpResponse.json({ items: [], nextCursor: '' })),
        http.get(`${BASE}/runs`, () => HttpResponse.json({ items: [], nextCursor: '' })),
        http.put(BASE, async ({ request }) => {
          const body: unknown = await request.json()
          if (!isJsonRecord(body) || !isJsonRecord(body['graph'])) {
            return HttpResponse.json({ code: 'invalid_request' }, { status: 400 })
          }
          writes += 1
          await saveGate
          stored = workflowFixture({ id: WORKFLOW_ID, graph: body['graph'], version: 2 })
          return HttpResponse.json(stored)
        }),
      )
      const { router } = renderRoutes(
        [
          {
            path: '/w/:workspaceSlug/workflows/:workflowId',
            element: (
              <CurrentSpaceProvider slug="cloud-dev">
                <WorkflowEditorPage slug={TEST_TENANT_ID} />
              </CurrentSpaceProvider>
            ),
          },
          {
            path: '/w/:workspaceSlug/workflows/:workflowId/runs',
            element: (
              <CurrentSpaceProvider slug="cloud-dev">
                <WorkflowRunsPage slug={TEST_TENANT_ID} />
              </CurrentSpaceProvider>
            ),
          },
          {
            path: '/w/:workspaceSlug/workflows/:workflowId/versions',
            element: (
              <CurrentSpaceProvider slug="cloud-dev">
                <WorkflowVersionsPage slug={TEST_TENANT_ID} />
              </CurrentSpaceProvider>
            ),
          },
        ],
        PATH,
      )

      const palette = await screen.findByRole('toolbar', { name: '添加工作流节点' })
      fireEvent.click(within(palette).getByRole('button', { name: '输出' }))
      expect(screen.getByText(/1 个节点/)).toBeInTheDocument()

      const link = entry.endsWith('标签')
        ? within(screen.getByRole('navigation', { name: '工作流的分区' })).getByRole('link', {
            name: label,
          })
        : screen.getByTitle(label)
      fireEvent.click(link)
      await waitFor(() => expect(writes).toBe(1))
      expect(router.state.location.pathname).toBe(PATH)
      expect(screen.getByText(/1 个节点/)).toBeInTheDocument()

      await act(async () => releaseSave?.())
      await waitFor(() => expect(router.state.location.pathname).toBe(`${PATH}/${destination}`))
      fireEvent.click(
        within(screen.getByRole('navigation', { name: '工作流的分区' })).getByRole('link', {
          name: '编辑',
        }),
      )
      await screen.findByRole('toolbar', { name: '添加工作流节点' })
      expect(stored.graph).toMatchObject({
        nodes: [expect.objectContaining({ id: 'output-1' })],
      })
      expect(screen.getByText(/1 个节点/)).toBeInTheDocument()
    },
  )

  it.each([409, 404] as const)(
    'offers a way out after a %i save failure without repeating the rejected write',
    async (status) => {
      const { router, writes } = renderFailedSave(status)
      await attemptHistoryNavigation()

      expect(await screen.findByRole('heading', { name: '草稿保存失败' })).toBeInTheDocument()
      expect(writes()).toBe(1)
      fireEvent.click(screen.getByRole('button', { name: '留在编辑器' }))
      expect(router.state.location.pathname).toBe(PATH)
      expect(screen.getByText(/1 个节点/)).toBeInTheDocument()

      await act(async () => {
        await router.navigate('/w/cloud-dev/issues')
      })
      expect(await screen.findByRole('heading', { name: '草稿保存失败' })).toBeInTheDocument()
      expect(writes()).toBe(1)
      fireEvent.click(screen.getByRole('button', { name: '放弃修改并离开' }))
      expect(await screen.findByText('议题列表已打开')).toBeInTheDocument()
      expect(router.state.location.pathname).toBe('/w/cloud-dev/issues')
      expect(writes()).toBe(1)
    },
  )

  it('opens the created run after the member discards a draft that cannot be saved', async () => {
    const user = userEvent.setup()
    const { router, writes, createdRuns } = renderFailedSave(409)
    await attemptHistoryNavigation()
    await screen.findByRole('heading', { name: '草稿保存失败' })
    fireEvent.click(screen.getByRole('button', { name: '留在编辑器' }))

    const runButton = screen.getByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())
    await user.click(runButton)
    await chooseProject()
    await user.click(await screen.findByRole('button', { name: '开始运行' }))

    expect(await screen.findByRole('heading', { name: '草稿保存失败' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(PATH)
    expect(createdRuns()).toBe(1)
    expect(writes()).toBe(1)
    fireEvent.click(screen.getByRole('button', { name: '放弃修改并离开' }))
    expect(await screen.findByText('运行详情已打开')).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`${PATH}/runs/run-new`)
    expect(createdRuns()).toBe(1)
    expect(writes()).toBe(1)
  })
})
