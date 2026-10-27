import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { WorkflowRun } from '@/api/generated.schemas'
import { WORKFLOW_RUN_PAGE_SIZE, WORKFLOW_RUN_POLL_MS } from '@/features/workflows/api'
import { WorkflowRunsPage } from '@/features/workflows/workflow-runs-page'
import {
  installCloudSpaceHandlers,
  installWorkflowHandlers,
  TEST_TENANT_ID,
  workflowFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderAtRoute } from '@/test/render'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
const WORKSPACE_SLUG = 'cloud-dev'
const WORKFLOW_URL = `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}`
const RUNS_URL = `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs`
const RUNS_PATH = `/w/${WORKSPACE_SLUG}/workflows/${WORKFLOW_ID}/runs`

/** A run row the history endpoints return; ids stay distinct so a repeat would be visible. */
function runFixture(index: number, overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return {
    id: `run-${String(index).padStart(3, '0')}`,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    snapshotId: 'snap-1',
    name: `第 ${index} 次运行`,
    workflowName: '安全审查',
    status: 'succeeded',
    input: {},
    nodeStates: {},
    rounds: [],
    error: '',
    definitionSnapshot: null,
    startedAt: '2026-09-20T02:00:00.000Z',
    finishedAt: '2026-09-20T02:00:10.000Z',
    cancelRequestedAt: null,
    pluginVersion: '1.0.0',
    createdAt: '2026-09-20T02:00:00.000Z',
    updatedAt: '2026-09-20T02:00:10.000Z',
    ...overrides,
  }
}

/**
 * Serves a newest-first history of `total` runs, `limit` rows at a time, using the row id as the
 * cursor exactly like the backend's opaque UUID cursor. `after` is exclusive, so a page boundary
 * that repeated or skipped a row would show up as a duplicate or a missing name on screen.
 */
function installHistory(total: number, onCursor?: (after: string) => void): void {
  const runs = Array.from({ length: total }, (_, index) => runFixture(index + 1))
  server.use(
    http.get(RUNS_URL, ({ request }) => {
      const params = new URL(request.url).searchParams
      const after = params.get('after') ?? ''
      onCursor?.(after)
      const limit = Number(params.get('limit') ?? '50')
      const start = after === '' ? 0 : runs.findIndex((run) => run.id === after) + 1
      const page = runs.slice(start, start + limit)
      const more = start + limit < runs.length
      return HttpResponse.json({ items: page, nextCursor: more ? page[page.length - 1]?.id : '' })
    }),
  )
}

/**
 * Serves a two-row head page and answers every later page with `code` at `status`.
 *
 * The two footer failures differ only in what the later page refused with, so the shape of the read
 * stays in one place: a test says how the walk fails, not how the walk is built.
 */
function installHeadThenRefusal(status: number, code: string): void {
  server.use(
    http.get(RUNS_URL, ({ request }) => {
      const after = new URL(request.url).searchParams.get('after') ?? ''
      if (after === '') {
        return HttpResponse.json({
          items: [runFixture(1), runFixture(2)],
          nextCursor: 'run-002',
        })
      }
      return HttpResponse.json({ code }, { status })
    }),
  )
}

/** Mounts the runs tab behind a real route match, so the workspace slug comes from the URL. */
function renderRunsPage(configureReads?: () => void) {
  installCloudSpaceHandlers('admin')
  installWorkflowHandlers([workflowFixture({ id: WORKFLOW_ID })])
  configureReads?.()
  return renderAtRoute(
    '/w/:workspaceSlug/workflows/:workflowId/runs',
    <WorkflowRunsPage slug={TEST_TENANT_ID} />,
    RUNS_PATH,
  )
}

/** The run names currently rendered, in display order. */
function renderedRunNames(): string[] {
  return screen
    .getAllByRole('listitem')
    .map((row) => within(row).getByRole('link').getAttribute('aria-label') ?? '')
}

/**
 * Presses "load older" until the list holds `total` rows.
 *
 * Recursive rather than a loop because each step has to wait for the page to render before the
 * next click: awaiting inside a loop would race the render it just triggered.
 */
async function loadEveryPage(total: number): Promise<void> {
  const loaded = screen.getAllByRole('listitem').length
  if (loaded >= total) return
  fireEvent.click(screen.getByRole('button', { name: '加载更早的运行' }))
  await waitFor(() => expect(screen.getAllByRole('listitem').length).toBeGreaterThan(loaded))
  await loadEveryPage(total)
}

