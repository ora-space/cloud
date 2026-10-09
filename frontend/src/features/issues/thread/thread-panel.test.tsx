import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { ThreadEntry } from '@/api/generated.schemas'
import { useSpaceEvents } from '@/features/spaces/use-space-events'
import type { IssueRun } from '@/features/issues/types'
import { server } from '@/test/msw-server'
import type { ThreadState } from './thread-entries'
import { IssueThreadPanel } from './thread-panel'

const RUNS_URL = '/api/v1/tenants/t1/issues/i1/runs'
const THREAD_URL = '/api/v1/tenants/t1/issues/i1/runs/r-new/thread'
const EVENTS_URL = '/api/v1/tenants/t1/spaces/s1/events'

/** A fake Thread backend with the server's cursor semantics (tail / after / before). */
interface ThreadStore {
  declared: boolean
  state: ThreadState
  entries: ThreadEntry[]
}

function entry(seq: number, overrides: Partial<ThreadEntry> = {}): ThreadEntry {
  return {
    seq,
    kind: 'update',
    source: 'node',
    record: { update: { sessionUpdate: 'tool_call' } },
    createdAt: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

function agentSays(seq: number, text: string, turnId: string): ThreadEntry {
  return entry(seq, {
    turnId,
    record: { update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } } },
  })
}

const prompt = entry(1, { source: 'system', kind: 'user_turn', record: { content: 'Fix login' } })

function run(id: string, executorType: IssueRun['executorType'], createdAt: string): IssueRun {
  return {
    id,
    tenantId: 't1',
    issueId: 'i1',
    executorType,
    executorId: 'e1',
    input: {},
    status: 'running',
    createdAt,
    updatedAt: createdAt,
  }
}

function faultResponse(code: string, status: number) {
  return HttpResponse.json({ code, params: {}, requestId: 'r' }, { status })
}

/** Serves the runs list (newest agent run `r-new`) and the Thread from `store`. */
function serveThread(store: ThreadStore): URLSearchParams[] {
  const reads: URLSearchParams[] = []
  server.use(
    http.get(RUNS_URL, () =>
      HttpResponse.json({
        items: [
          run('r-old', 'agent', '2026-10-01T00:00:00Z'),
          run('r-new', 'agent', '2026-10-02T00:00:00Z'),
          run('r-wf', 'workflow', '2026-10-03T00:00:00Z'),
        ],
        nextCursor: '',
      }),
    ),
    http.get(THREAD_URL, ({ request }) => {
      const params = new URL(request.url).searchParams
      reads.push(params)
      if (!store.declared) return faultResponse('not_found', 404)
      const limit = Number(params.get('limit') ?? '200')
      const after = params.get('after')
      const before = params.get('before')
      let window = store.entries.slice(-limit)
      if (after !== null)
        window = store.entries.filter((e) => e.seq > Number(after)).slice(0, limit)
      if (before !== null)
        window = store.entries.filter((e) => e.seq < Number(before)).slice(-limit)
      return HttpResponse.json({
        items: window,
        nextCursor: window.at(-1)?.seq ?? null,
        prevCursor: window[0]?.seq ?? null,
        threadState: store.state,
        idleSince: null,
      })
    }),
  )
  return reads
}

function EventsSubscriber() {
  useSpaceEvents('t1', 's1')
  return null
}

function renderPanel({ withEvents = false } = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      {withEvents && <EventsSubscriber />}
      <IssueThreadPanel tid="t1" issueId="i1" />
    </QueryClientProvider>,
  )
}

function messages() {
  return within(screen.getByRole('list', { name: '会话消息' }))
}

