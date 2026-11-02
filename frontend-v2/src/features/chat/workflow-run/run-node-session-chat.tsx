import { useMemo, type ReactNode } from 'react'
import type { GraphWorkflowNodeStatus, WorkflowNodeConversationItem } from '@/lib/workflows/runtime'
import { Thread } from '../components/thread'
import { useChatStore } from '../store'
import { nodeConversationTurns } from './conversation-turns'
import { useNodeSession } from './node-session'

// A node session has nothing to ask permission for, so answering is never offered.
const noAnswer = () => {}

/**
 * An Agent node's conversation, drawn by the chat page's own thread: the same prompts, answers,
 * thinking and tool rows as a chat session. As desktop shows the node's real session here, the
 * attempt's session is followed over v2-server's node events stream; until it can be read, or when
 * it cannot, the task and answer the run recorded stand in.
 */
export function RunNodeSessionChat({
  conversation,
  status,
  errorMessage,
  sessionActions,
  session,
}: {
  conversation: WorkflowNodeConversationItem[]
  status: GraphWorkflowNodeStatus
  errorMessage?: string
  sessionActions?: ReactNode
  /** The attempt whose session to follow; absent before it was dispatched. */
  session?: { runId: string; nodeRunId: string }
}) {
  const tenantId = useChatStore((s) => s.tenantId) ?? ''
  const followed = useNodeSession(tenantId, session?.runId, session?.nodeRunId)
  const recorded = useMemo(() => nodeConversationTurns(conversation, status, errorMessage), [conversation, status, errorMessage])
  const thread = followed !== null && followed.turns.length > 0 ? followed : recorded
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex min-h-0 flex-1 flex-col">
        {thread.turns.length > 0 ? (
          <Thread conversation={thread} onAnswer={noAnswer} />
        ) : (
          <p className="flex flex-1 items-center justify-center px-4 text-center text-sm text-muted-foreground">正在等待 Agent 的会话…</p>
        )}
      </div>
      {sessionActions && <div className="flex shrink-0 items-center pt-1">{sessionActions}</div>}
    </div>
  )
}
