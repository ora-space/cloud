import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { HttpResponse, http } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { TaskConversationMessage } from '@/api/generated.schemas'
import type { IssueRun } from '@/features/issues/types'
import { makeAgentTarget } from '@/test/issue-fixtures'
import { server } from '@/test/msw-server'
import { TaskConversationPanel } from './conversation-panel'

const RUNS = '/api/v1/tenants/t1/issues/i1/runs'
const CONVERSATION = '/api/v1/tenants/t1/issues/i1/conversation'

function run(id: string, agent: string, status: string, at: string): IssueRun {
  return {
    id,
    tenantId: 't1',
    issueId: 'i1',
    executorType: 'agent',
    executorId: agent,
    input: { task: `Task ${id}` },
    status,
    createdAt: at,
    updatedAt: at,
  }
}

function mockConversation(
  runs: IssueRun[],
  messages: TaskConversationMessage[],
  permissions: { canEnd?: boolean; initiatorUserId?: string } = {},
) {
  server.use(
    http.get(RUNS, () => HttpResponse.json({ items: runs, nextCursor: '' })),
    http.get(CONVERSATION, () =>
      HttpResponse.json({
        items: messages,
        nextCursor: messages.at(-1)?.seq ?? null,
        prevCursor: messages[0]?.seq ?? null,
        activeRunId: runs.find((candidate) => candidate.status === 'running')?.id ?? null,
      }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/timeline', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get('/api/v1/tenants/t1/issues/i1/runs/:rid/thread', ({ params }) =>
      HttpResponse.json({
        items: [
          {
            seq: 1,
            kind: 'update',
            source: 'node',
            turnId: 'turn-1',
            record: {
              update: {
                sessionUpdate: 'agent_message_chunk',
                content: { type: 'text', text: `Answer ${String(params['rid'])}` },
              },
            },
            createdAt: '2026-10-18T01:01:00Z',
          },
        ],
        threadState:
          runs.find((candidate) => candidate.id === params['rid'])?.status === 'running'
            ? 'idle'
            : 'ended',
        idleSince: null,
        canAppend: true,
        canEnd: permissions.canEnd ?? true,
        initiatorUserId: permissions.initiatorUserId ?? 'u1',
        model: null,
        nextCursor: 1,
        prevCursor: 1,
      }),
    ),
  )
}

function renderConversation(runs: IssueRun[], currentUserId = 'u1') {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const onLatest = vi.fn<() => void>()
  render(
    <QueryClientProvider client={queryClient}>
      <TaskConversationPanel
        tid="t1"
        issueId="i1"
        runs={runs}
        members={[]}
        targets={[makeAgentTarget('a1', 'OpenCode'), makeAgentTarget('a2', 'Claude')]}
        currentUserId={currentUserId}
        onLatest={onLatest}
      />
    </QueryClientProvider>,
  )
  return { queryClient, onLatest }
}

describe('TaskConversationPanel', () => {
  it('refreshes the selected blocked Agent after an external repair without losing the draft', async () => {
    mockConversation([], [])
    let repaired = false
    server.use(
      http.get(CONVERSATION, () =>
        HttpResponse.json({
          items: [],
          nextCursor: null,
          prevCursor: null,
          agentReadiness: [
            { agentId: 'a1', requiresPersonalModel: true, errorCode: 'model_default_required' },
            {
              agentId: 'a2',
              requiresPersonalModel: true,
              errorCode: repaired ? null : 'agent_configuration_required',
            },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    renderConversation([])
    await user.selectOptions(screen.getByLabelText('发送给'), 'a2')
    const input = screen.getByLabelText('任务对话输入')
    await user.type(input, 'preserve this repaired task')
    expect(
      await screen.findByText('请在插件的“我的配置”中选择个人模型连接和模型。'),
    ).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^发送$/ })).toBeDisabled()
    repaired = true
    await waitFor(() => expect(screen.getByRole('button', { name: /^发送$/ })).toBeEnabled(), {
      timeout: 7000,
    })
    expect(input).toHaveValue('preserve this repaired task')
  }, 10000)

  it('does not report older Runs as unread but does notify about a new task message', async () => {
    const runs = Array.from({ length: 6 }, (_, index) =>
      run(`r-${index}`, 'a1', 'completed', `2026-10-18T01:0${index}:00Z`),
    )
    const messages: TaskConversationMessage[] = []
    mockConversation(runs, messages)
    const { queryClient } = renderConversation(runs)
    await screen.findByText('Answer r-5')
    const viewport = screen.getByLabelText('对话滚动区域')
    Object.defineProperties(viewport, {
      clientHeight: { configurable: true, value: 300 },
      scrollHeight: { configurable: true, value: 1000 },
    })
    fireEvent.scroll(viewport, { target: { scrollTop: 100 } })
    await userEvent.setup().click(screen.getByRole('button', { name: '加载更早的执行记录' }))
    await screen.findByText('Answer r-0')
    expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
    messages.push({
      id: 'm-new',
      seq: 1,
      agentId: 'a1',
      userId: 'u1',
      body: 'New task message',
      state: 'waiting',
      runId: null,
      threadSeq: null,
      errorCode: null,
      createdAt: '2026-10-18T02:00:00Z',
    })
    await act(async () =>
      queryClient.invalidateQueries({ queryKey: ['task-conversation', 't1', 'i1'] }),
    )
    expect(await screen.findByRole('button', { name: '有新内容 · 回到底部' })).toBeInTheDocument()
    expect(viewport.scrollTop).toBe(100)
  })

  it('refreshes active ownership after release even when no messages remain waiting', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [])
    let active = true
    server.use(
      http.get(CONVERSATION, () =>
        HttpResponse.json({
          items: [],
          prevCursor: null,
          nextCursor: null,
          activeRunId: active ? 'r-live' : null,
        }),
      ),
    )
    renderConversation(runs)
    expect(await screen.findByText('当前会话 · 空闲')).toBeInTheDocument()
    active = false
    await waitFor(() => expect(screen.queryByText('当前会话 · 空闲')).not.toBeInTheDocument(), {
      timeout: 7000,
    })
  }, 10000)

  it('does not label another member as the current user in a waiting message', async () => {
    mockConversation(
      [],
      [
        {
          id: 'm-other',
          seq: 1,
          agentId: 'a1',
          body: '另一个成员的消息',
          userId: 'other',
          createdAt: '2026-10-18T00:00:00Z',
          state: 'waiting',
          runId: null,
          threadSeq: null,
          errorCode: null,
        },
      ],
    )
    renderConversation([])
    expect(await screen.findByText('另一个成员的消息')).toBeInTheDocument()
    expect(screen.getByText('用户 → OpenCode')).toBeInTheDocument()
    expect(screen.queryByText('你 → OpenCode')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消等待' })).not.toBeInTheDocument()
  })

  it('ends the active Thread from the task page without stopping or pausing the queue', async () => {
    const runs = [
      run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z'),
      run('r-next', 'a2', 'queued', '2026-10-18T02:00:00Z'),
    ]
    mockConversation(runs, [])
    const ends: { key: string | null; body: unknown }[] = []
    let stops = 0
    server.use(
      http.post(`${RUNS}/r-live/thread/end`, async ({ request }) => {
        ends.push({ key: request.headers.get('Idempotency-Key'), body: await request.json() })
        return HttpResponse.json({ threadState: 'ending' }, { status: 202 })
      }),
      http.post(`${CONVERSATION}/stop`, () => {
        stops++
        return HttpResponse.json({ runId: 'r-live', state: 'stopping' }, { status: 202 })
      }),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await user.click(await screen.findByRole('button', { name: '结束会话' }))
    expect(ends).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: '确认结束' }))
    await waitFor(() => expect(ends).toHaveLength(1))
    expect(ends[0]).toEqual({ key: expect.any(String), body: {} })
    expect(stops).toBe(0)
    expect(screen.queryByText(/后续消息已暂停/)).not.toBeInTheDocument()
    expect(screen.getByLabelText('任务对话输入')).toBeInTheDocument()
  })

  it('keeps the active session end action visible beyond the five most recent queued runs', async () => {
    const runs = [
      run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z'),
      ...Array.from({ length: 6 }, (_, index) =>
        run(`r-queued-${index}`, 'a2', 'queued', `2026-10-18T02:0${index}:00Z`),
      ),
    ]
    mockConversation(runs, [])
    renderConversation(runs)
    expect(await screen.findByText('Task r-live')).toBeInTheDocument()
    expect(await screen.findByRole('button', { name: '结束会话' })).toBeInTheDocument()
  })

  it('hides task ending from anyone without the initiator and server permissions', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [], { canEnd: false })
    renderConversation(runs)
    await screen.findByText('Answer r-live')
    expect(screen.queryByRole('button', { name: '结束会话' })).not.toBeInTheDocument()
  })

  it('does not offer task ending to a non-initiator even if Thread admin permission allows it', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [], { canEnd: true })
    renderConversation(runs, 'admin-other-than-initiator')
    await screen.findByText('Answer r-live')
    expect(screen.queryByRole('button', { name: '结束会话' })).not.toBeInTheDocument()
  })

  it('shows personal-model gaps before sending while leaving plain comments available', async () => {
    const runs = [run('r-old', 'a1', 'completed', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [])
    server.use(
      http.get(CONVERSATION, () =>
        HttpResponse.json({
          items: [],
          nextCursor: null,
          prevCursor: null,
          agentReadiness: [
            { agentId: 'a1', requiresPersonalModel: true, errorCode: 'model_default_required' },
            { agentId: 'a2', requiresPersonalModel: false, errorCode: null },
          ],
        }),
      ),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await user.type(screen.getByLabelText('任务对话输入'), 'work on this')
    expect(await screen.findByText('请先配置个人默认模型')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^发送$/ })).toBeDisabled()
    await user.selectOptions(screen.getByLabelText('发送给'), 'a2')
    expect(screen.getByRole('button', { name: /^发送$/ })).toBeEnabled()
    await user.selectOptions(screen.getByLabelText('发送给'), 'a1')
    await user.click(screen.getByRole('button', { name: '仅评论' }))
    expect(screen.getByRole('button', { name: '发表评论' })).toBeEnabled()
  })
  it('reuses a failed send identity for manual retry and refreshes it after success', async () => {
    const runs: IssueRun[] = []
    const messages: TaskConversationMessage[] = []
    mockConversation(runs, messages)
    const keys: (string | null)[] = []
    server.use(
      http.post(`${CONVERSATION}/messages`, ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'))
        if (keys.length === 1)
          return HttpResponse.json(
            { code: 'unavailable', params: {}, requestId: 'lost-response' },
            { status: 503 },
          )
        const message: TaskConversationMessage = {
          id: `m${keys.length}`,
          seq: keys.length - 1,
          userId: 'u1',
          agentId: 'a1',
          body: 'repeat',
          state: 'waiting',
          runId: null,
          threadSeq: null,
          errorCode: null,
          createdAt: '2026-10-18T02:00:00Z',
        }
        messages.push(message)
        return HttpResponse.json({ resource: message }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await user.selectOptions(screen.getByLabelText('发送给'), 'a1')
    const editor = screen.getByLabelText('任务对话输入')
    await user.type(editor, 'repeat')
    await user.click(screen.getByRole('button', { name: /^发送$/ }))
    await screen.findByRole('alert')
    await user.click(screen.getByRole('button', { name: /^发送$/ }))
    await waitFor(() => expect(editor).toHaveValue(''))
    await user.type(editor, 'repeat')
    await user.click(screen.getByRole('button', { name: /^发送$/ }))
    await waitFor(() => expect(keys).toHaveLength(3))
    expect(keys[0]).toBeTypeOf('string')
    expect(keys[1]).toBe(keys[0])
    expect(keys[2]).not.toBe(keys[1])
  })
  it.each<{ status: IssueRun['status']; result: Exclude<IssueRun['result'], undefined> }>([
    { status: 'completed', result: { deliveryState: 'failed' } },
    { status: 'failed', result: null },
  ])(
    'recovers an older $status run with an undelivered result before newer work',
    async (prior) => {
      const failed: IssueRun = {
        ...run('r-failed', 'a1', prior.status, '2026-10-18T01:00:00Z'),
        result: prior.result,
      }
      const runs = [failed, run('r-next', 'a2', 'queued', '2026-10-18T02:00:00Z')]
      mockConversation(runs, [])
      const bodies: unknown[] = []
      server.use(
        http.get(CONVERSATION, () =>
          HttpResponse.json({
            items: [],
            nextCursor: null,
            prevCursor: null,
            blockedRunId: 'r-failed',
            activeRunId: 'r-next',
            canStop: false,
          }),
        ),
        http.post(`${CONVERSATION}/discard`, async ({ request }) => {
          bodies.push(await request.json())
          return HttpResponse.json({ acknowledgedRunId: 'r-failed' })
        }),
      )
      const user = userEvent.setup()
      renderConversation(runs)
      await user.click(await screen.findByRole('button', { name: '放弃未交付成果并继续' }))
      await waitFor(() =>
        expect(bodies).toEqual([{ runId: 'r-failed', acknowledgeLostWork: true }]),
      )
      expect(screen.queryByRole('button', { name: '停止当前执行' })).not.toBeInTheDocument()
    },
  )
  it('stops the current task execution without clearing the input draft', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [])
    let paused = false
    const bodies: unknown[] = []
    server.use(
      http.get(CONVERSATION, () =>
        HttpResponse.json({
          items: [],
          nextCursor: null,
          prevCursor: null,
          activeRunId: 'r-live',
          pausedRunId: paused ? 'r-live' : null,
          canStop: !paused,
        }),
      ),
      http.post(`${CONVERSATION}/stop`, async ({ request }) => {
        bodies.push(await request.json())
        paused = true
        return HttpResponse.json({ runId: 'r-live', state: 'stopping' }, { status: 202 })
      }),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await user.type(screen.getByLabelText('任务对话输入'), 'keep this draft')
    await user.click(await screen.findByRole('button', { name: '停止当前执行' }))
    expect(await screen.findByText(/后续消息已暂停。当前成果保存/)).toBeInTheDocument()
    expect(screen.getByLabelText('任务对话输入')).toHaveValue('keep this draft')
    expect(bodies).toEqual([{ runId: 'r-live' }])
  })
  it('keeps an ended run readable while a new same-Agent message starts the next run', async () => {
    const runs = [run('r-old', 'a1', 'completed', '2026-10-18T01:00:00Z')]
    const messages: TaskConversationMessage[] = []
    mockConversation(runs, messages)
    const posted: unknown[] = []
    server.use(
      http.post(`${CONVERSATION}/messages`, async ({ request }) => {
        posted.push({ body: await request.json(), key: request.headers.get('Idempotency-Key') })
        const message: TaskConversationMessage = {
          id: 'm1',
          seq: 1,
          userId: 'u1',
          agentId: 'a1',
          body: 'Please continue',
          state: 'waiting',
          runId: null,
          threadSeq: null,
          errorCode: null,
          createdAt: '2026-10-18T02:00:00Z',
        }
        messages.push(message)
        return HttpResponse.json({ resource: message }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    const { queryClient } = renderConversation(runs)
    expect(await screen.findByText('Answer r-old')).toBeInTheDocument()
    expect(screen.getByLabelText('发送给')).toHaveValue('a1')
    await user.type(screen.getByLabelText('任务对话输入'), '  Please continue  ')
    await user.click(screen.getByRole('button', { name: /^发送$/ }))
    expect(await screen.findByText('等待执行或交接')).toBeInTheDocument()
    expect(posted).toEqual([
      { body: { agentId: 'a1', body: '  Please continue  ' }, key: expect.any(String) },
    ])
    runs.push(run('r-next', 'a1', 'queued', '2026-10-18T02:01:00Z'))
    const firstMessage = messages[0]
    if (!firstMessage) throw new Error('Queued message missing')
    messages[0] = { ...firstMessage, state: 'submitted', runId: 'r-next' }
    await queryClient.invalidateQueries({ queryKey: ['issue-runs', 't1', 'i1'] })
    await queryClient.invalidateQueries({ queryKey: ['task-conversation', 't1', 'i1'] })
    expect(await screen.findByText('Task r-next')).toBeInTheDocument()
    expect(screen.getByText('Answer r-old')).toBeInTheDocument()
  })

  it('switches Agent only when a message is sent and lets its author cancel the pending handoff', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    const messages: TaskConversationMessage[] = []
    mockConversation(runs, messages)
    let sent = 0
    server.use(
      http.post(`${CONVERSATION}/messages`, async ({ request }) => {
        sent++
        expect(await request.json()).toEqual({ agentId: 'a2', body: 'Review the result' })
        const message: TaskConversationMessage = {
          id: 'm2',
          seq: 1,
          userId: 'u1',
          agentId: 'a2',
          body: 'Review the result',
          state: 'waiting',
          runId: null,
          threadSeq: null,
          errorCode: null,
          createdAt: '2026-10-18T02:00:00Z',
        }
        messages.push(message)
        return HttpResponse.json({ resource: message }, { status: 201 })
      }),
      http.post(`${CONVERSATION}/messages/m2/cancel`, () => {
        const waiting = messages[0]
        if (!waiting) throw new Error('Waiting message missing')
        messages[0] = { ...waiting, state: 'cancelled' }
        return HttpResponse.json({ resource: messages[0] })
      }),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await screen.findByText('Answer r-live')
    await user.selectOptions(screen.getByLabelText('发送给'), 'a2')
    expect(sent).toBe(0)
    expect(screen.getByText(/等待当前 Agent 完成/)).toHaveTextContent('Claude')
    await user.type(screen.getByLabelText('任务对话输入'), 'Review the result')
    await user.click(screen.getByRole('button', { name: /^发送$/ }))
    const list = await screen.findByRole('list', { name: '任务对话' })
    expect(await within(list).findByText('等待执行或交接')).toBeInTheDocument()
    expect(sent).toBe(1)
    await user.click(screen.getByRole('button', { name: '取消等待' }))
    expect(await screen.findByText('已取消')).toBeInTheDocument()
    expect(runs).toHaveLength(1)
  })

  it('posts a plain task comment without choosing or starting an Agent', async () => {
    const runs: IssueRun[] = []
    mockConversation(runs, [])
    const bodies: unknown[] = []
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ resource: { id: 'c1' } }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderConversation(runs)
    await user.click(screen.getByRole('button', { name: '仅评论' }))
    await user.type(screen.getByLabelText('任务对话输入'), 'Note for colleagues')
    await user.click(screen.getByRole('button', { name: '发表评论' }))
    await waitFor(() => expect(bodies).toEqual([{ body: 'Note for colleagues' }]))
    expect(runs).toHaveLength(0)
  })

  it('changes the recipient only when an Agent mention is selected', async () => {
    const runs = [run('r-live', 'a1', 'running', '2026-10-18T01:00:00Z')]
    mockConversation(runs, [])
    const user = userEvent.setup()
    renderConversation(runs)
    await screen.findByText('Answer r-live')
    const editor = screen.getByLabelText('任务对话输入')
    await user.type(editor, 'Please ask @Claude')
    expect(screen.getByLabelText('发送给')).toHaveValue('a1')
    await user.click(
      within(screen.getByLabelText('选择提及的目标')).getByRole('button', {
        name: 'Claude · Agent',
      }),
    )
    expect(screen.getByLabelText('发送给')).toHaveValue('a2')
    expect(editor).toHaveValue('Please ask')
    expect(screen.getByText(/等待当前 Agent 完成/)).toHaveTextContent('Claude')
  })

  it('requires an explicit recovery action after an undelivered run', async () => {
    const failed: IssueRun = {
      ...run('r-failed', 'a1', 'completed', '2026-10-18T01:00:00Z'),
      result: { deliveryState: 'failed' },
    }
    const messages: TaskConversationMessage[] = [
      {
        id: 'm-wait',
        seq: 1,
        userId: 'u1',
        agentId: 'a1',
        body: 'continue',
        state: 'waiting',
        runId: null,
        threadSeq: null,
        errorCode: null,
        createdAt: '2026-10-18T02:00:00Z',
      },
    ]
    mockConversation([failed], messages)
    const bodies: unknown[] = []
    server.use(
      http.post(`${CONVERSATION}/discard`, async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ acknowledgedRunId: failed.id })
      }),
    )
    const user = userEvent.setup()
    renderConversation([failed])
    expect(await screen.findByText('放弃未交付成果并继续')).toBeInTheDocument()
    expect(bodies).toHaveLength(0)
    await user.click(screen.getByRole('button', { name: '放弃未交付成果并继续' }))
    await waitFor(() => expect(bodies).toEqual([{ runId: 'r-failed', acknowledgeLostWork: true }]))
  })
})
