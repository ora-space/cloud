import { useEffect } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { SpaceEvent } from '@/api/generated.schemas'
import { getGetApiV1MeSpacesQueryKey } from '@/api/me/me'
import { getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey } from '@/api/tenants/tenants'

/**
 * Narrows an unknown value to a property bag; JSON.parse and network payloads
 * are unknown until shaped, and a type guard is the boundary check.
 */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

/**
 * Validates an untrusted SSE payload at the boundary so only well-shaped
 * events reach the query cache. Events come from the network, never from
 * types.
 */
function isSpaceEvent(value: unknown): value is SpaceEvent {
  if (!isRecord(value)) return false
  if (typeof value['type'] !== 'string' || typeof value['spaceId'] !== 'string') return false
  return ['issueId', 'runId'].every((field) => optionalString(value[field]))
}

function optionalString(value: unknown): boolean {
  return value === undefined || typeof value === 'string'
}

/**
 * A Thread notice refetches that run's Thread and the issue's run list (the
 * run's status moves with its session). `lastSeq` is only a hint and is never
 * used as a cursor: the Thread query reads forward from what it has cached.
 */
function invalidateForThreadEvent(event: SpaceEvent, tenantId: string, queryClient: QueryClient) {
  if (!event.issueId) return
  void queryClient.invalidateQueries({ queryKey: ['issue-runs', tenantId, event.issueId] })
  if (!event.runId) return
  void queryClient.invalidateQueries({
    queryKey: getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey(
      tenantId,
      event.issueId,
      event.runId,
    ),
  })
}

/**
 * Splits a buffered SSE byte stream into typed events, keeping any trailing
 * partial frame in `rest`. Malformed `data:` payloads are skipped: an event
 * only triggers refetches, so a dropped notice costs at most a manual refresh.
 */
export function parseSSEFrames(buffer: string): { events: SpaceEvent[]; rest: string } {
  const events: SpaceEvent[] = []
  let rest = buffer
  for (;;) {
    const end = rest.indexOf('\n\n')
    if (end < 0) break
    const frame = rest.slice(0, end)
    rest = rest.slice(end + 2)
    for (const line of frame.split('\n')) {
      if (!line.startsWith('data: ')) continue
      try {
        const parsed: unknown = JSON.parse(line.slice('data: '.length))
        if (isSpaceEvent(parsed)) events.push(parsed)
      } catch {
        // ignore a frame this client cannot parse
      }
    }
  }
  return { events, rest }
}

/** Invalidates exactly the queries an event type affects; never carries state. */
function invalidateForEvent(
  event: SpaceEvent,
  tenantId: string,
  spaceId: string,
  queryClient: QueryClient,
): void {
  if (event.type === 'issue_run.thread_appended' || event.type === 'issue_run.thread_changed') {
    invalidateForThreadEvent(event, tenantId, queryClient)
  } else if (event.type.startsWith('project.')) {
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/projects`],
    })
  } else if (event.type === 'space.member_updated') {
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/members`],
    })
  } else if (event.type === 'space.plugins_updated') {
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/plugins`],
    })
  } else if (event.type === 'plugins.catalog_updated') {
    // The catalog is shared across spaces; this stream's subscriber refetches
    // its own copy, which is enough until a broker fans the notice out.
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/plugins/catalog`],
    })
  } else {
    void queryClient.invalidateQueries({
      queryKey: getGetApiV1MeSpacesQueryKey(),
    })
  }
}

/** Reconnect backoff: doubles from the first delay up to the cap, resets after a healthy stream. */
export const RECONNECT_DELAYS_MS = { first: 1000, max: 30000 } as const

/** Backoff delay for the n-th consecutive failed connection (0-based). */
export function reconnectDelay(attempt: number): number {
  return Math.min(RECONNECT_DELAYS_MS.first * 2 ** attempt, RECONNECT_DELAYS_MS.max)
}

// Resolves after `ms`, or immediately when the signal aborts, so an unmount
// never leaves a reconnect timer pending.
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort)
      resolve()
    }, ms)
    function onAbort() {
      clearTimeout(timer)
      resolve()
    }
    signal.addEventListener('abort', onAbort, { once: true })
  })
}

// Pulls frames off one open stream until it ends or the signal aborts.
async function drain(
  body: ReadableStream<Uint8Array>,
  onEvent: (event: SpaceEvent) => void,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  for (;;) {
    // oxlint-disable-next-line no-await-in-loop -- incremental stream, sequential awaits are the protocol
    const { done, value } = await reader.read()
    if (done) return
    buffer += decoder.decode(value, { stream: true })
    const { events, rest } = parseSSEFrames(buffer)
    buffer = rest
    events.forEach(onEvent)
  }
}

/**
 * Subscribes the tab to the space event stream and invalidates the affected
 * queries on every notice. Events are lightweight: they only trigger
 * refetches against the authoritative REST state, never carry it.
 *
 * The stream rides the gateway session cookie like every other request. A
 * dropped connection is reconnected with exponential backoff, and each
 * reconnect refetches the space's lists so nothing missed while offline
 * stays stale. A 401 ends the subscription; a 403 or 404 refreshes the joined
 * space list and ends it, so a revoked member leaves that space's route.
 * The subscription also ends with the component.
 */
export function useSpaceEvents(tenantId: string | undefined, spaceId: string | undefined): void {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!tenantId || !spaceId) return undefined
    const controller = new AbortController()
    const url = `/api/v1/tenants/${tenantId}/spaces/${spaceId}/events`
    const onEvent = (event: SpaceEvent) => invalidateForEvent(event, tenantId, spaceId, queryClient)
    const refetchAll = () => {
      void queryClient.invalidateQueries({ queryKey: getGetApiV1MeSpacesQueryKey() })
      return queryClient.invalidateQueries({
        predicate: (query) => String(query.queryKey[0]).startsWith(`/api/v1/tenants/${tenantId}/`),
      })
    }
    void (async () => {
      let failures = 0
      while (!controller.signal.aborted) {
        try {
          // oxlint-disable-next-line no-await-in-loop -- one connection at a time is the reconnect loop
          const response = await fetch(url, { signal: controller.signal })
          if (response.status === 401) return
          if (response.status === 403 || response.status === 404) {
            void queryClient.invalidateQueries({ queryKey: getGetApiV1MeSpacesQueryKey() })
            return
          }
          if (!response.ok || !response.body) throw new Error(`event stream ${response.status}`)
          if (failures > 0) void refetchAll()
          failures = 0
          // oxlint-disable-next-line no-await-in-loop -- the stream itself is long-lived
          await drain(response.body, onEvent)
        } catch {
          // aborted on unmount, or the connection dropped; fall through to backoff
        }
        if (controller.signal.aborted) return
        failures += 1
        // oxlint-disable-next-line no-await-in-loop -- backoff between reconnects
        await sleep(reconnectDelay(failures - 1), controller.signal)
      }
    })()
    return () => controller.abort()
  }, [tenantId, spaceId, queryClient])
}
