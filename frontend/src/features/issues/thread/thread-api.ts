import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import type {
  Error as ApiError,
  GetApiV1TenantsTidIssuesIidRunsRidThread200,
  GetApiV1TenantsTidIssuesIidRunsRidThreadParams,
  ThreadEntry,
} from '@/api/generated.schemas'
import {
  getApiV1TenantsTidIssuesIidRunsRidThread,
  getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey,
  postApiV1TenantsTidIssuesIidRunsRidThreadEnd,
  postApiV1TenantsTidIssuesIidRunsRidThreadMessages,
} from '@/api/tenants/tenants'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { faultCode, type ErrorType } from '@/lib/api-client'
import { mergeEntries, type ThreadState } from './thread-entries'

/** Identifies one agent run's Thread: tenant, issue and run. */
export interface ThreadRef {
  tid: string
  issueId: string
  runId: string
}

/**
 * The cached view of a Thread. `declared: false` means the GET answered 404:
 * the run's session has not been declared yet, which is a normal waiting
 * state rather than an error. Once declared, `entries` is one contiguous,
 * seq-ascending window of the Thread; older pages are prepended and newer
 * entries appended, so it never has a hole.
 */
export type ThreadSnapshot =
  | { declared: false }
  | ({ declared: true; entries: ThreadEntry[]; threadState: ThreadState } & ThreadDetails)

type ThreadDetails = Pick<
  GetApiV1TenantsTidIssuesIidRunsRidThread200,
  'initiatorUserId' | 'model' | 'canAppend' | 'canEnd'
>

function threadDetails(page: ThreadDetails): ThreadDetails {
  return {
    initiatorUserId: page.initiatorUserId,
    model: page.model,
    canAppend: page.canAppend,
    canEnd: page.canEnd,
  }
}

/** Entries requested per page; the server caps a page at 500. */
const THREAD_PAGE_LIMIT = 200

/** Poll interval while the session is not declared yet (GET answers 404). */
export const THREAD_DECLARE_POLL_MS = 2500

/**
 * Fallback poll while the Thread is live. Space events normally trigger the
 * read; this only bounds staleness when the event stream has dropped.
 */
export const THREAD_LIVE_POLL_MS = 5000

/**
 * The Thread's query key. It is the generated client's key without params, so
 * the space-event handler can address it from `tenantId`, `issueId` and
 * `runId` alone, and a stream reconnect's tenant-wide refetch covers it too.
 */
export function threadQueryKey(ref: ThreadRef) {
  return getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey(ref.tid, ref.issueId, ref.runId)
}

function readPage(
  ref: ThreadRef,
  params: GetApiV1TenantsTidIssuesIidRunsRidThreadParams,
  signal?: AbortSignal,
): Promise<GetApiV1TenantsTidIssuesIidRunsRidThread200> {
  return getApiV1TenantsTidIssuesIidRunsRidThread(
    ref.tid,
    ref.issueId,
    ref.runId,
    params,
    undefined,
    signal,
  )
}

/**
 * The exclusive `after` cursor for an incremental read: the highest seen seq,
 * pulled back to just before the oldest still-queued user turn. Re-reading
 * from there is what refreshes a turn's queued → delivered status, which an
 * after-the-tail read would never revisit.
 */
export function forwardCursor(entries: readonly ThreadEntry[]): number {
  const firstQueued = entries.find((entry) => entry.status === 'queued')
  const last = entries.at(-1)?.seq ?? 0
  return firstQueued ? Math.min(last, firstQueued.seq - 1) : last
}

/** Reads forward until a short page proves the window reached the Thread's end. */
async function readNewer(
  ref: ThreadRef,
  entries: ThreadEntry[],
  signal: AbortSignal,
): Promise<{ entries: ThreadEntry[]; threadState: ThreadState } & ThreadDetails> {
  const page = await readPage(
    ref,
    { limit: THREAD_PAGE_LIMIT, after: forwardCursor(entries) },
    signal,
  )
  const merged = mergeEntries(entries, page.items)
  if (page.items.length < THREAD_PAGE_LIMIT)
    return { entries: merged, threadState: page.threadState, ...threadDetails(page) }
  return readNewer(ref, merged, signal)
}

/**
 * One refetch: the tail when nothing is cached yet, otherwise only what is new
 * since the cached window. The result is merged into whatever the cache holds
 * when the read finishes, so a "load older" page that landed meanwhile is kept.
 */
