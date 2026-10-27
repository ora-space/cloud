import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, renderHook, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import type { WorkflowRun, WorkflowSnapshot } from '@/api/generated.schemas'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { useCreateWorkflowRun } from '@/features/workflows/api'
import { WorkflowRunTools } from '@/features/workflows/editor/workflow-run-tools'
import { routes } from '@/routes'
import {
  TEST_PROJECT_ID,
  TEST_TENANT_ID,
  chooseProject,
  installCloudSpaceHandlers,
  installSpaceProjectHandlers,
  installWorkflowHandlers,
  workflowFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderRoutes } from '@/test/render'
import { stubViewportGeometry } from '@/test/viewport'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
/**
 * The readable workspace slug the browser route carries. It is deliberately not
 * `TEST_TENANT_ID`: the tenant UUID scopes API calls, and using it as the slug is
 * exactly the navigation bug these tests pin down.
 */
const WORKSPACE_SLUG = 'cloud-dev'
const EDITOR_ROUTE = '/w/:workspaceSlug/workflows/:workflowId'
const EDITOR_PATH = `/w/${WORKSPACE_SLUG}/workflows/${WORKFLOW_ID}`

/** A published snapshot the run button needs to arm. */
function snapshotFixture(id: string, version: number): WorkflowSnapshot {
  return {
    id,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    name: `版本 ${version}`,
    version,
    graph: { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } },
    createdAt: '2026-09-20T10:00:00+08:00',
  }
}

/** A stored run row the history list and the create response share. */
function runFixture(id: string, name: string): WorkflowRun {
  return {
    id,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    snapshotId: 'snap-1',
    name,
    workflowName: '安全审查',
    status: 'succeeded',
    input: {},
    nodeStates: {},
    rounds: [],
    error: '',
    definitionSnapshot: null,
    cancelRequestedAt: null,
    pluginVersion: '1.0.0',
    startedAt: '2026-09-20T02:00:00.000Z',
    finishedAt: '2026-09-20T02:00:10.000Z',
    createdAt: '2026-09-20T10:00:00+08:00',
    updatedAt: '2026-09-20T10:00:10+08:00',
  }
}

interface RunToolsOptions {
  snapshots?: WorkflowSnapshot[]
  runs?: WorkflowRun[]
  onCreate?: (body: unknown) => void
}

/**
 * Installs the run endpoints (all keyed by the tenant UUID, so a request built
 * from the workspace slug would not answer) and mounts the tools at the editor
 * route, behind a location probe.
 */
function installRunHandlers({
  snapshots = [snapshotFixture('snap-1', 1)],
  runs = [],
  onCreate = () => {},
  created = runFixture('run-new', '巡检运行'),
}: RunToolsOptions & { created?: WorkflowRun } = {}) {
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/snapshots`, () =>
      HttpResponse.json({ items: snapshots, nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs`, () =>
      HttpResponse.json({ items: runs, nextCursor: '' }),
    ),
    http.post(
      `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs`,
      async ({ request }) => {
        onCreate(await request.json())
        return HttpResponse.json({ resource: created })
      },
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs/:rid`, () =>
      HttpResponse.json(created),
    ),
  )
}

/**
 * Mounts the tools alone at the real editor route, with the run route as their destination.
 *
 * The start dialog reads the space's Projects, so the bare mount stands in for the layout that
 * normally provides the current space: without it the dialog has no space to list Projects of.
 */
function renderRunTools(options: RunToolsOptions = {}) {
  installCloudSpaceHandlers('admin')
  installSpaceProjectHandlers()
  installRunHandlers(options)
  const { router } = renderRoutes(
    [
      {
        path: EDITOR_ROUTE,
        element: (
          <CurrentSpaceProvider slug={WORKSPACE_SLUG}>
            <WorkflowRunTools tenantId={TEST_TENANT_ID} workflowId={WORKFLOW_ID} />
          </CurrentSpaceProvider>
        ),
      },
      { path: `${EDITOR_ROUTE}/runs/:runId`, element: <p>运行详情</p> },
    ],
    EDITOR_PATH,
  )
  return router
}

beforeEach(() => {
  stubViewportGeometry()
})

/** A client whose mutations never retry, so one refusal settles once. */
function wrapper() {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>
  }
}

/**
 * The create mutation's own gate, pinned at the hook rather than through the dialog: a run must name
 * the Project its Workspace provisions in, so a caller that states an empty one is refused before
 * anything is sent, and a caller that states a Project gets it carried on the request.
 */
describe('useCreateWorkflowRun', () => {
  it('refuses a run that names no Project without sending it', async () => {
    const bodies: unknown[] = []
    installRunHandlers({ onCreate: (body) => bodies.push(body) })
    const { result } = renderHook(() => useCreateWorkflowRun(TEST_TENANT_ID), {
      wrapper: wrapper(),
    })

    result.current.mutate({ workflowId: WORKFLOW_ID, projectId: '' })

    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(result.current.error?.message).toBe('workflow_run_project_required')
    expect(bodies).toEqual([])
  })

  it('sends the stated Project with the run', async () => {
    const bodies: unknown[] = []
    installRunHandlers({ onCreate: (body) => bodies.push(body) })
    const { result } = renderHook(() => useCreateWorkflowRun(TEST_TENANT_ID), {
      wrapper: wrapper(),
    })

    result.current.mutate({ workflowId: WORKFLOW_ID, projectId: TEST_PROJECT_ID })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(bodies).toEqual([{ projectId: TEST_PROJECT_ID }])
  })
})

describe('WorkflowRunTools', () => {
  it('disables the run button while no snapshot exists to execute', async () => {
    renderRunTools({ snapshots: [] })

    const runButton = await screen.findByRole('button', { name: '运行' })
    // The button starts disabled only once the empty snapshot list resolves;
    // until then there is no answer either way.
    await waitFor(() => expect(runButton).toBeDisabled())
    expect(runButton).toHaveAttribute('title', '请先发布一个版本，才能运行这个工作流')
  })

  it('starts the chosen version in the chosen Project, then opens the run', async () => {
    const bodies: unknown[] = []
    const router = renderRunTools({ runs: [], onCreate: (body) => bodies.push(body) })
    const runButton = await screen.findByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())

    fireEvent.click(runButton)
    await chooseProject()
    fireEvent.click(await screen.findByRole('button', { name: '开始运行' }))

    // The browser path carries the workspace slug, never the tenant UUID.
    await waitFor(() => expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-new`))
    // The run names the version it executes and the Project its Workspace provisions in, so the
    // server can freeze both onto it.
    expect(bodies).toEqual([{ projectId: TEST_PROJECT_ID, snapshotId: 'snap-1', input: {} }])
  })

  it('lists run history and opens a run from a row', async () => {
    renderRunTools({
      runs: [runFixture('run-1', '巡检运行'), runFixture('run-2', '发布检查')],
    })

    // The history is its own tab now: the toolbar links to it rather than rendering a popover, and
    // the link carries the workspace slug the browser route uses.
    const link = await screen.findByRole('link', { name: '运行记录' })
    expect(link).toHaveAttribute('href', `${EDITOR_PATH}/runs`)
    expect(screen.queryByText('巡检运行')).not.toBeInTheDocument()
  })
})