describe('WorkflowRunsPage', () => {
  it('reports denied workflow and history reads as forbidden, not missing', async () => {
    renderRunsPage(() =>
      server.use(
        http.get(WORKFLOW_URL, () => HttpResponse.json({ code: 'forbidden' }, { status: 403 })),
        http.get(RUNS_URL, () => HttpResponse.json({ code: 'forbidden' }, { status: 403 })),
      ),
    )

    expect(await screen.findByText('没有查看运行记录的权限')).toBeInTheDocument()
    expect(screen.queryByText('未找到该工作流。')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
  })

  it('retries a failed workflow read and restores the history without starting a run', async () => {
    let detailReads = 0
    const historyReads: string[] = []
    installHistory(1, (cursor) => historyReads.push(cursor))
    renderRunsPage(() =>
      server.use(
        http.get(WORKFLOW_URL, () => {
          detailReads += 1
          return detailReads === 1
            ? HttpResponse.json({ code: 'internal' }, { status: 500 })
            : HttpResponse.json(workflowFixture({ id: WORKFLOW_ID }))
        }),
      ),
    )

    expect(await screen.findByText('运行记录读取失败')).toBeInTheDocument()
    expect(screen.queryByText('未找到该工作流。')).not.toBeInTheDocument()
    await waitFor(() => expect(historyReads).toEqual(['']))
    fireEvent.click(screen.getByRole('button', { name: '重试' }))

    expect(await screen.findByText('第 1 次运行')).toBeInTheDocument()
    expect(detailReads).toBe(2)
    expect(historyReads).toEqual([''])
    expect(screen.queryByText('运行记录读取失败')).not.toBeInTheDocument()
  })

  it('reports a genuinely missing workflow without offering a read retry', async () => {
    installHistory(0)
    renderRunsPage(() =>
      server.use(
        http.get(WORKFLOW_URL, () => HttpResponse.json({ code: 'not_found' }, { status: 404 })),
      ),
    )

    expect(await screen.findByText('未找到该工作流。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '重试' })).not.toBeInTheDocument()
    expect(screen.queryByText('运行记录读取失败')).not.toBeInTheDocument()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('lists the newest run first and links into its detail page', async () => {
    installHistory(3)
    renderRunsPage()

    await screen.findByText('第 1 次运行')
    expect(renderedRunNames()).toEqual([
      '打开运行 第 1 次运行',
      '打开运行 第 2 次运行',
      '打开运行 第 3 次运行',
    ])
    const firstRow = screen.getAllByRole('listitem')[0]
    expect(firstRow === undefined ? null : within(firstRow).getByRole('link')).toHaveAttribute(
      'href',
      `${RUNS_PATH}/run-001`,
    )
  })

  it('says the workflow has no runs instead of showing an empty list', async () => {
    installHistory(0)
    renderRunsPage()

    expect(await screen.findByText('还没有运行记录')).toBeInTheDocument()
    expect(screen.queryByRole('listitem')).not.toBeInTheDocument()
  })

  it('offers a retry when the history cannot be read, rather than calling it empty', async () => {
    server.use(http.get(RUNS_URL, () => HttpResponse.json({ code: 'internal' }, { status: 500 })))
    renderRunsPage()

    expect(await screen.findByText('运行记录读取失败')).toBeInTheDocument()
    expect(screen.queryByText('还没有运行记录')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '重试' })).toBeInTheDocument()
  })

  it('reports a missing permission as its own state', async () => {
    server.use(http.get(RUNS_URL, () => HttpResponse.json({ code: 'forbidden' }, { status: 403 })))
    renderRunsPage()

    expect(await screen.findByText('没有查看运行记录的权限')).toBeInTheDocument()
    expect(screen.queryByText('运行记录读取失败')).not.toBeInTheDocument()
  })

  it('loads every page past the first hundred runs without repeating or skipping one', async () => {
    // The old surface asked for one page of 100 and stopped; a history longer than that has to
    // stay fully reachable, so this walks the whole thing the way a member would.
    const total = 120
    const cursors: string[] = []
    installHistory(total, (after) => cursors.push(after))
    renderRunsPage()

    await screen.findByText('第 1 次运行')
    expect(screen.getAllByRole('listitem')).toHaveLength(WORKFLOW_RUN_PAGE_SIZE)

    await loadEveryPage(total)

    expect(renderedRunNames()).toHaveLength(total)
    expect(new Set(renderedRunNames()).size).toBe(total)
    expect(renderedRunNames()[0]).toBe('打开运行 第 1 次运行')
    expect(renderedRunNames()[total - 1]).toBe(`打开运行 第 ${total} 次运行`)
    // Each request continued below the row the previous page ended on: the first asks for the head,
    // every later one carries a cursor, and no cursor is repeated.
    expect(cursors[0]).toBe('')
    expect(new Set(cursors.slice(1)).size).toBe(cursors.length - 1)
    expect(await screen.findByText('已经到最早的一次运行。')).toBeInTheDocument()
  })

  it('keeps the rows it already loaded when a later page fails', async () => {
    // The head answers with two rows and a cursor; every later page fails. The loaded history
    // must survive, and the failure has to read as a failed read rather than as the end of it.
    installHeadThenRefusal(500, 'internal')
    renderRunsPage()
    await screen.findByText('第 1 次运行')

    fireEvent.click(screen.getByRole('button', { name: '加载更早的运行' }))

    expect(await screen.findByText('更早的运行读取失败。')).toBeInTheDocument()
    expect(screen.getByText('第 1 次运行')).toBeInTheDocument()
    expect(screen.getByText('第 2 次运行')).toBeInTheDocument()
    // The control that failed is still there, so the retry is the same button.
    expect(screen.getByRole('button', { name: '加载更早的运行' })).toBeInTheDocument()
  })

  it('reports a permission loss on a later page as a permission state, not a paging error', async () => {
    // Losing the role mid-walk is not a transient read failure: the rest of the history is now
    // refused, and telling the member it "could not be loaded" would invite a pointless retry.
    installHeadThenRefusal(403, 'forbidden')
    renderRunsPage()
    await screen.findByText('第 1 次运行')

    fireEvent.click(screen.getByRole('button', { name: '加载更早的运行' }))

    expect(await screen.findByText('没有查看运行记录的权限')).toBeInTheDocument()
    expect(screen.queryByText('更早的运行读取失败。')).not.toBeInTheDocument()
    // The rows already read stay: they are still this member's history.
    expect(screen.getByText('第 1 次运行')).toBeInTheDocument()
    expect(screen.getByText('第 2 次运行')).toBeInTheDocument()
  })

  it('offers a retry when a refresh fails after the walk reached its end', async () => {
    // One page ends the walk, so there is no "load older" control to double as the retry. A failed
    // refresh must still be reported and retryable; calling it the end of the history would say
    // "this list is complete" about a list the page just failed to read.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let reads = 0
    server.use(
      http.get(RUNS_URL, () => {
        reads += 1
        if (reads === 1) {
          return HttpResponse.json({
            items: [runFixture(1, { status: 'running', finishedAt: null })],
            nextCursor: '',
          })
        }
        return HttpResponse.json({ code: 'internal' }, { status: 500 })
      }),
    )
    renderRunsPage()
    await screen.findByText('第 1 次运行')

    // The running row keeps the history polling, and the next read fails.
    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))

    expect(await screen.findByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByText('已经到最早的一次运行。')).not.toBeInTheDocument()
    expect(screen.getByText('第 1 次运行')).toBeInTheDocument()
  })

  it('retries the head after its refresh fails before the last page', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let headReads = 0
    let recovered = false
    server.use(
      http.get(RUNS_URL, ({ request }) => {
        if (new URL(request.url).searchParams.has('after')) {
          return HttpResponse.json({ items: [runFixture(2)], nextCursor: '' })
        }
        headReads += 1
        if (headReads > 1 && !recovered) {
          return HttpResponse.json({ code: 'internal' }, { status: 500 })
        }
        return HttpResponse.json({
          items: [
            runFixture(1, {
              status: recovered ? 'succeeded' : 'running',
              finishedAt: recovered ? '2026-09-20T02:00:10.000Z' : null,
            }),
          ],
          nextCursor: 'run-001',
        })
      }),
    )
    renderRunsPage()
    await screen.findByText('第 1 次运行')

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))
    expect(await screen.findByRole('button', { name: '重试' })).toBeInTheDocument()
    expect(screen.queryByText('更早的运行读取失败。')).not.toBeInTheDocument()
    expect(headReads).toBe(2)

    recovered = true
    fireEvent.click(screen.getByRole('button', { name: '重试' }))
    await waitFor(() => expect(headReads).toBe(3))
    expect(screen.getByText('第 1 次运行')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '加载更早的运行' })).toBeInTheDocument()
  })

  it('shows when each run started and finished, and marks one that has not started', async () => {
    server.use(
      http.get(RUNS_URL, () =>
        HttpResponse.json({
          items: [
            runFixture(1),
            runFixture(2, { status: 'pending', startedAt: null, finishedAt: null }),
          ],
          nextCursor: '',
        }),
      ),
    )
    renderRunsPage()
    await screen.findByText('第 1 次运行')

    // A finished run names both ends of its span; the instant it was created is not the answer to
    // "how long did this take", so the row carries the start and the finish.
    expect(screen.getAllByText(/^开始 /)).toHaveLength(1)
    expect(screen.getAllByText(/^结束 /)).toHaveLength(1)
    // A run that has not been picked up yet is stated as such, with its creation time, rather than
    // looking like one that started at that instant.
    expect(screen.getByText(/^未开始（创建于 .*）$/)).toBeInTheDocument()
    expect(screen.queryByText('尚未结束')).not.toBeInTheDocument()
  })

  it('shows the finish time when a run was cancelled before it started', async () => {
    server.use(
      http.get(RUNS_URL, () =>
        HttpResponse.json({
          items: [runFixture(1, { status: 'cancelled', startedAt: null })],
          nextCursor: '',
        }),
      ),
    )
    renderRunsPage()

    await screen.findByText('第 1 次运行')
    expect(screen.getByText(/^未开始（创建于 /)).toBeInTheDocument()
    expect(screen.getByText(/^结束 /)).toBeInTheDocument()
  })
})
