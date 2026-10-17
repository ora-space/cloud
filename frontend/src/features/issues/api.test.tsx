import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import type { FormDescriptor, Issue } from './types'
import {
  useCreateIssue,
  useFormDescriptor,
  useIssues,
  useMoveIssue,
  useRuns,
  useUpdateIssue,
  useWorkflowPublishedSnapshotIds,
  useWorkflowSnapshotForm,
} from './api'

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
  }
}

const issueFixture: Issue = {
  id: 'i1',
  tenantId: 't1',
  creatorUserId: 'u1',
  assigneeType: 'user',
  assigneeId: null,
  assigneeUserId: null,
  parentIssueId: null,
  projectRef: null,
  title: 'Fix the login',
  description: '',
  status: 'backlog',
  priority: 'none',
  position: 0,
  number: 1,
  properties: {},
  version: 3,
  createdAt: '',
  updatedAt: '',
  labels: [],
}

describe('useIssues', () => {
  it('unwraps the items envelope into the issue list', async () => {
    server.use(
      http.get('/api/v1/tenants/t1/issues', () =>
        HttpResponse.json({ items: [issueFixture], nextCursor: '' }),
      ),
    )
    const { result } = renderHook(() => useIssues('t1'), { wrapper: wrapper(new QueryClient()) })

    await waitFor(() => expect(result.current.data).toEqual([issueFixture]))
  })
})

describe('useRuns', () => {
  it('preserves the WorkflowRun reference for confirmed interaction navigation', async () => {
    const linkedRun = {
      id: 'r1',
      executorType: 'workflow',
      executorId: 'wf1',
      workflowInvocationRef: 'wr1',
      status: 'queued',
    }
    server.use(
      http.get('/api/v1/tenants/t1/issues/i1/runs', () =>
        HttpResponse.json({ items: [linkedRun], nextCursor: '' }),
      ),
    )
    const { result } = renderHook(() => useRuns('t1', 'i1'), {
      wrapper: wrapper(new QueryClient()),
    })

    await waitFor(() => expect(result.current.data).toEqual([linkedRun]))
  })
})

describe('useUpdateIssue', () => {
  it('sends the version alongside the patch and caches the returned issue', async () => {
    let body: unknown
    const updated = { ...issueFixture, status: 'done', version: 4 }
    server.use(
      http.put('/api/v1/tenants/t1/issues/i1', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json(updated)
      }),
    )
    const queryClient = new QueryClient()
    const { result } = renderHook(() => useUpdateIssue('t1'), {
      wrapper: wrapper(queryClient),
    })

    result.current.mutate({ id: 'i1', version: 3, patch: { status: 'done' } })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(body).toEqual({ status: 'done', version: 3 })
    expect(queryClient.getQueryData(['issue', 't1', 'i1'])).toEqual(updated)
  })
})

describe('useCreateIssue', () => {
  it.each([
    {
      kind: 'associated repository task',
      input: { title: 'Fix the login', projectRef: 'project-1' },
    },
    { kind: 'sub-issue', input: { title: 'Child task', parentIssueId: 'p1' } },
  ])(
    'sends the complete $kind request with an idempotency key and unwraps its resource',
    async ({ input }) => {
      let idempotencyKey: string | null = null
      let body: unknown
      server.use(
        http.post('/api/v1/tenants/t1/issues', async ({ request }) => {
          idempotencyKey = request.headers.get('Idempotency-Key')
          body = await request.json()
          return HttpResponse.json({ resource: issueFixture })
        }),
      )
      const { result } = renderHook(() => useCreateIssue('t1'), {
        wrapper: wrapper(new QueryClient()),
      })

      result.current.mutate(input)

      await waitFor(() => expect(result.current.isSuccess).toBe(true))
      expect(idempotencyKey).toBeTruthy()
      expect(body).toEqual(input)
      expect(result.current.data).toEqual(issueFixture)
    },
  )
})

