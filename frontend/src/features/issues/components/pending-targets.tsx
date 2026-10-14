import { X } from 'lucide-react'
import { useState } from 'react'
import { ActorAvatar } from '@/components/common/actor-avatar'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { WorkflowInteractionComposer } from '@/features/issues/components/workflow-interaction-composer'
import { AgentRequestFailure } from '@/features/issues/components/agent-request-failure'
import type { CollaborationTargetSummary, CommentTargetInput } from '@/features/issues/types'

/** The inline message a target carries: a task for agent/team, a plain note for a human mention. */
function messagePlaceholder(type: CommentTargetInput['type']): string {
  if (type === 'agent' || type === 'team') return '这个目标要做什么…'
  return '给对方的留言…'
}

function avatarActor(target: CommentTargetInput, meta: CollaborationTargetSummary | undefined) {
  const name = meta?.displayName ?? target.id
  if (target.type === 'agent') return { name, type: 'agent' as const }
  if (target.type === 'team') return { name, type: 'team' as const }
  return { name, type: 'user' as const }
}

/**
 * One pending target: its own message box and its own submit button. Nothing is recorded — no comment,
 * no interaction, no run — until the user presses 提交, so an abandoned row leaves no trace.
 */
function PendingTargetRow({
  target,
  meta,
  onSubmit,
  onRemove,
}: {
  target: CommentTargetInput
  meta: CollaborationTargetSummary | undefined
  onSubmit: (message: string) => Promise<void>
  onRemove: () => void
}) {
  const [message, setMessage] = useState('')
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<unknown>(null)
  const displayName = meta?.displayName ?? target.id

  async function submit() {
    setBusy(true)
    setFailure(null)
    try {
      await onSubmit(message.trim())
    } catch (error) {
      setFailure(error)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2 rounded-md border px-2 py-1.5 text-sm">
        <ActorAvatar actor={avatarActor(target, meta)} size="sm" />
        <span className="shrink-0 font-medium">{displayName}</span>
        <Input
          value={message}
          onChange={(event) => setMessage(event.target.value)}
          placeholder={messagePlaceholder(target.type)}
          className="h-7 min-w-0 flex-1"
          aria-label={`${displayName} 的留言`}
        />
        <Button
          type="button"
          size="sm"
          disabled={message.trim() === '' || busy}
          onClick={() => {
            void submit()
          }}
        >
          {busy ? '提交中…' : '提交'}
        </Button>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          className="size-6 shrink-0"
          onClick={onRemove}
          aria-label={`移除 ${displayName}`}
        >
          <X className="size-3.5" />
        </Button>
      </div>
      {failure !== null && <AgentRequestFailure error={failure} />}
    </div>
  )
}

/**
 * The targets the user has selected but not submitted. A human mention, an agent task and a team task
 * each carry their own message and their own submit button; a Workflow opens its configuration panel
 * and is committed by 确认执行. Nothing here touches the server until one of those buttons is pressed.
 */
export function PendingTargets({
  slug,
  issueId,
  targets,
  catalog,
  onChange,
  onSubmit,
}: {
  slug: string
  issueId: string
  targets: CommentTargetInput[]
  catalog: CollaborationTargetSummary[]
  onChange: (targets: CommentTargetInput[]) => void
  onSubmit: (target: CommentTargetInput, message: string) => Promise<void>
}) {
  const byId = new Map(catalog.map((target) => [target.id, target]))
  if (targets.length === 0) return null

  function remove(id: string) {
    onChange(targets.filter((target) => target.id !== id))
  }

  return (
    <div className="space-y-1.5">
      {targets.map((target) => {
        const meta = byId.get(target.id)
        if (target.type === 'workflow' && meta?.interactionDescriptor.formRef) {
          return (
            <WorkflowInteractionComposer
              key={target.id}
              slug={slug}
              issueId={issueId}
              target={meta}
              onConfirmed={() => remove(target.id)}
              onRemove={() => remove(target.id)}
            />
          )
        }
        return (
          <PendingTargetRow
            key={target.id}
            target={target}
            meta={meta}
            onSubmit={(message) => onSubmit(target, message)}
            onRemove={() => remove(target.id)}
          />
        )
      })}
    </div>
  )
}
