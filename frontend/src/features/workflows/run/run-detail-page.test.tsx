import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { WorkflowRun } from '@/api/generated.schemas'
import { WORKFLOW_RUN_POLL_MS } from '@/features/workflows/api'
import { WorkflowRunDetailPage } from '@/features/workflows/run/run-detail-page'
import { TEST_TENANT_ID, installCloudSpaceHandlers } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { settleUntil } from '@/test/polling'
import { RUN_SNAPSHOT as SNAPSHOT } from '@/test/run-snapshot'
import { stubViewportGeometry } from '@/test/viewport'
import { renderAtRoute } from '@/test/render'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
const RUN_ID = '55555555-5555-5555-5555-555555555555'
const RUN_PATH = `/w/:workspaceSlug/workflows/:workflowId/runs/:runId`
// The browser URL carries the readable workspace slug; the tenant id (the `slug`
// prop CloudScope forwards) only scopes the API calls below.
const RUN_URL = `/w/cloud-dev/workflows/${WORKFLOW_ID}/runs/${RUN_ID}`

/** A stored run row as the detail endpoint returns it. */
function runFixture(overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return {
    id: RUN_ID,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    snapshotId: 'snap-1',
    name: '巡检运行',
    workflowName: '安全审查',
    status: 'succeeded',
    input: {},
    nodeStates: { 'agent-1': { status: 'succeeded', output: { ok: true } } },
    rounds: [],
    error: '',
    definitionSnapshot: SNAPSHOT,
    startedAt: '2026-09-20T02:00:00.000Z',
    finishedAt: '2026-09-20T02:00:10.000Z',
    createdAt: '2026-09-20T02:00:00.000Z',
    updatedAt: '2026-09-20T02:00:10.000Z',
    ...overrides,
  }
}

/** Installs the run detail endpoint and mounts the page at the run route. */
function renderRun(result: () => Promise<Response>): void {
  installCloudSpaceHandlers('admin')
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/runs/:rid`, () => result()),
  )
  renderAtRoute(RUN_PATH, <WorkflowRunDetailPage slug={TEST_TENANT_ID} />, RUN_URL)
}

/** One node's card on the run overview canvas. */
function runCard(id: string): HTMLElement {
  const card = document.querySelector(`[data-workflow-run-node][data-workflow-node-id="${id}"]`)
  if (!(card instanceof HTMLElement)) {
    throw new Error(`the run canvas did not draw a card for "${id}"`)
  }
  return card
}

beforeEach(() => {
  stubViewportGeometry()
})

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('WorkflowRunDetailPage', () => {
  it('renders the report-shell skeleton while the run resolves', () => {
    renderRun(() => new Promise<Response>(() => {}))

    expect(screen.getByRole('heading', { name: '运行详情' })).toBeInTheDocument()
    expect(document.querySelector('[data-slot="skeleton"]')).toBeInTheDocument()
  })

  it('shows a not-found note for a missing or foreign run', async () => {
    renderRun(async () =>
      HttpResponse.json({ code: 'not_found', message: 'not_found' }, { status: 404 }),
    )

    expect(await screen.findByText('未找到该运行记录。')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: '巡检运行' })).not.toBeInTheDocument()
  })

  it('renders the loaded run header, colored overview and empty inspector', async () => {
    renderRun(async () => HttpResponse.json(runFixture()))

    expect(await screen.findByRole('heading', { name: '巡检运行' })).toBeInTheDocument()
    // The run header and the succeeded node card both read 成功.
    expect(screen.getAllByText('成功').length).toBeGreaterThan(0)
    await waitFor(() => expect(runCard('agent-1')).toHaveAttribute('aria-label', 'Agent 1: 成功'))
    expect(runCard('start-1')).toHaveAttribute('aria-label', '开始: 未执行')
    expect(screen.getByText('在画布上选择一个节点查看详情')).toBeInTheDocument()
  })

  it('opens a read-only inspector when a node is clicked', async () => {
    renderRun(async () => HttpResponse.json(runFixture()))
    await screen.findByRole('heading', { name: '巡检运行' })
    await waitFor(() => expect(runCard('agent-1')).toBeInTheDocument())

    fireEvent.click(runCard('agent-1'))

    expect(await screen.findByRole('heading', { name: 'Agent 1' })).toBeInTheDocument()
    const inspector = screen.getByRole('complementary', { name: '节点检视' })
    expect(within(inspector).getByRole('region', { name: '输出' })).toBeInTheDocument()
    expect(within(inspector).queryByRole('button')).not.toBeInTheDocument()
  })

  it('follows a running run to its terminal status without a reload', async () => {
    // Only the query's interval is faked, so the retries below flush real timers.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let reads = 0
    renderRun(async () => {
      reads += 1
      return HttpResponse.json(
        reads === 1
          ? runFixture({
              status: 'running',
              finishedAt: null,
              updatedAt: '2026-09-20T02:00:05.000Z',
              nodeStates: { 'agent-1': { status: 'running' } },
            })
          : runFixture({
              nodeStates: { 'agent-1': { status: 'succeeded', output: '审查通过' } },
            }),
      )
    })

    await settleUntil(() =>
      expect(runCard('agent-1')).toHaveAttribute('aria-label', 'Agent 1: 运行中'),
    )
    expect(screen.getAllByText('运行中').length).toBeGreaterThan(0)

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))

    await settleUntil(() =>
      expect(runCard('agent-1')).toHaveAttribute('aria-label', 'Agent 1: 成功'),
    )
    // The node the run never reached still reads as unrun: the statuses are
    // overlaid on the frozen snapshot, not on a re-read definition.
    expect(runCard('start-1')).toHaveAttribute('aria-label', '开始: 未执行')

    // The output the poll delivered is the node's evidence, one click away.
    fireEvent.click(runCard('agent-1'))
    const inspector = screen.getByRole('complementary', { name: '节点检视' })
    expect(within(inspector).getByRole('region', { name: '输出' })).toHaveTextContent('审查通过')
  })
})
