import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, renderHook, screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import type { WorkflowRun, WorkflowSnapshot } from '@/api/generated.schemas'
import { useCreateWorkflowRun } from '@/features/workflows/api'
import { WorkflowRunTools } from '@/features/workflows/editor/workflow-run-tools'
import { routes } from '@/routes'
import {
  TEST_TENANT_ID,
  installCloudSpaceHandlers,
  installWorkflowHandlers,
  workflowFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderRoutes } from '@/test/render'
import { stubViewportGeometry } from '@/test/viewport'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
/** The Project a run freezes; the editor has none to state yet, so only the hook tests name one. */
const PROJECT_ID = '55555555-5555-5555-5555-555555555555'
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

/** Mounts the tools alone at the real editor route, with the run route as their destination. */
function renderRunTools(options: RunToolsOptions = {}) {
  installCloudSpaceHandlers('admin')
  installRunHandlers(options)
  const { router } = renderRoutes(
    [
      {
        path: EDITOR_ROUTE,
        element: <WorkflowRunTools tenantId={TEST_TENANT_ID} workflowId={WORKFLOW_ID} />,
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
 * The create mutation's own gate, pinned at the hook because the editor cannot reach it: a run must
 * name the Project its Workspace provisions in, and the editor has none to state until the run form
 * that picks one lands. A caller that states a Project still gets a run, and the Project it stated
 * is what the request carries.
 */
describe('useCreateWorkflowRun', () => {
  it('refuses a run that names no Project without sending it', async () => {
    const bodies: unknown[] = []
    installRunHandlers({ onCreate: (body) => bodies.push(body) })
    const { result } = renderHook(() => useCreateWorkflowRun(TEST_TENANT_ID), {
      wrapper: wrapper(),
    })

    result.current.mutate({ workflowId: WORKFLOW_ID })

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

    result.current.mutate({ workflowId: WORKFLOW_ID, projectId: PROJECT_ID })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(bodies).toEqual([{ projectId: PROJECT_ID }])
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

  it('arms the run button once a snapshot exists, but starts no run it cannot state a Project for', async () => {
    const bodies: unknown[] = []
    const router = renderRunTools({ runs: [], onCreate: (body) => bodies.push(body) })
    const runButton = await screen.findByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())

    fireEvent.click(runButton)

    // A run must name the Project its Workspace provisions in, and the editor has none to state
    // yet: the refusal happens before the request, so nothing is created and nothing navigates.
    // The button goes pending and comes back armed, which is where the refusal settles.
    await waitFor(() => expect(runButton).toBeEnabled())
    expect(bodies).toEqual([])
    expect(router.state.location.pathname).toBe(EDITOR_PATH)
  })

  it('lists run history and opens a run from a row', async () => {
    const router = renderRunTools({
      runs: [runFixture('run-1', '巡检运行'), runFixture('run-2', '发布检查')],
    })

    fireEvent.click(await screen.findByRole('button', { name: '运行历史' }))

    fireEvent.click(await screen.findByRole('button', { name: '打开运行 巡检运行' }))
    await waitFor(() => expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-1`))
  })

  it('shows the quiet empty state when nothing has ever run', async () => {
    renderRunTools({ runs: [] })

    fireEvent.click(await screen.findByRole('button', { name: '运行历史' }))

    expect(await screen.findByText('还没有运行记录。')).toBeInTheDocument()
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

  it('starts no run, and stays on the same workspace route instead of the UUID one', async () => {
    const { router, bodies } = renderAppAt(EDITOR_PATH)
    const runButton = await screen.findByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())

    fireEvent.click(runButton)

    // A UUID workspace path would have been redirected to Issues by the Layout before
    // this page ever mounted, so the route is what proves the click stayed put.
    await waitFor(() => expect(runButton).toBeEnabled())
    expect(bodies).toHaveLength(0)
    expect(router.state.location.pathname).toBe(EDITOR_PATH)
    expect(screen.queryByRole('heading', { name: '运行详情' })).not.toBeInTheDocument()
  })

  it('opens an existing run from the history with the same slug and creates nothing', async () => {
    const { router, bodies } = renderAppAt(EDITOR_PATH, [runFixture('run-1', '巡检运行')])
    fireEvent.click(await screen.findByRole('button', { name: '运行历史' }))

    fireEvent.click(await screen.findByRole('button', { name: '打开运行 巡检运行' }))

    expect(await screen.findByRole('heading', { name: '运行详情' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-1`)
    expect(bodies).toHaveLength(0)
  })

  it('opens and reloads the run URL directly, with a breadcrumb back to the editor', async () => {
    const { router } = renderAppAt(`${EDITOR_PATH}/runs/run-1`, [runFixture('run-1', '巡检运行')])

    expect(await screen.findByRole('heading', { name: '运行详情' })).toBeInTheDocument()
    expect(router.state.location.pathname).toBe(`${EDITOR_PATH}/runs/run-1`)
    expect(await screen.findByRole('link', { name: '工作流' })).toHaveAttribute(
      'href',
      `/w/${WORKSPACE_SLUG}/workflows`,
    )
  })
})
