import { useEffect, useMemo, useState } from 'react'
import type { ChatEvent } from '../api/generated'
import { applySessionEvent, conversationOf, emptySessionView, type SessionView } from '../model/history'
import type { Conversation } from '../model/types'

/**
 * Follows one node attempt's session over v2-server's node events stream and folds it as the chat
 * page folds a conversation's, so the run page shows the Agent's whole turn: its thinking, tool
 * calls and answer, live while it runs. Returns null until the first event arrives, and stays null
 * when the stream is refused (an attempt not dispatched yet, or a session that belongs to whoever
 * else started the run), so the page keeps showing what the attempt recorded.
 */
export function useNodeSession(tenantId: string, runId: string | undefined, nodeRunId: string | undefined): Conversation | null {
  const [view, setView] = useState<SessionView | null>(null)
  useEffect(() => {
    if (tenantId === '' || runId === undefined || nodeRunId === undefined) return
    let current = emptySessionView(nodeRunId)
    const source = new EventSource(`/api/v2/tenants/${tenantId}/workflow-runs/${runId}/nodes/${nodeRunId}/events`)
    source.onmessage = (m: MessageEvent<string>) => {
      current = applySessionEvent(current, JSON.parse(m.data) as ChatEvent)
      setView(current)
    }
    // The session has sent everything it ever will, or cannot be read on: stop reconnecting.
    source.addEventListener('end', () => source.close())
    source.addEventListener('failure', () => source.close())
    return () => {
      source.close()
      setView(null)
    }
  }, [tenantId, runId, nodeRunId])
  return useMemo(() => {
    if (view === null) return null
    // The execution bridge ends a node's session after its one turn: the chat page's marker that a
    // conversation cannot continue says nothing here.
    const conversation = conversationOf([view])
    return { ...conversation, markers: conversation.markers.filter((marker) => marker.kind !== 'session') }
  }, [view])
}
