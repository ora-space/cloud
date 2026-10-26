import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { CollaborationTargetSummary, IssueRun } from '@/features/issues/types'
import { makeAgentTarget } from '@/test/issue-fixtures'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { ConversationComposer } from './conversation-composer'
import { useConversationDraft } from './use-conversation-draft'
import { TaskConversationPanel } from './conversation-panel'

const target: CollaborationTargetSummary = {
  type: 'workflow',
  id: 'wf1',
  displayName: 'Check',
  description: '',
  interactionDescriptor: { mode: 'form', requiresTask: false, formRef: 'wf1' },
}
const descriptor = {
  formRef: 'wf1',
  title: 'Check configuration',
  fields: [
    {
      key: 'snapshotId',
      label: '运行版本',
      type: 'select',
      required: true,
      defaultValue: 'snap2',
      options: [{ value: 'snap2', label: 'v2 · Check' }],
    },
    { key: 'input', label: '检查内容', type: 'text', required: true },
  ],
}

function Composer({ onSent }: { onSent: () => void }) {
  return (
    <ConversationComposer
      tid="t1"
      issueId="i1"
      targets={[makeAgentTarget('a1'), target]}
      latestRun={undefined}
      state={useConversationDraft()}
      onSent={onSent}
      readiness={undefined}
      settingsPath={undefined}
    />
  )
}

function serveForm() {
  const mutations = vi.fn<(url: string, body: unknown) => void>()
  server.use(
    http.get('/api/v1/tenants/t1/collaboration/forms/wf1', () => HttpResponse.json(descriptor)),
    http.get('/api/v1/tenants/t1/workflows/wf1/snapshots', () =>
      HttpResponse.json({ items: [{ id: 'snap2' }], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/snap2/form', () =>
      HttpResponse.json({ ...descriptor, snapshotId: 'snap2' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/interactions', () =>
      HttpResponse.json({
        items: [
          {
            id: 'ix1',
            commentId: 'c1',
            targetType: 'workflow',
            targetId: 'wf1',
            mode: 'form',
            runId: null,
          },
        ],
        nextCursor: '',
      }),
    ),
    http.post('/api/v1/tenants/t1/issues/i1/:action', async ({ request, params }) => {
      mutations(request.url, await request.json())
      return HttpResponse.json({
        resource: { id: params['action'] === 'comments' ? 'c1' : 'unexpected' },
      })
    }),
    http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', async ({ request }) => {
      mutations(request.url, await request.json())
      return HttpResponse.json({ resource: { id: 'wr1', workflowInvocationRef: 'execution1' } })
    }),
  )
  return mutations
}

describe('Workflow in the task conversation', () => {
  it('opens a version form from @ without mutations and preserves the Agent draft on cancel', async () => {
    const mutations = serveForm()
    const user = userEvent.setup()
    renderWithProviders(<Composer onSent={vi.fn<() => void>()} />)
    await user.type(screen.getByLabelText('任务对话输入'), 'Keep this draft @Check')
    await user.click(
      within(screen.getByLabelText('选择提及的目标')).getByRole('button', {
        name: 'Check · 工作流',
      }),
    )
    await screen.findByLabelText(/检查内容/)
    expect(mutations).not.toHaveBeenCalled()
    await user.click(screen.getByRole('button', { name: '取消' }))
    expect(screen.getByLabelText('任务对话输入')).toHaveValue('Keep this draft')
    expect(mutations).not.toHaveBeenCalled()
  })

  it('uses interaction Confirm and the rendered snapshot instead of the Agent message endpoint', async () => {
    const mutations = serveForm()
    const onSent = vi.fn<() => void>()
    const user = userEvent.setup()
    renderWithProviders(<Composer onSent={onSent} />)
    await user.selectOptions(screen.getByLabelText('发送给'), 'workflow:wf1')
    await user.type(await screen.findByLabelText(/检查内容/), 'Check login')
    await user.click(screen.getByRole('button', { name: '审阅' }))
    expect(mutations).not.toHaveBeenCalled()
    await user.click(await screen.findByRole('button', { name: '确认执行' }))
    await waitFor(() => expect(onSent).toHaveBeenCalledOnce())
    expect(mutations.mock.calls.map(([url]) => new URL(url).pathname)).toEqual([
      '/api/v1/tenants/t1/issues/i1/comments',
      '/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm',
    ])
    expect(mutations.mock.calls[1]?.[1]).toEqual({
      values: { snapshotId: 'snap2', input: 'Check login' },
    })
  })

  it('opens a selected WorkflowRun as a workflow card without requesting an Agent Thread', async () => {
    const threadRead = vi.fn<() => void>()
    const run: IssueRun = {
      id: 'wr1',
      tenantId: 't1',
      issueId: 'i1',
      executorType: 'workflow',
      executorId: 'wf1',
      workflowInvocationRef: 'execution1',
      input: { workflowSnapshotId: 'snap2' },
      status: 'completed',
      createdAt: '2026-10-25T00:00:00Z',
      updatedAt: '2026-10-25T00:00:00Z',
    }
    server.use(
      http.get('/api/v1/tenants/t1/issues/i1/conversation', () =>
        HttpResponse.json({ items: [], nextCursor: null, prevCursor: null }),
      ),
      http.get('/api/v1/tenants/t1/issues/i1/timeline', () =>
        HttpResponse.json({ items: [], nextCursor: '' }),
      ),
      http.get('/api/v1/tenants/t1/issues/i1/runs/:rid/thread', () => {
        threadRead()
        return HttpResponse.json({ items: [] })
      }),
      http.get('/api/v1/tenants/t1/workflows/wf1/runs/execution1', () =>
        HttpResponse.json({
          id: 'execution1',
          name: 'Frozen Check',
          status: 'succeeded',
          snapshotId: 'snap2',
          nodeStates: {},
          input: {},
          error: '',
        }),
      ),
    )
    renderWithProviders(
      <TaskConversationPanel
        tid="t1"
        issueId="i1"
        runs={[run]}
        members={[]}
        targets={[target]}
        selectedRunId="wr1"
        workspaceSlug="demo"
        onLatest={vi.fn<() => void>()}
      />,
    )
    await screen.findByText('工作流 · Frozen Check')
    expect(screen.getByRole('link', { name: '工作流运行详情' })).toHaveAttribute(
      'href',
      '/w/demo/workflows/wf1/runs/execution1',
    )
    expect(threadRead).not.toHaveBeenCalled()
  })
})