describe('useFormDescriptor', () => {
  const descriptor: FormDescriptor = {
    formRef: 'f1',
    fields: [
      {
        key: 'repository',
        label: '仓库地址',
        type: 'text',
        required: true,
        defaultValue: 'https://github.com/ora/cloud',
      },
    ],
  }

  it('asks for the descriptor alone when no issue is given', async () => {
    let search = ''
    server.use(
      http.get('/api/v1/tenants/t1/collaboration/forms/f1', ({ request }) => {
        search = new URL(request.url).search
        return HttpResponse.json(descriptor)
      }),
    )
    const { result } = renderHook(() => useFormDescriptor('t1', 'f1'), {
      wrapper: wrapper(new QueryClient()),
    })

    await waitFor(() => expect(result.current.data).toEqual(descriptor))
    expect(search).toBe('')
  })

  it('carries the issue and caches the descriptor per issue', async () => {
    const asked: string[] = []
    server.use(
      http.get('/api/v1/tenants/t1/collaboration/forms/f1', ({ request }) => {
        asked.push(new URL(request.url).searchParams.get('issueId') ?? '')
        return HttpResponse.json(descriptor)
      }),
    )
    const queryClient = new QueryClient()
    const { result, rerender } = renderHook(
      ({ issueId }: { issueId: string }) => useFormDescriptor('t1', 'f1', issueId),
      { wrapper: wrapper(queryClient), initialProps: { issueId: 'i1' } },
    )

    await waitFor(() => expect(result.current.data).toEqual(descriptor))
    expect(asked).toEqual(['i1'])

    // A second issue is a different cache entry: the first issue's defaults were its own project's
    // repository, so reusing them would seed the wrong form.
    rerender({ issueId: 'i2' })
    await waitFor(() => expect(asked).toEqual(['i1', 'i2']))
  })
})

describe('useMoveIssue', () => {
  it('POSTs status, anchors and version to the move endpoint and caches the result', async () => {
    let body: unknown
    let idempotencyKey: string | null = null
    const moved = { ...issueFixture, status: 'done', version: 4 }
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/move', async ({ request }) => {
        idempotencyKey = request.headers.get('Idempotency-Key')
        body = await request.json()
        return HttpResponse.json(moved)
      }),
    )
    const queryClient = new QueryClient()
    const { result } = renderHook(() => useMoveIssue('t1'), { wrapper: wrapper(queryClient) })

    result.current.mutate({ id: 'i1', version: 3, move: { status: 'done', beforeId: 'b1' } })

    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(body).toEqual({ status: 'done', beforeId: 'b1', version: 3 })
    expect(idempotencyKey).toBeTruthy()
    expect(queryClient.getQueryData(['issue', 't1', 'i1'])).toEqual(moved)
  })
})

describe('useWorkflowSnapshotForm', () => {
  it.each([
    {
      scope: 'tenant',
      change: { tenant: 't2' },
      path: '/tenants/t2/workflows/wf1/snapshots/s1/form?issueId=i1',
    },
    {
      scope: 'workflow',
      change: { workflow: 'wf2' },
      path: '/tenants/t1/workflows/wf2/snapshots/s1/form?issueId=i1',
    },
    {
      scope: 'snapshot',
      change: { snapshot: 's2' },
      path: '/tenants/t1/workflows/wf1/snapshots/s2/form?issueId=i1',
    },
    {
      scope: 'issue',
      change: { issue: 'i2' },
      path: '/tenants/t1/workflows/wf1/snapshots/s1/form?issueId=i2',
    },
  ])('separates the descriptor cache when $scope changes', async ({ change, path }) => {
    const queryClient = new QueryClient()
    const requests: string[] = []
    server.use(
      http.get(
        '/api/v1/tenants/:tid/workflows/:wfid/snapshots/:snapshotId/form',
        ({ request, params }) => {
          requests.push(new URL(request.url).pathname + new URL(request.url).search)
          return HttpResponse.json({
            formRef: params['wfid'],
            snapshotId: params['snapshotId'],
            fields: [],
          })
        },
      ),
    )
    const initial = { tenant: 't1', workflow: 'wf1', snapshot: 's1', issue: 'i1' }
    const { result, rerender } = renderHook(
      (props: typeof initial) =>
        useWorkflowSnapshotForm(props.tenant, props.workflow, props.snapshot, props.issue),
      { wrapper: wrapper(queryClient), initialProps: initial },
    )
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    rerender({ ...initial, ...change })
    await waitFor(() => expect(requests).toHaveLength(2))
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(requests).toEqual([
      '/api/v1/tenants/t1/workflows/wf1/snapshots/s1/form?issueId=i1',
      `/api/v1${path}`,
    ])
    expect(queryClient.getQueryCache().getAll()).toHaveLength(2)
  })
})

