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
    cancelRequestedAt: null,
    pluginVersion: '1.0.0',
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

  it('hides the started and finished times while the run has not set them', async () => {
    renderRun(async () =>
      HttpResponse.json(
        runFixture({ status: 'pending', startedAt: '', finishedAt: '', nodeStates: {} }),
      ),
    )

    await screen.findByRole('heading', { name: '巡检运行' })
    expect(screen.getByText('创建于', { exact: false })).toBeInTheDocument()
    expect(screen.queryByText('开始于', { exact: false })).not.toBeInTheDocument()
    expect(screen.queryByText('结束于', { exact: false })).not.toBeInTheDocument()
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

/** A frozen Loop whose body is an entry, a writer and an early exit. */
const LOOP_SNAPSHOT: Record<string, unknown> = {
  nodes: [
    {
      id: 'loop-1',
      type: 'workflow',
      position: { x: 0, y: 0 },
      initialWidth: 620,
      initialHeight: 300,
      data: {
        kind: 'loop',
        title: '循环',
        description: '',
        loopConfig: {
          maxIterations: 5,
          variables: [],
          until: { logic: 'and', conditions: [] },
          outputs: [],
        },
      },
    },
    ...[
      ['entry', 'start', 40],
      ['writer', 'agent', 300],
      ['exit', 'loopExit', 560],
    ].map(([id, kind, x]) => ({
      id,
      type: 'workflow',
      position: { x, y: 145 },
      parentId: 'loop-1',
      data: { kind, title: id, description: '', containerId: 'loop-1' },
    })),
  ],
  edges: [{ id: 'e-entry-writer', source: 'entry', target: 'writer' }],
  viewport: { x: 0, y: 0, zoom: 1 },
}

describe('WorkflowRunDetailPage loop rounds', () => {
  /** Two rounds: the first continued, the second left through the early exit. */
  function loopRun(): WorkflowRun {
    const ok = { status: 'succeeded' }
    return runFixture({
      definitionSnapshot: LOOP_SNAPSHOT,
      nodeStates: {
        'loop-1': {
          status: 'succeeded',
          loop: {
            round: 2,
            maxIterations: 5,
            carried: {},
            completed: [
              { index: 1, carried: {}, states: { entry: ok, writer: { ...ok, output: '初稿' } } },
              {
                index: 2,
                carried: {},
                states: { entry: ok, writer: { ...ok, output: '终稿' }, exit: ok },
              },
            ],
          },
        },
        entry: ok,
        writer: { ...ok, output: '终稿' },
        exit: ok,
      },
    })
  }

  it('summarizes the loop on its frame and paints its members from the latest round', async () => {
    renderRun(() => Promise.resolve(HttpResponse.json(loopRun())))

    await waitFor(() => {
      expect(screen.getByText('第 2 / 5 轮 · 早退')).toBeInTheDocument()
    })
    expect(runCard('exit')).toHaveAttribute('aria-label', 'exit: 成功')
  })

  it('switches every member to the round the reader picks, each round listed once', async () => {
    renderRun(() => Promise.resolve(HttpResponse.json(loopRun())))
    const frame = await waitFor(() => {
      const element = document.querySelector('[data-workflow-loop-frame]')
      if (!(element instanceof HTMLElement)) {
        throw new Error('no loop frame yet')
      }
      return element
    })

    fireEvent.click(frame)
    const rounds = await screen.findByRole('region', { name: '轮次' })
    const buttons = within(rounds).getAllByRole('button')
    expect(buttons.map((button) => button.textContent)).toEqual([
      '第 1 轮进入下一轮',
      '第 2 轮早退',
    ])

    fireEvent.click(within(rounds).getByRole('button', { name: /第 1 轮/ }))

    expect(runCard('exit')).toHaveAttribute('aria-label', 'exit: 未到达')
    expect(within(rounds).getByText('初稿')).toBeInTheDocument()
  })
})
