import { Button } from '@/components/ui/button'
import { WorkflowInteractionComposer } from '@/features/issues/components/workflow-interaction-composer'
import type { CollaborationTargetSummary } from '@/features/issues/types'
import { useQueryClient } from '@tanstack/react-query'
import { taskConversationKey } from './api'

/** Opens the existing explicit review/confirm flow without submitting or discarding the Agent draft. */
export function ConversationWorkflow({
  tid,
  issueId,
  target,
  onRemove,
  onConfirmed,
}: {
  tid: string
  issueId: string
  target: CollaborationTargetSummary | undefined
  onRemove: () => void
  onConfirmed: () => void
}) {
  const queryClient = useQueryClient()
  if (!target?.interactionDescriptor.formRef) {
    return (
      <div className="space-y-2">
        <p role="alert">该工作流已不可用，或尚未提供可用表单。</p>
        <Button type="button" variant="outline" onClick={onRemove}>
          返回任务输入
        </Button>
      </div>
    )
  }
  return (
    <WorkflowInteractionComposer
      slug={tid}
      issueId={issueId}
      target={target}
      onRemove={onRemove}
      onConfirmed={() => {
        void queryClient.invalidateQueries({ queryKey: taskConversationKey(tid, issueId) })
        onConfirmed()
      }}
    />
  )
}
