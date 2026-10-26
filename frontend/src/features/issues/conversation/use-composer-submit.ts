import { useCreateComment } from '@/features/issues/api'
import { useSendTaskMessage } from './api'
import type { TaskAgentReadiness } from '@/api/generated.schemas'
import type { CollaborationTargetSummary } from '@/features/issues/types'

/** Owns one explicit send workflow while preserving the draft on refused or unknown outcomes. */
export function useComposerSubmit({
  tid,
  issueId,
  draft,
  mode,
  agent,
  readiness,
  clearDraft,
  onSent,
}: {
  tid: string
  issueId: string
  draft: string
  mode: 'agent' | 'comment'
  agent: CollaborationTargetSummary | undefined
  readiness: TaskAgentReadiness | undefined
  clearDraft: () => void
  onSent: () => void
}) {
  const send = useSendTaskMessage(tid, issueId)
  const comment = useCreateComment(tid, issueId)
  const busy = send.isPending || comment.isPending
  const agentId = readiness?.errorCode ? null : (agent?.id ?? null)
  const canSubmit = !!draft.trim() && !busy && (mode === 'comment' || !!agentId)
  function submit(event: React.FormEvent) {
    event.preventDefault()
    if (!canSubmit) return
    const complete = () => {
      clearDraft()
      onSent()
    }
    if (mode === 'comment') comment.mutate({ body: draft }, { onSuccess: complete })
    else if (agentId) send.mutate({ agentId, body: draft }, { onSuccess: complete })
  }
  return { busy, canSubmit, submit, sendError: send.error, commentError: comment.error }
}