async function readThread(
  ref: ThreadRef,
  queryClient: QueryClient,
  signal: AbortSignal,
): Promise<ThreadSnapshot> {
  const key = threadQueryKey(ref)
  const cached = queryClient.getQueryData<ThreadSnapshot>(key)
  try {
    const next =
      cached?.declared && cached.entries.length > 0
        ? await readNewer(ref, cached.entries, signal)
        : await readPage(ref, { limit: THREAD_PAGE_LIMIT }, signal).then((page) => ({
            entries: page.items,
            threadState: page.threadState,
            ...threadDetails(page),
          }))
    const latest = queryClient.getQueryData<ThreadSnapshot>(key)
    const base = latest?.declared ? latest.entries : []
    return {
      declared: true,
      entries: mergeEntries(base, next.entries),
      threadState: next.threadState,
      ...threadDetails(next),
    }
  } catch (error) {
    if (faultCode(error) === 'not_found') return { declared: false }
    throw error
  }
}

/** How often the Thread query polls in a given state; ended Threads stop polling. */
export function threadPollInterval(snapshot: ThreadSnapshot | undefined): number | false {
  if (snapshot === undefined) return false
  if (!snapshot.declared) return THREAD_DECLARE_POLL_MS
  return snapshot.threadState === 'ended' ? false : THREAD_LIVE_POLL_MS
}

/**
 * The Thread of one agent run. The first read loads the tail; every later read
 * (poll, space event, mutation) fetches only entries after the cached window,
 * so an entry is never duplicated and a concurrent append is never skipped.
 */
export function useThread(ref: ThreadRef) {
  const queryClient = useQueryClient()
  return useQuery({
    queryKey: threadQueryKey(ref),
    queryFn: ({ signal }) => readThread(ref, queryClient, signal),
    refetchInterval: (query) => threadPollInterval(query.state.data),
  })
}

/**
 * Pages one window older than the cached one (`before` = the oldest cached
 * seq) and prepends it. It is a mutation because it extends the cached query
 * imperatively rather than reading a query of its own.
 */
export function useLoadOlderThread(ref: ThreadRef) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (before: number) => readPage(ref, { limit: THREAD_PAGE_LIMIT, before }),
    onSuccess: (page) => {
      queryClient.setQueryData<ThreadSnapshot>(threadQueryKey(ref), (current) =>
        current?.declared
          ? { ...current, entries: mergeEntries(page.items, current.entries) }
          : current,
      )
    },
  })
}

/** One user turn's text, as the composer submits it. */
export interface ThreadMessageInput {
  text: string
}

/**
 * Appends a user turn. The idempotency key is minted per submission and
 * replayed by retries of that submission. The created entry is deliberately
 * not merged into the cache: its seq may be ahead of entries not read yet, and
 * advancing the window past them would skip them forever. A refetch picks it
 * up in order instead.
 */
export function useSendThreadMessage(ref: ThreadRef) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<ThreadEntry, ErrorType<ApiError>, ThreadMessageInput>({
    mutationFn: async (input) => {
      const created = await postApiV1TenantsTidIssuesIidRunsRidThreadMessages(
        ref.tid,
        ref.issueId,
        ref.runId,
        { content: [{ type: 'text', text: input.text }] },
        { headers: mutationHeaders(keyFor(input)) },
      )
      return created.resource
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: threadQueryKey(ref) }),
  })
}

/** Asks the session to end; on acceptance the cached state moves to `ending` at once. */
export function useEndThread(ref: ThreadRef) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<{ threadState: ThreadState }, ErrorType<ApiError>, Record<string, never>>({
    mutationFn: (body) =>
      postApiV1TenantsTidIssuesIidRunsRidThreadEnd(ref.tid, ref.issueId, ref.runId, body, {
        headers: mutationHeaders(keyFor(body)),
      }),
    onSuccess: (result) => {
      queryClient.setQueryData<ThreadSnapshot>(threadQueryKey(ref), (current) =>
        current?.declared ? { ...current, threadState: result.threadState } : current,
      )
    },
    onSettled: () => queryClient.invalidateQueries({ queryKey: threadQueryKey(ref) }),
  })
}

const SEND_FAULTS: Record<string, string> = {
  thread_closed: '会话已结束，无法继续发送',
  content_too_large: '消息过长',
  forbidden: '没有发送此消息的权限',
  model_run_initiator_required: '只有运行发起者可以继续发送模型请求',
  model_connection_unavailable: '此会话的模型授权已失效，请结束后重新发起任务',
}

const END_FAULTS: Record<string, string> = {
  thread_closed: '会话已结束',
}

/** User-facing reason a send failed, keyed by the public Fault code. */
export function sendFailureMessage(error: unknown): string {
  return SEND_FAULTS[faultCode(error) ?? ''] ?? '发送失败，请稍后重试'
}

/** User-facing reason an end request failed, keyed by the public Fault code. */
export function endFailureMessage(error: unknown): string {
  return END_FAULTS[faultCode(error) ?? ''] ?? '结束会话失败，请稍后重试'
}
