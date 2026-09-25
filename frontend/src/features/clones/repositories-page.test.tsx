import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { CloneOperation } from '@/api/generated.schemas'
import { installCloudSpaceHandlers, TEST_TENANT_ID, TEST_USER_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { CLONE_POLL_INTERVAL_MS } from './api'
import {
  STORAGE_UNREADABLE_NOTICE,
  STORAGE_WRITE_FAILED_NOTICE,
  UNCONFIRMED_NOTICE,
} from './messages'
import { pendingSubmissionKey, readPendingSubmission, type CloneSubmission } from './pending'
import { RepositoriesPage } from './repositories-page'

const CLONES = `/api/v1/tenants/${TEST_TENANT_ID}/clones`
const KEY = pendingSubmissionKey(TEST_TENANT_ID, TEST_USER_ID)
const REPOSITORY = 'https://github.com/octocat/Hello-World'
const COMMIT = '7fd1a60b01f91b314f59955a4e4d4e80d8edf11d'

function operation(operationId: string, overrides: Partial<CloneOperation> = {}): CloneOperation {
  return {
    operationId,
    requestId: `request-${operationId}`,
    repository: REPOSITORY,
    branch: 'master',
    executionId: null,
    nodeId: null,
    state: { kind: 'pending' },
    createdAt: '2026-09-23T08:00:00Z',
    updatedAt: '2026-09-23T08:00:00Z',
    ...overrides,
  }
}

/** Serves the list from `items`, which a test may change between reads; counts every read. */
function serveClones(items: CloneOperation[], nextCursor = '') {
  const reads = { count: 0 }
  server.use(
    http.get(CLONES, () => {
      reads.count += 1
      return HttpResponse.json({ items, nextCursor })
    }),
  )
  return reads
}

/** Accepts submissions like Cloud: records them, lists them, answers 202. */
function acceptClones(items: CloneOperation[]) {
  const sent: { body: unknown; key: string | null }[] = []
  server.use(
    http.post(CLONES, async ({ request }) => {
      const body: unknown = await request.json()
      sent.push({ body, key: request.headers.get('Idempotency-Key') })
      const accepted = operation(`op-${sent.length}`)
      items.push(accepted)
      return HttpResponse.json(accepted, { status: 202 })
    }),
  )
  return sent
}

function renderPage() {
  return renderWithProviders(<RepositoriesPage slug="cloud-dev" />, { slug: 'cloud-dev' })
}

async function openDialog(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: /Clone 仓库/ }))
  return screen.findByRole('dialog')
}

async function fillAndSubmit(
  user: ReturnType<typeof userEvent.setup>,
  { repository, branch }: { repository: string; branch: string },
) {
  const dialog = await openDialog(user)
  await user.type(within(dialog).getByLabelText(/仓库 URL/), repository)
  const branchInput = within(dialog).getByLabelText('分支')
  await user.clear(branchInput)
  await user.type(branchInput, branch)
  await user.click(within(dialog).getByRole('button', { name: '提交' }))
  return dialog
}

const advancePoll = () => act(() => vi.advanceTimersByTimeAsync(CLONE_POLL_INTERVAL_MS))

function storePending(key: string, submission: CloneSubmission) {
  sessionStorage.setItem(key, JSON.stringify(submission))
}

beforeEach(() => sessionStorage.clear())
afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

