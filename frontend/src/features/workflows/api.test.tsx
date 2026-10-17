import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { GetApiV1TenantsTidWorkflowsWfidRuns200, WorkflowRun } from '@/api/generated.schemas'
import {
  WORKFLOW_RUN_POLL_MS,
  mergeRunPage,
  mergeRunRead,
  runPollInterval,
  runsPollInterval,
  useWorkflowRun,
  useWorkflowRuns,
} from '@/features/workflows/api'
import { TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { settleUntil } from '@/test/polling'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'
const RUN_ID = '55555555-5555-5555-5555-555555555555'
const RUNS_URL = `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs`
const RUN_URL = `${RUNS_URL}/${RUN_ID}`

/** A run row as the read endpoints return it; `updatedAt` orders competing reads. */
function runFixture(overrides: Partial<WorkflowRun> = {}): WorkflowRun {
  return {
    id: RUN_ID,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    snapshotId: 'snap-1',
    name: '巡检运行',
    workflowName: '安全审查',
    status: 'running',
    input: {},
    nodeStates: { 'agent-1': { status: 'running', startedAt: '2026-09-20T02:00:00.000Z' } },
    rounds: [],
    error: '',
    definitionSnapshot: null,
    startedAt: '2026-09-20T02:00:00.000Z',
    finishedAt: null,
    createdAt: '2026-09-20T02:00:00.000Z',
    updatedAt: '2026-09-20T02:00:05.000Z',
    ...overrides,
  }
}

/** The same run after it settled, with the node output the poll is meant to deliver. */
function settledRun(): WorkflowRun {
  return runFixture({
    status: 'succeeded',
    finishedAt: '2026-09-20T02:00:10.000Z',
    updatedAt: '2026-09-20T02:00:10.000Z',
    nodeStates: { 'agent-1': { status: 'succeeded', output: '审查通过' } },
  })
}

/** A run-history page as the read endpoint returns it. */
function pageFixture(items: WorkflowRun[]): GetApiV1TenantsTidWorkflowsWfidRuns200 {
  return { items, nextCursor: '' }
}

/** A fresh client per test, with retries off so a failed read surfaces at once. */
function newQueryClient(): QueryClient {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

/**
 * What the cache currently holds for each read the hooks poll. Tests read the
 * cache rather than the hook result wherever a read must be rejected: the cache
 * takes the read synchronously, while the observer hands the new value to React
 * a macrotask later, which would let a stale read look as if it never landed.
 */
function cachedRun(queryClient: QueryClient): WorkflowRun | undefined {
  return queryClient.getQueryState<WorkflowRun>([RUN_URL])?.data
}

/** The cached history page, as {@link cachedRun} reads the cached run. */
function cachedPage(queryClient: QueryClient): GetApiV1TenantsTidWorkflowsWfidRuns200 | undefined {
  return queryClient.getQueryState<GetApiV1TenantsTidWorkflowsWfidRuns200>([RUNS_URL])?.data
}

/** Mounts a hook under the query client, with no other provider it does not need. */
function wrapperFor(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  }
}

afterEach(() => {
  vi.useRealTimers()
})

describe('runPollInterval', () => {
  it('polls a run that can still change and stops once it settles', () => {
    expect(WORKFLOW_RUN_POLL_MS).toBe(5000)
    expect(runPollInterval(undefined)).toBe(false)
    for (const status of ['pending', 'running', 'awaiting_input'] as const) {
      expect(runPollInterval(status)).toBe(WORKFLOW_RUN_POLL_MS)
    }
    for (const status of ['succeeded', 'failed', 'cancelled'] as const) {
      expect(runPollInterval(status)).toBe(false)
    }
  })
})

describe('runsPollInterval', () => {
  it('polls the history while any listed run can still change', () => {
    expect(runsPollInterval(undefined)).toBe(false)
    expect(runsPollInterval([])).toBe(false)
    expect(runsPollInterval([settledRun(), runFixture()])).toBe(WORKFLOW_RUN_POLL_MS)
    expect(runsPollInterval([settledRun(), { ...settledRun(), id: 'run-2' }])).toBe(false)
  })
})

describe('mergeRunRead', () => {
  it('takes the first read as it comes', () => {
    const first = runFixture()

    expect(mergeRunRead(undefined, first)).toBe(first)
  })

  it('never reopens a settled run with a read that says it is running again', () => {
    const settled = settledRun()
    // Newer by `updatedAt`, so only the settled rule can reject it: a run that
    // reached a terminal status has no way back, whatever a read claims.
    const late = runFixture({ status: 'running', updatedAt: '2026-09-20T02:00:20.000Z' })

    expect(mergeRunRead(settled, late)).toBe(settled)
  })

  it('keeps the newer of two reads of a run that is still moving', () => {
    const held = runFixture({ status: 'pending', updatedAt: '2026-09-20T02:00:05.000Z' })
    const next = runFixture({ status: 'running', updatedAt: '2026-09-20T02:00:06.000Z' })

    expect(mergeRunRead(held, next)).toBe(next)
  })

  it('keeps the newer of two settled reads', () => {
    const held = runFixture({ status: 'failed', updatedAt: '2026-09-20T02:00:10.000Z' })
    const next = runFixture({ status: 'succeeded', updatedAt: '2026-09-20T02:00:11.000Z' })

    expect(mergeRunRead(held, next)).toBe(next)
  })

  it('keeps a node entry only the losing read carried', () => {
    const held = runFixture({
      updatedAt: '2026-09-20T02:00:05.000Z',
      nodeStates: { 'start-1': { status: 'succeeded', output: '收到输入' } },
    })
    const next = runFixture({
      updatedAt: '2026-09-20T02:00:06.000Z',
      nodeStates: { 'agent-1': { status: 'running' } },
    })

    expect(mergeRunRead(held, next)).toEqual({
      ...next,
      nodeStates: {
        'agent-1': { status: 'running' },
        'start-1': { status: 'succeeded', output: '收到输入' },
      },
    })
  })
})

describe('mergeRunPage', () => {
  it('follows the incoming page for membership and merges each run it returns', () => {
    const settled = settledRun()
    const held = pageFixture([settled, runFixture({ id: 'run-2' })])
    const next = pageFixture([
      runFixture({ status: 'running', updatedAt: '2026-09-20T02:00:00.000Z' }),
      runFixture({ id: 'run-3' }),
    ])

    const merged = mergeRunPage(held, next)

    // The server decides which runs exist: the dropped row is gone and the new
    // one is in, while the settled run keeps what the list already showed.
    expect(merged.items.map((run) => run.id)).toEqual([RUN_ID, 'run-3'])
    expect(merged.items[0]).toBe(settled)
    expect(merged.items[1]).toEqual(next.items[1])
    expect(merged.nextCursor).toBe('')
  })
})

describe('useWorkflowRun', () => {
  it('follows a run to its terminal status and then stops reading it', async () => {
    // Only the query's interval is faked: the retries below flush real timers.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let reads = 0
    server.use(
      http.get(RUN_URL, () => {
        reads += 1
        return HttpResponse.json(reads === 1 ? runFixture() : settledRun())
      }),
    )
    const { result } = renderHook(() => useWorkflowRun(TEST_TENANT_ID, WORKFLOW_ID, RUN_ID), {
      wrapper: wrapperFor(newQueryClient()),
    })

    await settleUntil(() => expect(result.current.data?.status).toBe('running'))
    expect(reads).toBe(1)

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))

    await settleUntil(() => expect(result.current.data?.status).toBe('succeeded'))
    expect(reads).toBe(2)
    expect(result.current.data?.nodeStates).toEqual({
      'agent-1': { status: 'succeeded', output: '审查通过' },
    })

    // The terminal status cleared the interval: further ticks issue no request.
    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS * 3))
    expect(reads).toBe(2)
  })

  it('keeps reading a run whose status this build does not know', async () => {
    // A newer backend can add a status. Stopping on a value the viewer cannot
    // classify would silently freeze a run the server is still advancing.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let reads = 0
    server.use(
      http.get(RUN_URL, () => {
        reads += 1
        return HttpResponse.json({ ...runFixture(), status: 'paused' })
      }),
    )
    renderHook(() => useWorkflowRun(TEST_TENANT_ID, WORKFLOW_ID, RUN_ID), {
      wrapper: wrapperFor(newQueryClient()),
    })

    await settleUntil(() => expect(reads).toBe(1))
    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))

    await settleUntil(() => expect(reads).toBe(2))
  })

  it('leaves the cached run alone when a read returns the same thing', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const queryClient = newQueryClient()
    let reads = 0
    server.use(
      http.get(RUN_URL, () => {
        reads += 1
        return HttpResponse.json(runFixture())
      }),
    )
    renderHook(() => useWorkflowRun(TEST_TENANT_ID, WORKFLOW_ID, RUN_ID), {
      wrapper: wrapperFor(queryClient),
    })
    await settleUntil(() => expect(reads).toBe(1))
    const first = cachedRun(queryClient)

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))
    await settleUntil(() => expect(reads).toBe(2))

    // The identity is the contract: the detail page re-lays out the frozen graph
    // whenever this object changes, so an unchanged read must not replace it.
    expect(first).toBeDefined()
    expect(cachedRun(queryClient)).toBe(first)
  })

  it('keeps what it already showed when a later read is stale', async () => {
    const queryClient = newQueryClient()
    let reads = 0
    server.use(
      http.get(RUN_URL, () => {
        reads += 1
        return HttpResponse.json(reads === 2 ? settledRun() : runFixture())
      }),
    )
    const { result } = renderHook(() => useWorkflowRun(TEST_TENANT_ID, WORKFLOW_ID, RUN_ID), {
      wrapper: wrapperFor(queryClient),
    })
    await waitFor(() => expect(result.current.data?.status).toBe('running'))

    // A reconnect or an invalidation refetches the run; this one is answered by
    // a copy that has not caught up.
    await act(async () => {
      await result.current.refetch()
    })
    await waitFor(() => expect(result.current.data?.status).toBe('succeeded'))
    const settled = cachedRun(queryClient)
    await act(async () => {
      await result.current.refetch()
    })

    // The stale read was answered and then dropped: the cache still holds the
    // settled run, down to the object identity the canvas keys its layout on.
    expect(reads).toBe(3)
    expect(settled?.status).toBe('succeeded')
    expect(cachedRun(queryClient)).toBe(settled)
  })

  it('replaces a cache entry it cannot read instead of merging into it', async () => {
    // An entry this build did not produce (an older shape, a hand-written seed)
    // has nothing the merge can order, so the read replaces it.
    const queryClient = newQueryClient()
    queryClient.setQueryData([RUN_URL], { unexpected: true })
    server.use(http.get(RUN_URL, () => HttpResponse.json(runFixture())))
    const { result } = renderHook(() => useWorkflowRun(TEST_TENANT_ID, WORKFLOW_ID, RUN_ID), {
      wrapper: wrapperFor(queryClient),
    })

    await waitFor(() => expect(result.current.data?.status).toBe('running'))
  })
})

