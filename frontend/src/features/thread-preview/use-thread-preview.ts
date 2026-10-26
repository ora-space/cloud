import { useEffect, useState } from 'react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type { ThreadPreviewSnapshot } from '@/api/generated.schemas'
import {
  getApiV1TenantsTidIssuesIidRunsRidThreadPreview,
  getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey,
} from '@/api/tenants/tenants'
import { drainEventStream, pauseEventStream } from '@/lib/event-stream'
import { adoptThreadPreview, emptyPreview, parseThreadPreview } from './preview'

type PreviewRef = { tid: string; issueId: string; runId: string }
type Connection = 'connecting' | 'live' | 'unavailable' | 'disconnected'

/** Owns a run-scoped optional stream; query cache holds content and React only holds connection UI. */
export function useThreadPreview(ref: PreviewRef, enabled: boolean) {
  const { tid, issueId, runId } = ref
  const queryClient = useQueryClient()
  const [connection, setConnection] = useState<Connection>('connecting')
  const key = ['thread-preview', tid, issueId, runId] as const
  const query = useQuery({
    queryKey: key,
    queryFn: () => getApiV1TenantsTidIssuesIidRunsRidThreadPreview(tid, issueId, runId),
    enabled: false,
    initialData: emptyPreview,
    gcTime: 0,
  })
  useEffect(() => {
    if (!enabled) return undefined
    const controller = new AbortController()
    const queryKey = ['thread-preview', tid, issueId, runId] as const
    let attempts = 0
    const clear = () => queryClient.setQueryData(queryKey, emptyPreview())
    const receive = (data: string) => {
      if (controller.signal.aborted) return
      const next = parseThreadPreview(data)
      if (!next) return
      // Backoff applies to consecutive failures, not to a lifetime of healthy
      // streams that end when their Gateway credentials expire.
      attempts = 0
      setConnection(next.available ? 'live' : 'unavailable')
      queryClient.setQueryData<ThreadPreviewSnapshot>(queryKey, (current) =>
        adoptThreadPreview(current ?? emptyPreview(), next),
      )
    }
    void (async () => {
      while (!controller.signal.aborted) {
        try {
          if (attempts > 0) {
            // oxlint-disable-next-line no-await-in-loop -- restore durable records before each reconnect
            await queryClient.invalidateQueries({
              queryKey: getGetApiV1TenantsTidIssuesIidRunsRidThreadQueryKey(tid, issueId, runId),
            })
          }
          // oxlint-disable-next-line no-await-in-loop -- exactly one open connection belongs to this scope
          const response = await fetch(
            `/api/v1/tenants/${tid}/issues/${issueId}/runs/${runId}/thread/preview/events`,
            { signal: controller.signal },
          )
          if ([401, 403, 404].includes(response.status)) {
            clear()
            setConnection('unavailable')
            return
          }
          if (!response.ok || !response.body) throw new Error('Preview connection unavailable')
          // oxlint-disable-next-line no-await-in-loop -- consume before reconnecting
          await drainEventStream(response.body, receive, controller.signal)
        } catch {
          if (controller.signal.aborted) return
        }
        if (controller.signal.aborted) return
        setConnection('disconnected')
        // oxlint-disable-next-line no-await-in-loop -- cancellation owns the retry timer
        await pauseEventStream(controller.signal, Math.min(1000 * 2 ** attempts, 30000))
        attempts++
      }
    })()
    return () => controller.abort()
  }, [enabled, tid, issueId, runId, queryClient])
  return { snapshot: enabled ? query.data : emptyPreview(), connection }
}
