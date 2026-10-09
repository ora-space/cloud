import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { SidebarProvider } from '@/components/ui/sidebar'
import { makeIssue, makeStatus } from '@/test/issue-fixtures'
import { server } from '@/test/msw-server'
import type { Issue, IssueRun } from './types'
import { IssueDetailPage } from './issue-detail-page'

const statuses = [makeStatus('backlog'), makeStatus('todo'), makeStatus('done')]

/** Serves the issue plus the tenant/queries the detail page and its panels mount. */
function serveDetail(issue: Issue, allIssues: Issue[] = [issue], runs: IssueRun[] = []) {
  server.use(
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

function renderIssueDetail(issueId: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      { path: '/w/:workspaceSlug/issues', element: <div>Issues list screen</div> },
      { path: '/w/:workspaceSlug/issues/:issueId', element: <IssueDetailPage slug="t1" /> },
    ],
    { initialEntries: [`/w/t1/issues/${issueId}`] },
  )
  return render(
    <QueryClientProvider client={queryClient}>
      <SidebarProvider>
        <RouterProvider router={router} />
      </SidebarProvider>
    </QueryClientProvider>,
  )
}

describe('IssueDetailPage', () => {
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
  })

  it('has no Agent 会话 column without an agent run', async () => {
    serveDetail(makeIssue('i1', 'Fix the login'))
    renderIssueDetail('i1')
    await screen.findByText('Fix the login')
    await screen.findByText('暂无子任务')

    expect(screen.queryByRole('heading', { name: 'Agent 会话' })).not.toBeInTheDocument()
  })
})