describe('useWorkflowRuns', () => {
  it('refreshes the history while a listed run can still change and stops once it settles', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let reads = 0
    server.use(
      http.get(RUNS_URL, () => {
        reads += 1
        return HttpResponse.json(pageFixture([reads === 1 ? runFixture() : settledRun()]))
      }),
    )
    const { result } = renderHook(() => useWorkflowRuns(TEST_TENANT_ID, WORKFLOW_ID), {
      wrapper: wrapperFor(newQueryClient()),
    })

    await settleUntil(() => expect(result.current.data?.[0]?.status).toBe('running'))

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))

    await settleUntil(() => expect(result.current.data?.[0]?.status).toBe('succeeded'))
    expect(reads).toBe(2)

    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS * 3))
    expect(reads).toBe(2)
  })

  it('keeps a settled history row from flipping back on a stale page', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const queryClient = newQueryClient()
    let reads = 0
    server.use(
      http.get(RUNS_URL, () => {
        reads += 1
        return HttpResponse.json(pageFixture([reads === 2 ? settledRun() : runFixture()]))
      }),
    )
    const { result } = renderHook(() => useWorkflowRuns(TEST_TENANT_ID, WORKFLOW_ID), {
      wrapper: wrapperFor(queryClient),
    })
    const firstRow = () => cachedPage(queryClient)?.items[0]

    await settleUntil(() => expect(firstRow()?.status).toBe('running'))
    await act(() => vi.advanceTimersByTimeAsync(WORKFLOW_RUN_POLL_MS))
    await settleUntil(() => expect(firstRow()?.status).toBe('succeeded'))
    const settled = firstRow()

    // Polling stopped when the row settled; an invalidation brings one more
    // read, answered by a copy that has not caught up.
    await act(async () => {
      await result.current.refetch()
    })

    // The history is a list of runs, not of reads: a row the list already
    // showed as settled keeps that status, and its identity with it.
    expect(reads).toBe(3)
    expect(settled?.status).toBe('succeeded')
    expect(firstRow()).toBe(settled)
  })
})
