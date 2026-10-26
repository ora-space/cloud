import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { useState } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import { renderAtRoute, renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { installSignedInSession } from '@/test/cloud-handlers'
import type {
  CollaborationTargetSummary,
  IssueInteraction,
  TimelineEntry,
} from '@/features/issues/types'
import { ActivityPanel } from './activity-panel'

const targets: CollaborationTargetSummary[] = [
  {
    type: 'user',
    id: 'u1',
    displayName: 'Alice',
    description: '',
    interactionDescriptor: { mode: 'mention', requiresTask: false },
  },
  {
    type: 'agent',
    id: 'ag1',
    displayName: 'Backend Agent',
    description: 'demo agent',
    interactionDescriptor: { mode: 'task', requiresTask: true },
  },
  {
    type: 'workflow',
    id: 'wf1',
    displayName: 'Security Review Workflow',
    description: 'form mode',
    interactionDescriptor: { mode: 'form', requiresTask: false, formRef: 'security-review' },
  },
]

function comment(over: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    kind: 'comment',
    id: 'c1',
    seq: 1,
    createdAt: '2026-01-01T00:00:00.000Z',
    authorType: 'user',
    authorId: 'u1',
    authorUserId: 'u1',
    body: 'please look at this',
    parentId: null,
    action: null,
    details: null,
    ...over,
  }
}

function activity(action: string, over: Partial<TimelineEntry> = {}): TimelineEntry {
  return {
    kind: 'activity',
    id: `a-${action}`,
    seq: 2,
    createdAt: '2026-01-01T00:00:00.000Z',
    authorType: 'agent',
    authorId: 'ag1',
    authorUserId: null,
    body: null,
    parentId: null,
    action,
    details: { runId: 'r1', executorType: 'agent' },
    ...over,
  }
}

/** A persisted interaction row as the spine returns it. */
function interaction(over: Partial<IssueInteraction> = {}): IssueInteraction {
  return {
    id: 'ix1',
    tenantId: 't1',
    issueId: 'i1',
    commentId: 'c1',
    targetType: 'workflow',
    targetId: 'wf1',
    mode: 'form',
    task: '',
    runId: null,
    input: {},
    createdAt: '2026-01-01T00:00:00.000Z',
    ...over,
  }
}