describe('RepositoriesPage', () => {
  it('lists the facts Cloud reported, newest first', async () => {
    installCloudSpaceHandlers('member')
    serveClones(
      [
        operation('queued', { createdAt: '2026-09-23T08:00:00Z' }),
        operation('failed', {
          createdAt: '2026-09-23T11:00:00Z',
          executionId: 'exec-f',
          nodeId: 'node-a',
          state: { kind: 'failed', reason: 'branchNotFound', retainedPath: '/srv/left' },
        }),
        operation('done', {
          createdAt: '2026-09-23T10:00:00Z',
          executionId: 'exec-d',
          nodeId: 'node-a',
          state: { kind: 'succeeded', commit: COMMIT, path: '/srv/repositories/done' },
        }),
        operation('dispatched', {
          createdAt: '2026-09-23T09:00:00Z',
          executionId: 'exec-p',
          nodeId: 'node-a',
        }),
        operation('interrupted', {
          createdAt: '2026-09-23T07:00:00Z',
          executionId: 'exec-i',
          nodeId: 'node-a',
          state: { kind: 'failed', reason: 'interrupted', retainedPath: '/srv/cut' },
        }),
      ],
      'more',
    )
    renderPage()

    await screen.findByText('等待 Controller 领取')
    const rows = screen.getAllByRole('row').slice(1)
    expect(rows.map((row) => within(row).getAllByRole('cell')[1]?.textContent)).toEqual([
      '失败',
      '已完成',
      '已派发',
      '排队中',
      '失败',
    ])
    const outcomes = rows.map((row) => within(row).getAllByRole('cell')[2]?.textContent)
    expect(outcomes).toEqual([
      '找不到指定分支残留已保留：/srv/left',
      `${COMMIT.slice(0, 12)}/srv/repositories/done`,
      '已交给 Node node-a，等待结果',
      '等待 Controller 领取',
      '执行被中断，可重新提交残留已保留：/srv/cut',
    ])
    expect(screen.getByText('仅显示其中 100 条记录。')).toBeInTheDocument()
  })

  it('sends a trimmed submission whose request id is also its idempotency key, then lists it', async () => {
    installCloudSpaceHandlers('member')
    const items: CloneOperation[] = []
    serveClones(items)
    const sent = acceptClones(items)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/还没有 clone 过仓库/)
    await fillAndSubmit(user, { repository: `  ${REPOSITORY} `, branch: ' master ' })

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(sent).toEqual([
      {
        body: { requestId: expect.any(String), repository: REPOSITORY, branch: 'master' },
        key: expect.any(String),
      },
    ])
    expect(sent[0]?.body).toEqual(expect.objectContaining({ requestId: sent[0]?.key }))
    expect(await screen.findByText('等待 Controller 领取')).toBeInTheDocument()
    expect(sessionStorage.getItem(KEY)).toBeNull()
  })

  it('keeps the request id when the response is lost and resends it unchanged', async () => {
    installCloudSpaceHandlers('member')
    const items: CloneOperation[] = []
    serveClones(items)
    const sent = acceptClones(items)
    const lost: unknown[] = []
    server.use(
      http.post(
        CLONES,
        async ({ request }) => {
          lost.push(await request.json())
          return HttpResponse.error()
        },
        { once: true },
      ),
    )
    const user = userEvent.setup()
    renderPage()

    const dialog = await fillAndSubmit(user, { repository: REPOSITORY, branch: 'master' })
    expect(await within(dialog).findByText(UNCONFIRMED_NOTICE)).toBeInTheDocument()
    expect(within(dialog).getByText('尚未确认的提交')).toBeInTheDocument()
    expect(readPendingSubmission(KEY)).toEqual({ status: 'unconfirmed', submission: lost[0] })

    await user.click(within(dialog).getByRole('button', { name: '重试原请求' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(sent.map((s) => s.body)).toEqual(lost)
    expect(sessionStorage.getItem(KEY)).toBeNull()
  })

  it('restores an unconfirmed submission after a reload without sending it', async () => {
    installCloudSpaceHandlers('member')
    serveClones([])
    const sent = acceptClones([])
    storePending(KEY, { requestId: 'kept', repository: REPOSITORY, branch: 'master' })
    const user = userEvent.setup()
    renderPage()

    await user.click(await screen.findByRole('button', { name: '查看并重试' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(REPOSITORY)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '重试原请求' })).toBeInTheDocument()
    expect(sent).toEqual([])
  })

  it('confirms an unconfirmed submission once Cloud lists its request id', async () => {
    installCloudSpaceHandlers('member')
    serveClones([operation('accepted', { requestId: 'kept' })])
    storePending(KEY, { requestId: 'kept', repository: REPOSITORY, branch: 'master' })
    renderPage()

    await screen.findByText('等待 Controller 领取')
    await waitFor(() => expect(sessionStorage.getItem(KEY)).toBeNull())
    expect(screen.queryByRole('button', { name: '查看并重试' })).not.toBeInTheDocument()
  })

  it('drops a refused submission and explains the fault code', async () => {
    installCloudSpaceHandlers('member')
    serveClones([])
    server.use(
      http.post(CLONES, () =>
        HttpResponse.json({ code: 'invalid_ref', params: {}, requestId: 'r' }, { status: 400 }),
      ),
    )
    const user = userEvent.setup()
    renderPage()

    const dialog = await fillAndSubmit(user, { repository: REPOSITORY, branch: 'feature' })
    expect(await within(dialog).findByText(/分支名无效/)).toBeInTheDocument()
    expect(sessionStorage.getItem(KEY)).toBeNull()
    expect(within(dialog).getByRole('button', { name: '提交' })).toBeInTheDocument()
  })

  it("never offers another member's unconfirmed submission", async () => {
    installCloudSpaceHandlers('member')
    serveClones([])
    storePending(pendingSubmissionKey(TEST_TENANT_ID, 'someone-else'), {
      requestId: 'theirs',
      repository: REPOSITORY,
      branch: 'master',
    })
    const user = userEvent.setup()
    renderPage()

    const dialog = await openDialog(user)
    expect(within(dialog).getByLabelText(/仓库 URL/)).toHaveValue('')
    expect(screen.queryByRole('button', { name: '查看并重试' })).not.toBeInTheDocument()
  })

  it('polls while a clone awaits its result, survives a failed read and stops once terminal', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    installCloudSpaceHandlers('member')
    const answers = [
      () => HttpResponse.json({ items: [operation('a')], nextCursor: '' }),
      () => HttpResponse.json({ code: 'unavailable', params: {}, requestId: 'r' }, { status: 503 }),
      () =>
        HttpResponse.json({
          items: [operation('a', { state: { kind: 'succeeded', commit: COMMIT, path: '/p' } })],
          nextCursor: '',
        }),
    ]
    let reads = 0
    server.use(
      http.get(CLONES, () => {
        const answer = answers[Math.min(reads, answers.length - 1)]
        reads += 1
        return answer ? answer() : HttpResponse.error()
      }),
    )
    renderPage()

    expect(await screen.findByText('等待 Controller 领取')).toBeInTheDocument()
    await advancePoll()
    expect(await screen.findByRole('alert')).toBeInTheDocument()
    expect(screen.getByText('等待 Controller 领取')).toBeInTheDocument()
    await advancePoll()
    expect(await screen.findByText('/p')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    await advancePoll()
    await advancePoll()
    expect(reads).toBe(3)
  })

  it('refuses to mint a new identity when session storage cannot be read', async () => {
    installCloudSpaceHandlers('member')
    serveClones([])
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('storage denied')
    })
    const user = userEvent.setup()
    renderPage()

    const dialog = await openDialog(user)
    await user.type(within(dialog).getByLabelText(/仓库 URL/), REPOSITORY)
    expect(within(dialog).getByText(STORAGE_UNREADABLE_NOTICE)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: '提交' })).toBeDisabled()
  })

  it('sends nothing when the identity cannot be written first', async () => {
    installCloudSpaceHandlers('member')
    serveClones([])
    const sent = acceptClones([])
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('quota exceeded')
    })
    const user = userEvent.setup()
    renderPage()

    const dialog = await fillAndSubmit(user, { repository: REPOSITORY, branch: 'master' })
    expect(await within(dialog).findByText(STORAGE_WRITE_FAILED_NOTICE)).toBeInTheDocument()
    expect(sent).toEqual([])
  })

  it('requests nothing and offers no clone until the space resolved', () => {
    renderPage()

    expect(screen.getByText('仓库')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Clone 仓库/ })).not.toBeInTheDocument()
  })
})