describe('useWorkflowPublishedSnapshotIds', () => {
  it.each([
    { scope: 'tenant', tenant: 't2', workflow: 'wf1' },
    { scope: 'workflow', tenant: 't1', workflow: 'wf2' },
  ])('follows all pages and isolates ids when $scope changes', async ({ tenant, workflow }) => {
    const requests: { path: string; limit: string | null; cursor: string | null }[] = []
    server.use(
      http.get('/api/v1/tenants/:tid/workflows/:wfid/snapshots', ({ request, params }) => {
        const url = new URL(request.url)
        const cursor = url.searchParams.get('cursor')
        requests.push({ path: url.pathname, limit: url.searchParams.get('limit'), cursor })
        const prefix = `${String(params['tid'])}/${String(params['wfid'])}`
        return HttpResponse.json({
          items: [{ id: `${prefix}/${cursor ? 'second' : 'first'}` }],
          nextCursor: cursor ? '' : 'page-two',
        })
      }),
    )
    const cache = new QueryClient()
    const { result, rerender } = renderHook(
      (props: { tenant: string; workflow: string }) =>
        useWorkflowPublishedSnapshotIds(props.tenant, props.workflow),
      { wrapper: wrapper(cache), initialProps: { tenant: 't1', workflow: 'wf1' } },
    )
    await waitFor(() => expect(result.current.data).toEqual(['t1/wf1/first', 't1/wf1/second']))
    rerender({ tenant, workflow })
    await waitFor(() =>
      expect(result.current.data).toEqual([
        `${tenant}/${workflow}/first`,
        `${tenant}/${workflow}/second`,
      ]),
    )
    expect(requests).toEqual([
      { path: '/api/v1/tenants/t1/workflows/wf1/snapshots', limit: '100', cursor: null },
      { path: '/api/v1/tenants/t1/workflows/wf1/snapshots', limit: '100', cursor: 'page-two' },
      {
        path: `/api/v1/tenants/${tenant}/workflows/${workflow}/snapshots`,
        limit: '100',
        cursor: null,
      },
      {
        path: `/api/v1/tenants/${tenant}/workflows/${workflow}/snapshots`,
        limit: '100',
        cursor: 'page-two',
      },
    ])
    expect(
      cache
        .getQueryCache()
        .getAll()
        .map((query) => query.queryKey),
    ).toEqual([
      ['workflow-published-snapshot-ids', 't1', 'wf1'],
      ['workflow-published-snapshot-ids', tenant, workflow],
    ])
  })

  it.each([
    { tenant: '', workflow: 'wf1' },
    { tenant: 't1', workflow: undefined },
  ])('does not fetch until tenant and workflow ids are present: %j', ({ tenant, workflow }) => {
    const { result } = renderHook(() => useWorkflowPublishedSnapshotIds(tenant, workflow), {
      wrapper: wrapper(new QueryClient()),
    })
    expect(result.current.fetchStatus).toBe('idle')
    expect(result.current.data).toBeUndefined()
  })
})