/** Serves every endpoint an ActivityPanel mount needs; returns a spy on comment creation. */
function servePanel(items: TimelineEntry[] = [], interactions: IssueInteraction[] = []) {
  installSignedInSession()
  const posted = vi.fn<(body: unknown) => void>()
  server.use(
    http.get('/api/v1/me/spaces', () => HttpResponse.json({ items: [], nextCursor: '' })),
    http.get('/api/v1/tenants/t1/issues/i1/timeline', () =>
      HttpResponse.json({ items, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/interactions', () =>
      HttpResponse.json({ items: interactions, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/runs', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/collaboration/targets', () =>
      HttpResponse.json({ items: targets, nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/collaboration/forms/security-review', () =>
      HttpResponse.json({
        formRef: 'security-review',
        title: 'Security Review',
        fields: [{ key: 'repository', label: 'Repository', type: 'text', required: true }],
      }),
    ),
    http.post('/api/v1/tenants/t1/issues/i1/comments', async ({ request }) => {
      posted(await request.json())
      return HttpResponse.json({ resource: { id: 'c-new' } })
    }),
  )
  return { posted }
}

/** Opens the `@` picker and returns the suggestion button whose name matches. */
async function openPicker(user: ReturnType<typeof userEvent.setup>, name: RegExp) {
  await user.click(await screen.findByRole('button', { name: /提及/ }))
  return screen.findByRole('button', { name })
}

async function pickTarget(user: ReturnType<typeof userEvent.setup>, name: RegExp) {
  await user.click(await openPicker(user, name))
}

function SwitchIssue() {
  const [id, setId] = useState('i1')
  const cache = useQueryClient()
  function switchIssue() {
    for (const collection of ['timeline', 'interactions', 'runs']) {
      cache.setQueryData([`issue-${collection}`, 't1', 'i2'], [])
    }
    setId(id === 'i1' ? 'i2' : 'i1')
  }
  return (
    <>
      <button onClick={switchIssue}>Switch issue</button>
      <ActivityPanel slug="t1" issueId={id} members={[]} />
    </>
  )
}

describe('ActivityPanel', () => {
  it('keeps an ordinary comment draft visible when submission fails', async () => {
    servePanel()
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', () =>
        HttpResponse.json({ code: 'unavailable', params: {}, requestId: 'r' }, { status: 503 }),
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)
    await user.type(await screen.findByPlaceholderText('添加评论…'), 'Keep this comment')
    await user.click(screen.getByRole('button', { name: '评论' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('提交失败')
    expect(screen.getByPlaceholderText('添加评论…')).toHaveValue('Keep this comment')
  })

  it('never carries comment or staged Agent drafts into a cached different issue', async () => {
    const { posted } = servePanel()
    server.use(
      http.get('/api/v1/tenants/t1/issues/i2/:collection', () =>
        HttpResponse.json({ items: [], nextCursor: '' }),
      ),
    )
    const user = userEvent.setup()
    renderWithProviders(<SwitchIssue />)
    await user.type(await screen.findByPlaceholderText('添加评论…'), 'Private comment')
    await pickTarget(user, /Backend Agent/)
    await user.type(screen.getByLabelText('Backend Agent 的留言'), 'Private task')
    await user.click(screen.getByRole('button', { name: 'Switch issue' }))
    expect(screen.getByPlaceholderText('添加评论…')).toHaveValue('')
    expect(screen.queryByLabelText('Backend Agent 的留言')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Switch issue' }))
    expect(screen.getByPlaceholderText('添加评论…')).toHaveValue('')
    expect(screen.queryByLabelText('Backend Agent 的留言')).not.toBeInTheDocument()
    expect(posted).not.toHaveBeenCalled()
  })
  it('renders the merged timeline from the real endpoint', async () => {
    servePanel([comment(), activity('run.enqueued')])
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    expect(await screen.findByText('please look at this')).toBeInTheDocument()
    expect(screen.getByText('排队执行')).toBeInTheDocument()
  })

  it('offers Workflow targets as a selectable Form Mode option', async () => {
    servePanel()
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    const workflow = await openPicker(user, /Security Review Workflow/)
    expect(workflow).toBeEnabled()
    expect(within(workflow).getByText('表单')).toBeInTheDocument()
  })

  it('stages a mention with its own message box and posts nothing until 提交', async () => {
    const { posted } = servePanel()
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    await pickTarget(user, /Alice/)

    const box = await screen.findByLabelText('Alice 的留言')
    expect(box).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '提交' })).toBeDisabled()
    // Merely selecting a target records nothing.
    expect(posted).not.toHaveBeenCalled()

    await user.type(box, '帮忙看一下')
    await user.click(screen.getByRole('button', { name: '提交' }))

    await waitFor(() =>
      expect(posted).toHaveBeenCalledWith({
        body: '帮忙看一下',
        targets: [{ type: 'user', id: 'u1' }],
      }),
    )
  })

  it('carries the typed text as the task for an agent target', async () => {
    const { posted } = servePanel()
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    await pickTarget(user, /Backend Agent/)
    await user.type(await screen.findByLabelText('Backend Agent 的留言'), '实现 X')
    await user.click(screen.getByRole('button', { name: '提交' }))

    await waitFor(() =>
      expect(posted).toHaveBeenCalledWith({
        body: '实现 X',
        targets: [{ type: 'agent', id: 'ag1', task: '实现 X' }],
      }),
    )
  })

  it('opens the workflow form as a draft and only writes on 确认执行', async () => {
    const { posted } = servePanel()
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    await pickTarget(user, /Security Review Workflow/)

    // The descriptor drives the form, and nothing has been written yet.
    expect(await screen.findByText('Security Review')).toBeInTheDocument()
    expect(screen.getByLabelText(/Repository/)).toBeInTheDocument()
    expect(posted).not.toHaveBeenCalled()
  })

  it('never offers a second confirm for an already confirmed interaction', async () => {
    servePanel([], [interaction({ runId: 'r1' })])
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    expect(await screen.findByText(/已确认/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '审阅' })).not.toBeInTheDocument()
  })

  it('reads WorkflowRun status and links its identity within the current workspace', async () => {
    servePanel([], [interaction({ runId: 'r1' })])
    server.use(
      http.get('/api/v1/tenants/t1/issues/i1/runs', () =>
        HttpResponse.json({
          items: [
            {
              id: 'r1',
              executorId: 'wf-executor',
              executorType: 'workflow',
              status: 'queued',
              workflowInvocationRef: 'workflow-run-1',
            },
          ],
          nextCursor: '',
        }),
      ),
      http.get('/api/v1/tenants/t1/workflows/wf-executor/runs/workflow-run-1', () =>
        HttpResponse.json({ id: 'workflow-run-1', status: 'succeeded' }),
      ),
    )
    renderAtRoute(
      '/w/:workspaceSlug/issues/:issueId',
      <ActivityPanel slug="t1" issueId="i1" members={[]} />,
      '/w/team-space/issues/i1',
    )

    expect(await screen.findByText('已确认 · 运行 succeeded')).toBeInTheDocument()
    expect(screen.queryByText(/运行 queued/)).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: '运行详情' })).toHaveAttribute(
      'href',
      '/w/team-space/workflows/wf-executor/runs/workflow-run-1',
    )
  })

  it.each([null, undefined])(
    'keeps historical IssueRun status without a detail link when the workflow reference is %s',
    async (workflowInvocationRef) => {
      servePanel([], [interaction({ runId: 'legacy-run' })])
      const workflowRequest = vi.fn<() => void>()
      server.use(
        http.get('/api/v1/tenants/t1/issues/i1/runs', () =>
          HttpResponse.json({
            items: [
              {
                id: 'legacy-run',
                executorId: 'wf1',
                executorType: 'workflow',
                status: 'failed',
                workflowInvocationRef,
              },
            ],
            nextCursor: '',
          }),
        ),
        http.get('/api/v1/tenants/t1/workflows/wf1/runs/:runId', () => {
          workflowRequest()
          return HttpResponse.json({ status: 'succeeded' })
        }),
      )
      renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

      expect(await screen.findByText('已确认 · 运行 failed')).toBeInTheDocument()
      expect(screen.queryByRole('link', { name: '运行详情' })).not.toBeInTheDocument()
      expect(workflowRequest).not.toHaveBeenCalled()
    },
  )

  it('renders a workflow progress/message activity with its human-readable detail boxed', async () => {
    servePanel([
      comment(),
      activity('run.message', {
        seq: 3,
        authorType: 'system',
        details: { runId: 'r1', executorType: 'workflow', message: 'Demo workflow result.' },
      }),
    ])
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    expect(await screen.findByText('执行结果')).toBeInTheDocument()
    const detail = screen.getByText('Demo workflow result.')
    expect(detail.className).toContain('border')
  })

  it('lays the column out chronologically: history, then the comment box, then the staged rows', async () => {
    servePanel([comment()])
    const user = userEvent.setup()
    renderWithProviders(<ActivityPanel slug="t1" issueId="i1" members={[]} />)

    const history = await screen.findByText('please look at this')
    const commentBox = screen.getByPlaceholderText('添加评论…')

    // Node.DOCUMENT_POSITION_FOLLOWING means the second node comes after the first in the document.
    const AFTER = Node.DOCUMENT_POSITION_FOLLOWING
    expect(history.compareDocumentPosition(commentBox) & AFTER).toBeTruthy()

    await pickTarget(user, /Security Review Workflow/)
    const form = await screen.findByText('Security Review')
    expect(commentBox.compareDocumentPosition(form) & AFTER).toBeTruthy()
  })
})
