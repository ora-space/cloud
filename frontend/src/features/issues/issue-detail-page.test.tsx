import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { createMemoryRouter, RouterProvider, useParams } from 'react-router-dom'
import { describe, expect, it, onTestFinished } from 'vitest'
import { SidebarProvider } from '@/components/ui/sidebar'
import { makeAgentTarget, makeIssue, makeProject, makeStatus } from '@/test/issue-fixtures'
import { installSignedInSession } from '@/test/cloud-handlers'
import { SessionProvider } from '@/features/auth/session'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { server } from '@/test/msw-server'
import type { Issue, IssueRun } from './types'
import { IssueDetailPage } from './issue-detail-page'

const statuses = [makeStatus('backlog'), makeStatus('todo'), makeStatus('done')]

/** Serves the issue plus the tenant/queries the detail page and its panels mount. */
function serveDetail(issue: Issue, allIssues: Issue[] = [issue], runs: IssueRun[] = []) {
  installSignedInSession()
  server.use(
    http.get('/api/v1/me/spaces', () =>
      HttpResponse.json({
        items: [
          {
            id: 's1',
            tenantId: 't1',
            slug: 'real-workspace',
            name: 'Real workspace',
            role: 'admin',
          },
          { id: 's2', tenantId: 't1', slug: 'other-space', name: 'Other space', role: 'admin' },
        ],
        nextCursor: '',
      }),
    ),
    http.get('/api/v1/tenants/t1/spaces/:sid/projects', ({ params }) =>
      HttpResponse.json({
        items: [makeProject('p1', { spaceId: String(params['sid']) })],
        nextCursor: '',
      }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1', () => HttpResponse.json(issue)),
    http.get('/api/v1/tenants/t1/issues', () =>
      HttpResponse.json({ items: allIssues, nextCursor: '' }),
    ),
    http.put('/api/v1/tenants/t1/issues/i1', async ({ request }) => {
      const body = await request.json()
      const updates: Record<string, unknown> = {}
      if (typeof body === 'object' && body !== null) {
        for (const [key, value] of Object.entries(body)) updates[key] = value
      }
      return HttpResponse.json({ ...issue, ...updates })
    }),
    http.get('/api/v1/tenants/t1/issue-statuses', () =>
      HttpResponse.json({ items: statuses, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/members', () => HttpResponse.json({ items: [], nextCursor: '' })),
    http.get('/api/v1/tenants/t1/collaboration/targets', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/timeline', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/context-refs', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/interactions', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/runs', () =>
      HttpResponse.json({ items: runs, nextCursor: '' }),
    ),
  )
}

function DetailRoute() {
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  return (
    <CurrentSpaceProvider slug={workspaceSlug ?? ''}>
      <IssueDetailPage slug="t1" />
    </CurrentSpaceProvider>
  )
}

function renderIssueDetail(issueId: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      { path: '/w/:workspaceSlug/issues', element: <div>Issues list screen</div> },
      { path: '/w/:workspaceSlug/issues/:issueId', element: <DetailRoute /> },
    ],
    { initialEntries: [`/w/real-workspace/issues/${issueId}`] },
  )
  const rendered = render(
    <QueryClientProvider client={queryClient}>
      <SessionProvider>
        <SidebarProvider>
          <RouterProvider router={router} />
        </SidebarProvider>
      </SessionProvider>
    </QueryClientProvider>,
  )
  return { ...rendered, router, queryClient }
}

async function selectRepository(user: ReturnType<typeof userEvent.setup>) {
  const project = await screen.findByRole('combobox', { name: '项目' })
  await waitFor(() => expect(project).toBeEnabled())
  await user.click(project)
  await user.click(await screen.findByRole('option', { name: 'Repository p1' }))
  return project
}

describe('IssueDetailPage', () => {
  it.each([200, 503])(
    'keeps recovery reload exclusive and preserves the next save failure after GET %s',
    async (reloadStatus) => {
      const issue = makeIssue('i1', 'Recovery race')
      serveDetail(issue)
      let releaseReload: (() => void) | undefined
      let releaseSave: (() => void) | undefined
      const reload = new Promise<void>((resolve) => {
        releaseReload = resolve
      })
      const save = new Promise<void>((resolve) => {
        releaseSave = resolve
      })
      onTestFinished(() => {
        releaseReload?.()
        releaseSave?.()
      })
      const requests: unknown[] = []
      const fault = { code: 'version_conflict', params: {}, requestId: 'r' }
      server.use(
        http.put('/api/v1/tenants/t1/issues/i1', async ({ request }) => {
          requests.push(await request.json())
          if (requests.length === 2) await save
          return HttpResponse.json(fault, { status: 409 })
        }),
      )
      const user = userEvent.setup()
      renderIssueDetail('i1')
      const project = await selectRepository(user)
      expect(await screen.findByRole('alert')).toHaveTextContent('保存任务失败')
      let reloads = 0
      server.use(
        http.get('/api/v1/tenants/t1/issues/i1', async () => {
          reloads += 1
          await reload
          return HttpResponse.json(reloadStatus === 200 ? { ...issue, version: 4 } : fault, {
            status: reloadStatus,
          })
        }),
      )
      const reloadButton = screen.getByRole('button', { name: '重新加载任务' })
      await user.click(reloadButton)
      await waitFor(() => expect(reloads).toBe(1))
      expect(project).toBeDisabled()
      const status = screen.getAllByRole('combobox')[0]
      if (!status) throw new Error('The versioned status selector is missing')
      expect(status).toBeDisabled()
      expect(reloadButton).toBeDisabled()
      await user.click(project)
      await user.click(reloadButton)
      expect(requests).toEqual([{ projectRef: 'p1', version: 1 }])
      expect(reloads).toBe(1)
      releaseReload?.()
      await waitFor(() => expect(project).toBeEnabled())
      expect(screen.queryAllByRole('alert')).toHaveLength(reloadStatus === 200 ? 0 : 1)
      await selectRepository(user)
      await waitFor(() => expect(requests).toHaveLength(2))
      expect(requests[1]).toEqual({ projectRef: 'p1', version: reloadStatus === 200 ? 4 : 1 })
      expect(project).toBeDisabled()
      releaseSave?.()
      expect(await screen.findByRole('alert')).toHaveTextContent('保存任务失败')
      expect(project).toBeEnabled()
    },
  )
  it('does not send another versioned property update while project saving is pending', async () => {
    const issue = makeIssue('i1', 'Pending association')
    serveDetail(issue)
    let release: (() => void) | undefined
    const accepted = new Promise<void>((resolve) => {
      release = resolve
    })
    onTestFinished(() => release?.())
    let writes = 0
    server.use(
      http.put('/api/v1/tenants/t1/issues/i1', async () => {
        writes += 1
        await accepted
        return HttpResponse.json({ ...issue, projectRef: 'p1', version: 2 })
      }),
    )
    const user = userEvent.setup()
    renderIssueDetail('i1')
    const project = await selectRepository(user)
    await waitFor(() => expect(project).toBeDisabled())
    const status = screen.getAllByRole('combobox')[0]
    if (!status) throw new Error('The versioned status selector is missing')
    expect(status).toBeDisabled()
    await user.click(status)
    expect(screen.queryByRole('option')).not.toBeInTheDocument()
    await user.click(project)
    expect(writes).toBe(1)
    release?.()
    await waitFor(() => expect(project).toHaveTextContent('Repository p1'))
    expect(project).toBeEnabled()
  })
  it('associates an existing task using its current version and can clear that association', async () => {
    const issue = makeIssue('i1', 'Existing task')
    serveDetail(issue)
    const requests: unknown[] = []
    server.use(
      http.put('/api/v1/tenants/t1/issues/i1', async ({ request }) => {
        const body = await request.json()
        requests.push(body)
        if (typeof body === 'object' && body !== null && 'projectRef' in body) {
          issue.projectRef = String(body['projectRef']) || null
          issue.version += 1
        }
        return HttpResponse.json(issue)
      }),
    )
    const user = userEvent.setup()
    renderIssueDetail('i1')
    await selectRepository(user)
    await waitFor(() => expect(requests).toEqual([{ projectRef: 'p1', version: 1 }]))
    await waitFor(() => expect(screen.getByRole('combobox', { name: '项目' })).toBeEnabled())
    await user.click(screen.getByRole('combobox', { name: '项目' }))
    await user.click(await screen.findByRole('option', { name: '未关联项目' }))
    await waitFor(() =>
      expect(requests).toEqual([
        { projectRef: 'p1', version: 1 },
        { projectRef: '', version: 2 },
      ]),
    )
    expect(screen.getByRole('link', { name: '模型连接设置' })).toHaveAttribute(
      'href',
      '/w/real-workspace/settings/model-connections',
    )
    expect(screen.queryByText(/执行尚未接入/)).not.toBeInTheDocument()
  })

  it('keeps a stored association on a version conflict and reloads it with GET', async () => {
    const issue = makeIssue('i1', 'Concurrent task', { projectRef: 'missing-project' })
    serveDetail(issue)
    let puts = 0
    server.use(
      http.put('/api/v1/tenants/t1/issues/i1', () => {
        puts += 1
        return HttpResponse.json(
          { code: 'version_conflict', params: {}, requestId: 'r' },
          { status: 409 },
        )
      }),
    )
    const user = userEvent.setup()
    renderIssueDetail('i1')
    const project = await screen.findByRole('combobox', { name: '项目' })
    await waitFor(() => expect(project).toHaveTextContent('missing-project'))
    await user.click(project)
    await user.click(await screen.findByRole('option', { name: 'Repository p1' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('保存任务失败')
    expect(project).toHaveTextContent('missing-project')
    await user.click(screen.getByRole('button', { name: '重新加载任务' }))
    await waitFor(() =>
      expect(screen.queryByText('保存任务失败', { exact: false })).not.toBeInTheDocument(),
    )
    expect(puts).toBe(1)
  })

  it('opens the Thread created by Task Mode while an ordinary comment creates no Run', async () => {
    const issue = makeIssue('i1', 'Agent task', { projectRef: 'p1' })
    const runs: IssueRun[] = []
    serveDetail(issue, [issue], runs)
    const posted: unknown[] = []
    server.use(
      http.get('/api/v1/tenants/t1/collaboration/targets', () =>
        HttpResponse.json({ items: [makeAgentTarget('a1')], nextCursor: '' }),
      ),
      http.post('/api/v1/tenants/t1/issues/i1/comments', async ({ request }) => {
        const body = await request.json()
        posted.push(body)
        if (typeof body === 'object' && body !== null && 'targets' in body) {
          runs.push({
            id: 'r-created',
            tenantId: 't1',
            issueId: 'i1',
            executorType: 'agent',
            executorId: 'a1',
            input: {},
            status: 'running',
            createdAt: '2026-10-13T00:00:00Z',
            updatedAt: '2026-10-13T00:00:00Z',
          })
        }
        return HttpResponse.json({ resource: { id: 'c-created' } }, { status: 201 })
      }),
      http.get('/api/v1/tenants/t1/issues/i1/runs/r-created/thread', () => {
        return HttpResponse.json({
          items: [
            {
              seq: 1,
              kind: 'update',
              source: 'node',
              turnId: 'turn-1',
              record: {
                update: {
                  sessionUpdate: 'agent_message_chunk',
                  content: { type: 'text', text: 'Actual run reply' },
                },
              },
              createdAt: '2026-10-13T00:00:00Z',
            },
          ],
          nextCursor: 1,
          prevCursor: 1,
          threadState: 'active',
          idleSince: null,
          canAppend: true,
          canEnd: true,
          initiatorUserId: 'u1',
          model: null,
          failureCode: null,
        })
      }),
    )
    const user = userEvent.setup()
    renderIssueDetail('i1')
    await user.click(await screen.findByPlaceholderText('添加评论…'))
    await user.paste('A plain comment')
    await user.click(screen.getByRole('button', { name: '评论' }))
    await waitFor(() => expect(posted).toEqual([{ body: 'A plain comment' }]))
    expect(runs).toEqual([])
    await user.click(screen.getByRole('button', { name: '提及 / 任务' }))
    await user.click(await screen.findByRole('button', { name: /Installed Agent/ }))
    await user.click(screen.getByLabelText('Installed Agent 的留言'))
    await user.paste('Read the repository')
    await user.click(screen.getByRole('button', { name: '提交' }))
    expect(await screen.findByText('Actual run reply')).toBeInTheDocument()
    expect(posted[1]).toEqual({
      body: 'Read the repository',
      targets: [{ type: 'agent', id: 'a1', task: 'Read the repository' }],
    })
    expect(screen.getByLabelText('给 Agent 发送消息')).toBeEnabled()
    expect(screen.queryByLabelText('Installed Agent 的留言')).not.toBeInTheDocument()
  })
  it('renders the title and lets the status be changed', async () => {
    serveDetail(makeIssue('i1', 'Fix the login', { status: 'backlog' }))
    const user = userEvent.setup()
    renderIssueDetail('i1')

    expect(await screen.findByText('Fix the login')).toBeInTheDocument()

    const [statusTrigger] = screen.getAllByRole('combobox')
    if (!statusTrigger) throw new Error('issue detail must render a status selector')
    await user.click(statusTrigger)
    await user.click(await screen.findByRole('option', { name: '已完成' }))

    await waitFor(() => {
      expect(within(statusTrigger).getByText('已完成')).toBeInTheDocument()
    })
  })

  it('navigates back to the issues list via the breadcrumb', async () => {
    serveDetail(makeIssue('i1', 'Fix the login', { status: 'backlog' }))
    const user = userEvent.setup()
    renderIssueDetail('i1')
    await screen.findByText('Fix the login')

    await user.click(screen.getByRole('link', { name: '任务' }))

    expect(await screen.findByText('Issues list screen')).toBeInTheDocument()
  })

  it('shows the parent task and its sub-issues', async () => {
    const issue = makeIssue('i1', 'Fix the login', {
      status: 'backlog',
      parentIssueId: 'p1',
      number: 5,
    })
    const parent = makeIssue('p1', 'Epic task', { number: 1 })
    const child = makeIssue('c1', 'Sub task', { number: 6, parentIssueId: 'i1' })
    serveDetail(issue, [parent, issue, child])
    renderIssueDetail('i1')

    await screen.findByText('Fix the login')

    expect(await screen.findByText('Epic task')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Sub task/ })).toBeInTheDocument()
  })

  it('shows a placeholder when the issue has no sub-issues', async () => {
    serveDetail(makeIssue('i1', 'Fix the login', { status: 'backlog' }))
    renderIssueDetail('i1')
    await screen.findByText('Fix the login')

    expect(await screen.findByText('暂无子任务')).toBeInTheDocument()
  })

  it('shows the Agent 会话 column for an agent run whose session is not declared yet', async () => {
    const agentRun: IssueRun = {
      id: 'r1',
      tenantId: 't1',
      issueId: 'i1',
      executorType: 'agent',
      executorId: 'a1',
      input: {},
      status: 'queued',
      createdAt: '2026-10-01T00:00:00Z',
      updatedAt: '2026-10-01T00:00:00Z',
    }
    serveDetail(makeIssue('i1', 'Fix the login'), undefined, [agentRun])
    server.use(
      http.get('/api/v1/tenants/t1/issues/i1/runs/r1/thread', () =>
        HttpResponse.json({ code: 'not_found', params: {}, requestId: 'r' }, { status: 404 }),
      ),
    )
    renderIssueDetail('i1')

    expect(await screen.findByRole('heading', { name: 'Agent 会话' })).toBeInTheDocument()
    expect(await screen.findByText('等待 Agent 会话启动…')).toBeInTheDocument()
    expect(screen.queryByLabelText('给 Agent 发送消息')).not.toBeInTheDocument()
  })

  it('has no Agent 会话 column without an agent run', async () => {
    serveDetail(makeIssue('i1', 'Fix the login'))
    renderIssueDetail('i1')
    await screen.findByText('Fix the login')
    await screen.findByText('暂无子任务')

    expect(screen.queryByRole('heading', { name: 'Agent 会话' })).not.toBeInTheDocument()
  })
})
