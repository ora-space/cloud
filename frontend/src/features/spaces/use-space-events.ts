import { useEffect } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { SpaceEvent } from '@/api/generated.schemas'
import { getGetApiV1TenantsTidSpacesQueryKey } from '@/api/spaces/spaces'

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
  return typeof value['type'] === 'string' && typeof value['spaceId'] === 'string'
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
  if (event.type.startsWith('project.')) {
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/projects`],
    })
  } else if (event.type === 'space.member_updated') {
    void queryClient.invalidateQueries({
      queryKey: [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/members`],
    })
  } else {
    void queryClient.invalidateQueries({
      queryKey: getGetApiV1TenantsTidSpacesQueryKey(tenantId),
    })
  }
}

/**
 * Subscribes the tab to the space event stream and invalidates the affected
 * queries on every notice. Events are lightweight: they only trigger
 * refetches against the authoritative REST state, never carry it. The
 * subscription ends with the component. Authentication is supplied by the
 * same-origin HttpOnly Gateway session cookie rather than JavaScript headers.
 */
export function useSpaceEvents(tenantId: string | undefined, spaceId: string | undefined): void {
  const queryClient = useQueryClient()
  useEffect(() => {
    if (!tenantId || !spaceId) return undefined
    const controller = new AbortController()
    void (async () => {
      try {
        const response = await fetch(`/api/v1/tenants/${tenantId}/spaces/${spaceId}/events`, {
          signal: controller.signal,
        })
        if (response.ok && response.body) {
          const reader = response.body.getReader()
          const decoder = new TextDecoder()
          let buffer = ''
          for (;;) {
            // oxlint-disable-next-line no-await-in-loop -- incremental stream, sequential awaits are the protocol
            const { done, value } = await reader.read()
            if (done) break
            buffer += decoder.decode(value, { stream: true })
            const { events, rest } = parseSSEFrames(buffer)
            buffer = rest
            for (const event of events) {
              invalidateForEvent(event, tenantId, spaceId, queryClient)
            }
          }
        }
      } catch {
        // aborted on unmount or the connection dropped; MVP does not reconnect
      }
    })()
    return () => controller.abort()
  }, [tenantId, spaceId, queryClient])
}