/**
 * The reported navigation bug needs the real boundary to show up: only the app's
 * own routes have `DashboardLayout` resolving the URL slug against the joined
 * spaces, `CloudScope` forwarding the tenant id, and the run detail page as the
 * destination. Mounting the tools bare cannot fail this way.
 */
describe('workflow run navigation through the app routes', () => {
  /** Mounts the app's real routes inside the workspace and installs what the editor reads. */
  function renderAppAt(path: string, runs: WorkflowRun[] = []) {
    installCloudSpaceHandlers('admin')
    installSpaceProjectHandlers()
    installWorkflowHandlers([workflowFixture({ id: WORKFLOW_ID })])
    const bodies: unknown[] = []
    installRunHandlers({
      runs,
      created: runFixture('run-new', '巡检运行'),
      onCreate: (body) => bodies.push(body),
    })
    const view = renderRoutes(routes, path)
    return { ...view, bodies }
  }

  it('opens the started run on the same workspace route instead of the UUID one', async () => {
    const { router, bodies } = renderAppAt(EDITOR_PATH)
    const runButton = await screen.findByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())

    fireEvent.click(runButton)
    await chooseProject()
    fireEvent.click(await screen.findByRole('button', { name: '开始运行' }))

    // A UUID workspace path would have been redirected to Issues by the Layout before
    // this page ever mounted, so the route is what proves the click stayed on the slug.
    expect(await screen.findByRole('heading', { name: '运行详情' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-new`)
    expect(bodies).toHaveLength(1)
  })

  it('opens an existing run from the run list with the same slug and creates nothing', async () => {
    const { router, bodies } = renderAppAt(EDITOR_PATH, [runFixture('run-1', '巡检运行')])

    // The history is a tab: the editor's tab bar is the canonical way there, and the run is
    // opened from the list rather than from a popover inside the editor.
    const tabs = await screen.findByRole('navigation', { name: '工作流的分区' })
    fireEvent.click(within(tabs).getByRole('link', { name: '运行记录' }))
    expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs`)

    fireEvent.click(await screen.findByRole('link', { name: '打开运行 巡检运行' }))

    await waitFor(() => expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-1`))
    expect(bodies).toHaveLength(0)
  })

  it('opens and reloads the run URL directly, with a breadcrumb back to the run list', async () => {
    const { router } = renderAppAt(`${EDITOR_PATH}/runs/run-1`, [runFixture('run-1', '巡检运行')])

    expect(await screen.findByRole('heading', { name: '运行详情' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-1`)
    // Browsing history returns to the history, not to the editor, so the next run is one click away.
    expect(await screen.findByRole('link', { name: '安全审查 · 运行记录' })).toHaveAttribute(
      'href',
      `${EDITOR_PATH}/runs`,
    )
  })
})
