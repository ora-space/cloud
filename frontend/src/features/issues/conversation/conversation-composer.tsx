import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import type { CollaborationTargetSummary, IssueRun } from '@/features/issues/types'
import { taskConversationError } from './api'
import { useComposerSubmit } from './use-composer-submit'
import type { useConversationDraft } from './use-conversation-draft'
import type { TaskAgentReadiness } from '@/api/generated.schemas'
import { AgentReadiness } from './agent-readiness'
import { useState } from 'react'
import { ConversationWorkflow } from './conversation-workflow'

// Catalog identities are type + id; a workflow must not alias an Agent with the same opaque id.
function recipientKey(target: CollaborationTargetSummary) {
  return target.type === 'workflow' ? `workflow:${target.id}` : target.id
}

function TaskRecipient({
  targets,
  agentId,
  busy,
  mode,
  select,
  toggle,
}: {
  targets: CollaborationTargetSummary[]
  agentId: string
  busy: boolean
  mode: 'agent' | 'comment'
  select: (id: string) => void
  toggle: () => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      <label htmlFor="task-conversation-agent" className="font-medium">
        发送给
      </label>
      <select
        id="task-conversation-agent"
        value={
          targets.some((target) => target.type === 'agent' && target.id === agentId) ? agentId : ''
        }
        disabled={mode === 'comment' || busy}
        onChange={(event) => select(event.target.value)}
        className="h-9 min-w-36 rounded-md border bg-background px-2"
      >
        <option value="">选择 Agent 或工作流</option>
        {targets.map((target) => (
          <option key={`${target.type}:${target.id}`} value={recipientKey(target)}>
            {target.displayName} · {target.type === 'workflow' ? '工作流' : 'Agent'}
          </option>
        ))}
      </select>
      <Button
        type="button"
        size="sm"
        variant={mode === 'comment' ? 'secondary' : 'ghost'}
        aria-pressed={mode === 'comment'}
        onClick={toggle}
      >
        仅评论
      </Button>
    </div>
  )
}

function ComposerFeedback({
  handoff,
  agentName,
  agentMissing,
  sendError,
  commentError,
}: {
  handoff: boolean
  agentName: string
  agentMissing: boolean
  sendError: unknown
  commentError: unknown
}) {
  return (
    <>
      {handoff && (
        <p className="text-xs text-muted-foreground">
          将等待当前 Agent 完成、保存成果并释放工作区后交接给 {agentName}。
        </p>
      )}
      {agentMissing && (
        <p className="text-xs text-muted-foreground">
          先选择可用 Agent；切换目标本身不会启动执行。
        </p>
      )}
      {sendError && (
        <p role="alert" className="text-xs text-destructive">
          {taskConversationError(sendError)}
        </p>
      )}
      {commentError && (
        <p role="alert" className="text-xs text-destructive">
          评论发送失败，请重试。
        </p>
      )}
    </>
  )
}

function waitingForHandoff(
  mode: 'agent' | 'comment',
  agent: CollaborationTargetSummary | undefined,
  latest: IssueRun | undefined,
): boolean {
  return (
    mode === 'agent' &&
    !!agent &&
    !!latest &&
    latest.executorId !== agent.id &&
    ['queued', 'dispatched', 'running'].includes(latest.status)
  )
}

function mentionAtEnd(draft: string): { start: number; query: string } | null {
  const match = /(^|\s)@([\p{L}\p{N}_-]*)$/u.exec(draft)
  if (!match) return null
  return { start: match.index + (match[1]?.length ?? 0), query: match[2] ?? '' }
}

function MentionPicker({
  targets,
  draft,
  choose,
}: {
  targets: CollaborationTargetSummary[]
  draft: string
  choose: (agentId: string, start: number) => void
}) {
  const mention = mentionAtEnd(draft)
  if (!mention) return null
  const matches = targets.filter((target) =>
    target.displayName.toLocaleLowerCase().includes(mention.query.toLocaleLowerCase()),
  )
  if (!matches.length) return null
  return (
    <div aria-label="选择提及的目标" className="flex flex-wrap gap-1 rounded-md border p-2">
      {matches.map((target) => (
        <Button
          key={`${target.type}:${target.id}`}
          type="button"
          size="xs"
          variant="ghost"
          onClick={() => choose(recipientKey(target), mention.start)}
        >
          {target.displayName} · {target.type === 'workflow' ? '工作流' : 'Agent'}
        </Button>
      ))}
    </div>
  )
}

