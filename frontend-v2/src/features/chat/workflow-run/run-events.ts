import { useEffect, useRef } from 'react'

/**
 * Follows a run's `workflow_run_invalidated` stream while `active`, as desktop's app events do: each
 * event calls `invalidated`, which reads the run again, since the event carries no state of its own.
 * `connected` reports whether the stream is open, so polling can slow down to a fallback meanwhile
 * and speed up again when it drops; the browser's EventSource reconnects by itself.
 */
export function useRunInvalidations(
  tenantId: string,
  runId: string,
  active: boolean,
  on: { invalidated: () => void; connected: (open: boolean) => void },
): void {
  const handlers = useRef(on)
  useEffect(() => {
    handlers.current = on
  })
  useEffect(() => {
    if (!active || tenantId === '') return
    const source = new EventSource(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}/events`)
    source.onopen = () => handlers.current.connected(true)
    source.onerror = () => handlers.current.connected(false)
    source.addEventListener('workflow_run_invalidated', () => handlers.current.invalidated())
    return () => {
      source.close()
      handlers.current.connected(false)
    }
  }, [tenantId, runId, active])
}
