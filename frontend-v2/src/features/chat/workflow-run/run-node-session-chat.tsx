import { useMemo, type ReactNode } from 'react'
import type { GraphWorkflowNodeStatus, WorkflowNodeConversationItem } from '@/lib/workflows/runtime'
import { Thread } from '../components/thread'
import { nodeConversationTurns } from './conversation-turns'

// A node session has nothing to ask permission for, so answering is never offered.
const noAnswer = () => {}

/**
 * An Agent node's conversation, drawn by the chat page's own thread: the same prompts, answers,
 * thinking and tool rows as a chat session. Desktop shows the node's live session here through its
 * chat view; v2-server returns only the conversation items, which are read as chat turns.
 */
export function RunNodeSessionChat({
  conversation,
  status,
  errorMessage,
  sessionActions,
}: {
  conversation: WorkflowNodeConversationItem[]
  status: GraphWorkflowNodeStatus
  errorMessage?: string
  sessionActions?: ReactNode
}) {
  const thread = useMemo(() => nodeConversationTurns(conversation, status, errorMessage), [conversation, status, errorMessage])
  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="flex min-h-0 flex-1 flex-col">
        <Thread conversation={thread} onAnswer={noAnswer} />
      </div>
      {sessionActions && <div className="flex shrink-0 items-center pt-1">{sessionActions}</div>}
    </div>
  )
}