type ComposerProps = {
  tid: string
  issueId: string
  targets: CollaborationTargetSummary[]
  latestRun: IssueRun | undefined
  state: ReturnType<typeof useConversationDraft>
  onSent: () => void
  readiness: TaskAgentReadiness[] | undefined
  settingsPath: string | undefined
  pluginsPath?: string | undefined
}

/** Routes explicit selection to Agent messages or Workflow review while retaining the task draft. */
export function ConversationComposer(props: ComposerProps) {
  const [workflow, setWorkflow] = useState<{ tid: string; issueId: string; targetId: string }>()
  const workflowId =
    workflow?.tid === props.tid && workflow.issueId === props.issueId
      ? workflow.targetId
      : undefined
  return (
    <>
      <div hidden={!!workflowId}>
        <AgentConversationComposer
          {...props}
          onWorkflow={(targetId) =>
            setWorkflow({ tid: props.tid, issueId: props.issueId, targetId })
          }
        />
      </div>
      {workflowId && (
        <ConversationWorkflow
          key={`${props.tid}:${props.issueId}:${workflowId}`}
          tid={props.tid}
          issueId={props.issueId}
          target={props.targets.find(
            (target) => target.type === 'workflow' && target.id === workflowId,
          )}
          onRemove={() => setWorkflow(undefined)}
          onConfirmed={() => {
            setWorkflow(undefined)
            props.onSent()
          }}
        />
      )}
    </>
  )
}

function AgentConversationComposer({
  tid,
  issueId,
  targets,
  latestRun,
  state,
  onSent,
  readiness,
  settingsPath,
  pluginsPath,
  onWorkflow,
}: ComposerProps & { onWorkflow: (id: string) => void }) {
  const { mode, setMode, selected, setSelected, draft, setDraft, input } = state
  const recipients = targets.filter(
    (target) => target.type === 'agent' || target.type === 'workflow',
  )
  const agents = targets.filter((target) => target.type === 'agent')
  const agentId = selected ?? latestRun?.executorId ?? ''
  const agent = agents.find((target) => target.id === agentId)
  const handoff = waitingForHandoff(mode, agent, latestRun)
  const recipient = readiness?.find((item) => item.agentId === agentId)
  const submission = useComposerSubmit({
    tid,
    issueId,
    draft,
    mode,
    agent,
    readiness: recipient,
    clearDraft: () => setDraft(''),
    onSent,
  })
  const { busy, canSubmit, submit } = submission

  function choose(targetId: string) {
    const target = recipients.find((candidate) => recipientKey(candidate) === targetId)
    if (target?.type === 'workflow') onWorkflow(target.id)
    else setSelected(targetId)
  }

  let buttonLabel = '发送'
  if (busy) buttonLabel = '发送中…'
  else if (mode === 'comment') buttonLabel = '发表评论'

  return (
    <form onSubmit={submit} className="space-y-2">
      <TaskRecipient
        targets={recipients}
        agentId={agentId}
        busy={busy}
        mode={mode}
        select={choose}
        toggle={() => setMode((current) => (current === 'agent' ? 'comment' : 'agent'))}
      />
      <Textarea
        ref={input}
        aria-label="任务对话输入"
        value={draft}
        onChange={(event) => setDraft(event.target.value)}
        placeholder={mode === 'comment' ? '写下任务评论…' : '给 Agent 发送消息…'}
        rows={3}
        className="max-h-40 resize-none [field-sizing:content]"
        disabled={busy}
      />
      {mode === 'agent' && (
        <MentionPicker
          targets={recipients}
          draft={draft}
          choose={(targetId, start) => {
            choose(targetId)
            setDraft(draft.slice(0, start).trimEnd())
            input.current?.focus()
          }}
        />
      )}
      <ComposerFeedback
        handoff={handoff}
        agentName={agent?.displayName ?? 'Agent'}
        agentMissing={mode === 'agent' && !agent}
        sendError={submission.sendError}
        commentError={submission.commentError}
      />
      {mode === 'agent' && (
        <AgentReadiness
          readiness={recipient}
          settingsPath={settingsPath}
          pluginsPath={pluginsPath}
        />
      )}
      <div className="flex justify-end">
        <Button type="submit" disabled={!canSubmit}>
          {buttonLabel}
        </Button>
      </div>
    </form>
  )
}