describe('IssueThreadPanel', () => {
  it('loads the tail of the newest agent run and shows the conversation and state', async () => {
    const reads = serveThread({
      declared: true,
      state: 'active',
      entries: [
        prompt,
        entry(2, { kind: 'meta', record: {} }),
        agentSays(3, 'Looking ', 't1'),
        agentSays(4, 'into it', 't1'),
        entry(5, {
          source: 'user',
          kind: 'user_turn',
          status: 'delivered',
          record: { content: [{ type: 'text', text: 'Thanks' }] },
        }),
      ],
    })
    renderPanel()

    expect(await screen.findByRole('heading', { name: 'Agent 会话' })).toBeInTheDocument()
    expect(await screen.findByText('Looking into it')).toBeInTheDocument()
    expect(messages().getByText('Fix login')).toBeInTheDocument()
    expect(messages().getByText('Thanks')).toBeInTheDocument()
    expect(messages().getByText('已送达')).toBeInTheDocument()
    expect(messages().getByText('会话已开始')).toBeInTheDocument()
    expect(screen.getByText('进行中')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '加载更早' })).not.toBeInTheDocument()
    expect(reads[0]?.toString()).toBe('limit=200')
  })

  it('renders nothing when the issue has no agent run', async () => {
    let asked = false
    server.use(
      http.get(RUNS_URL, () => {
        asked = true
        return HttpResponse.json({
          items: [run('r-wf', 'workflow', '2026-10-03T00:00:00Z')],
          nextCursor: '',
        })
      }),
    )
    const { container } = renderPanel()
    await waitFor(() => expect(asked).toBe(true))
    expect(container).toBeEmptyDOMElement()
  })

  it('waits while the session is undeclared and shows the Thread once it appears', async () => {
    const store: ThreadStore = { declared: false, state: 'pending', entries: [prompt] }
    serveThread(store)
    renderPanel()

    expect(await screen.findByText('等待 Agent 会话启动…')).toBeInTheDocument()
    store.declared = true
    expect(await screen.findByText('Fix login', undefined, { timeout: 6000 })).toBeInTheDocument()
    expect(screen.getByText('等待中')).toBeInTheDocument()
    expect(screen.queryByText('等待 Agent 会话启动…')).not.toBeInTheDocument()
  })

  it('pages older entries with before=<oldest seq> and prepends them once', async () => {
    const tools = Array.from({ length: 203 }, (_, i) => entry(i + 2))
    const reads = serveThread({
      declared: true,
      state: 'idle',
      entries: [prompt, ...tools, agentSays(205, 'Done', 't9')],
    })
    const user = userEvent.setup()
    renderPanel()

    expect(await screen.findByText('Done')).toBeInTheDocument()
    expect(messages().getAllByText('tool_call')).toHaveLength(199)
    await user.click(screen.getByRole('button', { name: '加载更早' }))

    expect(await screen.findByText('Fix login')).toBeInTheDocument()
    expect(messages().getAllByText('tool_call')).toHaveLength(203)
    expect(reads.some((params) => params.get('before') === '6')).toBe(true)
    expect(screen.queryByRole('button', { name: '加载更早' })).not.toBeInTheDocument()
  })

  it('sends a message with an idempotency key and shows it after the refetch', async () => {
    const store: ThreadStore = { declared: true, state: 'idle', entries: [prompt] }
    const reads = serveThread(store)
    const posted: { body: unknown; key: string | null }[] = []
    server.use(
      http.post(`${THREAD_URL}/messages`, async ({ request }) => {
        const body = await request.json()
        posted.push({ body, key: request.headers.get('Idempotency-Key') })
        const created = entry(2, {
          source: 'user',
          kind: 'user_turn',
          status: 'queued',
          record: { content: [{ type: 'text', text: 'Please hurry' }] },
        })
        store.entries.push(created)
        return HttpResponse.json({ resource: created }, { status: 201 })
      }),
    )
    const user = userEvent.setup()
    renderPanel()

    await screen.findByText('Fix login')
    await user.type(screen.getByLabelText('给 Agent 发送消息'), '  Please hurry ')
    await user.click(screen.getByRole('button', { name: '发送' }))

    expect(await messages().findByText('Please hurry')).toBeInTheDocument()
    expect(messages().getByText('排队中')).toBeInTheDocument()
    expect(posted).toEqual([
      { body: { content: [{ type: 'text', text: 'Please hurry' }] }, key: expect.any(String) },
    ])
    expect(posted[0]?.key).not.toBe('')
    expect(reads.at(-1)?.get('after')).toBe('1')
    expect(screen.getByLabelText('给 Agent 发送消息')).toHaveValue('')
  })

  it('explains a thread_closed refusal and keeps the draft', async () => {
    serveThread({ declared: true, state: 'active', entries: [prompt] })
    server.use(http.post(`${THREAD_URL}/messages`, () => faultResponse('thread_closed', 409)))
    const user = userEvent.setup()
    renderPanel()

    await screen.findByText('Fix login')
    await user.type(screen.getByLabelText('给 Agent 发送消息'), 'Still there?')
    await user.click(screen.getByRole('button', { name: '发送' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('会话已结束，无法继续发送')
    expect(screen.getByLabelText('给 Agent 发送消息')).toHaveValue('Still there?')
  })

  it('ends the session only after confirmation and then disables the composer', async () => {
    const store: ThreadStore = { declared: true, state: 'active', entries: [prompt] }
    serveThread(store)
    let ended = 0
    server.use(
      http.post(`${THREAD_URL}/end`, async ({ request }) => {
        expect(await request.json()).toEqual({})
        ended += 1
        store.state = 'ending'
        return HttpResponse.json({ threadState: 'ending' }, { status: 202 })
      }),
    )
    const user = userEvent.setup()
    renderPanel()

    await screen.findByText('Fix login')
    await user.click(screen.getByRole('button', { name: '结束会话' }))
    await user.click(screen.getByRole('button', { name: '取消' }))
    expect(ended).toBe(0)

    await user.click(screen.getByRole('button', { name: '结束会话' }))
    await user.click(screen.getByRole('button', { name: '确认结束' }))

    expect(await screen.findByText('结束中')).toBeInTheDocument()
    expect(ended).toBe(1)
    expect(screen.getByRole('button', { name: '结束会话' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '发送' })).toBeDisabled()
    expect(screen.getByLabelText('给 Agent 发送消息')).toBeDisabled()
  })

  it('shows why ending failed when the server already closed the Thread', async () => {
    serveThread({ declared: true, state: 'idle', entries: [prompt] })
    server.use(http.post(`${THREAD_URL}/end`, () => faultResponse('thread_closed', 409)))
    const user = userEvent.setup()
    renderPanel()

    await screen.findByText('Fix login')
    await user.click(screen.getByRole('button', { name: '结束会话' }))
    await user.click(screen.getByRole('button', { name: '确认结束' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('会话已结束')
  })

  it('reads the new entries after a thread_appended space event', async () => {
    const store: ThreadStore = { declared: true, state: 'active', entries: [prompt] }
    const reads = serveThread(store)
    const encoder = new TextEncoder()
    let push: ((frame: string) => void) | undefined
    server.use(
      http.get(
        EVENTS_URL,
        () =>
          new HttpResponse(
            new ReadableStream<Uint8Array>({
              start(controller) {
                push = (frame) => controller.enqueue(encoder.encode(frame))
              },
            }),
            { headers: { 'Content-Type': 'text/event-stream' } },
          ),
      ),
    )
    renderPanel({ withEvents: true })

    await screen.findByText('Fix login')
    await waitFor(() => expect(push).toBeDefined())
    store.entries.push(agentSays(2, 'Pushed reply', 't1'))
    push?.(
      'data: {"type":"issue_run.thread_appended","spaceId":"s1","issueId":"i1","runId":"r-new","lastSeq":2}\n\n',
    )

    // Well inside the 5s fallback poll, so only the event can have caused the read.
    expect(
      await screen.findByText('Pushed reply', undefined, { timeout: 2000 }),
    ).toBeInTheDocument()
    expect(reads.at(-1)?.get('after')).toBe('1')
  })
})
