import { useState } from 'react'
import { ConversationViewport } from '@/features/issues/thread/conversation-viewport'
import { AgentSpeaker, MessageAuthor } from '@/features/issues/thread/message-author'
import { useThread } from '@/features/issues/thread/thread-api'
import { isUnauthorizedError, isForbiddenError } from '@/lib/api-client'
import { memberNameById } from '@/features/issues/present'
import { Link } from 'react-router-dom'
import type { TaskConversationMessage } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { ConversationMarkdown } from '@/features/conversation-content/markdown'
import { useTimeline } from '@/features/issues/api'
import { IssueThreadPanel } from '@/features/issues/thread/thread-panel'
import type {
  CollaborationTargetSummary,
  IssueRun,
  TenantMember,
  TimelineEntry,
} from '@/features/issues/types'
import { workspacePaths } from '@/lib/paths'
import { faultCode } from '@/lib/api-client'
import {
  loadedTaskMessages,
  taskConversationError,
  taskConversationFailureCode,
  useTaskConversation,
  useUpdateTaskMessage,
} from './api'
import { ConversationFooter } from './conversation-footer'
import { useConversationDraft, type ConversationQuote } from './use-conversation-draft'
import { WorkflowRunCard } from './workflow-run-card'

type ConversationRow =
  | { kind: 'run'; at: string; id: string; run: IssueRun }
  | { kind: 'message'; at: string; id: string; message: TaskConversationMessage }
  | { kind: 'comment'; at: string; id: string; comment: TimelineEntry }

const RUN_LABELS: Record<string, string> = {
  queued: '排队中',
  dispatched: '准备中',
  running: '执行中',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

function RunContext({ input }: { input: IssueRun['input'] }) {
  const raw = input['conversationHistory']
  const history: Record<string, unknown>[] = Array.isArray(raw)
    ? raw.filter(
        (item): item is Record<string, unknown> =>
          typeof item === 'object' && item !== null && typeof item['id'] === 'string',
      )
    : []
  const incomplete = input['conversationHistoryIncomplete'] === true
  if (!history.length && !incomplete) return null
  return (
    <details className="rounded-lg border border-dashed p-3 text-xs">
      <summary className="cursor-pointer text-muted-foreground">
        本次带入历史：{history.length} 轮{incomplete ? ' · 部分旧记录未带入' : ''}
      </summary>
      <ol className="mt-3 space-y-3">
        {history.map((item) => (
          <li key={String(item['id'])} className="rounded-md bg-muted/40 p-2">
            {typeof item['user'] === 'string' && <p>用户：{item['user']}</p>}
            {typeof item['assistant'] === 'string' && <p>Agent：{item['assistant']}</p>}
            {item['assistantTruncated'] === true && (
              <p className="text-muted-foreground">此轮回复已截断</p>
            )}
          </li>
        ))}
      </ol>
    </details>
  )
}

function conversationRows(
  runs: IssueRun[],
  messages: TaskConversationMessage[],
  timeline: TimelineEntry[],
): ConversationRow[] {
  const triggered = new Set(runs.map((run) => run.triggerEvidenceRefId).filter(Boolean))
  const indexed = new Set(runs.map((run) => run.id))
  const rows: ConversationRow[] = [
    ...runs.map((run): ConversationRow => ({ kind: 'run', at: run.createdAt, id: run.id, run })),
    ...messages
      .filter((message) => !message.runId || !indexed.has(message.runId))
      .map((message): ConversationRow => ({
        kind: 'message',
        at: message.createdAt,
        id: message.id,
        message,
      })),
    ...timeline
      .filter((entry) => entry.kind === 'comment' && entry.body && !triggered.has(entry.id))
      .map((comment): ConversationRow => ({
        kind: 'comment',
        at: comment.createdAt,
        id: comment.id,
        comment,
      })),
  ]
  return rows.toSorted((a, b) => a.at.localeCompare(b.at) || a.id.localeCompare(b.id))
}

function visibleAgentRuns(
  runs: IssueRun[],
  count: number,
  activeRunId: string | null | undefined,
): IssueRun[] {
  const recent = runs.slice(-count)
  const active = runs.find((run) => run.id === activeRunId)
  return active && !recent.some((run) => run.id === active.id) ? [active, ...recent] : recent
}

function WaitingMessage({
  message,
  name,
  author,
  own,
  tid,
  issueId,
  onQuote,
}: {
  message: TaskConversationMessage
  name: string
  author: string
  own: boolean
  tid: string
  issueId: string
  onQuote: (quote: ConversationQuote) => void
}) {
  const action = useUpdateTaskMessage(tid, issueId)
  const label = {
    waiting: '等待执行或交接',
    submitted: '已提交执行',
    cancelled: '已取消',
    failed: '未能启动',
  }[message.state]
  return (
    <li data-message-anchor data-conversation-output={message.id} className="space-y-2 py-3">
      <div className="flex items-center justify-between gap-2 text-sm">
        <span className="font-medium">
          {author} → {name}
        </span>
        <span className="text-xs text-muted-foreground">{label}</span>
      </div>
      <div className="mt-2">
        <ConversationMarkdown text={message.body} />
      </div>
      {message.errorCode && (
        <p role="alert" className="mt-2 text-xs text-destructive">
          {taskConversationFailureCode(message.errorCode)}
        </p>
      )}
      <div className="mt-2 flex gap-2">
        {own && message.state === 'waiting' && (
          <Button
            variant="ghost"
            size="xs"
            disabled={action.isPending}
            onClick={() => action.mutate({ id: message.id, action: 'cancel' })}
          >
            取消等待
          </Button>
        )}
        {own && message.state === 'failed' && (
          <Button
            variant="ghost"
            size="xs"
            disabled={action.isPending}
            onClick={() => action.mutate({ id: message.id, action: 'retry' })}
          >
            重试
          </Button>
        )}
        <Button
          variant="ghost"
          size="xs"
          onClick={() => onQuote({ text: message.body, agentId: message.agentId })}
        >
          引用回复
        </Button>
      </div>
      {action.isError && (
        <p role="alert" className="text-xs text-destructive">
          {taskConversationError(action.error)}
        </p>
      )}
    </li>
  )
}

function useRunAuthor(
  { tid, issueId, run }: { tid: string; issueId: string; run: IssueRun },
  members: TenantMember[],
  currentUserId: string | undefined,
) {
  const thread = useThread(
    { tid, issueId, runId: run.id },
    ['completed', 'failed', 'cancelled'].includes(run.status),
  )
  const snapshot = thread.isError ? undefined : thread.data
  const task = useTaskConversation(tid, issueId)
  const message = loadedTaskMessages(task.data?.pages).find((item) => item.runId === run.id)
  const id = message?.userId ?? (snapshot?.declared ? snapshot.initiatorUserId : undefined)
  if (id && id === currentUserId) return '你'
  return memberNameById(members).get(id ?? '') ?? '用户'
}

function RunConversation({
  run,
  name,
  tid,
  issueId,
  members,
  workspaceSlug,
  currentUserId,
  onQuote,
}: {
  run: IssueRun
  name: string
  tid: string
  issueId: string
  members: TenantMember[]
  workspaceSlug: string | undefined
  currentUserId: string | undefined
  onQuote: (quote: ConversationQuote) => void
}) {
  const task = run.input['task']
  const author = useRunAuthor({ tid, issueId, run }, members, currentUserId)
  return (
    <li
      data-message-anchor
      data-conversation-output={run.id}
      className="min-w-0 space-y-3 border-t pt-5"
    >
      <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
        <span className="font-medium">
          {name} · {RUN_LABELS[run.status] ?? '状态未知'}
        </span>
        {workspaceSlug && (
          <Link
            className="text-xs text-muted-foreground underline"
            to={workspacePaths(workspaceSlug).agentSession(issueId, run.id)}
          >
            完整执行记录
          </Link>
        )}
      </div>
      {typeof task === 'string' && (
        <div data-message-anchor className="rounded-lg bg-muted/40 p-3">
          <MessageAuthor name={author} at={run.createdAt} />
          <ConversationMarkdown text={task} />
          <Button
            variant="ghost"
            size="xs"
            onClick={() => onQuote({ text: task, agentId: run.executorId })}
          >
            引用回复
          </Button>
        </div>
      )}
      <RunContext input={run.input} />
      <AgentSpeaker name={name}>
        <IssueThreadPanel
          tid={tid}
          issueId={issueId}
          members={members}
          selectedRunId={run.id}
          readOnly
          showEndControl={false}
          endActorId={currentUserId}
          onQuote={(text, agentId) => onQuote({ text, agentId })}
        />
      </AgentSpeaker>
    </li>
  )
}

function ConversationRowItem({
  row,
  targets,
  tid,
  issueId,
  members,
  workspaceSlug,
  currentUserId,
  onQuote,
}: {
  row: ConversationRow
  targets: CollaborationTargetSummary[]
  tid: string
  issueId: string
  members: TenantMember[]
  workspaceSlug: string | undefined
  currentUserId: string | undefined
  onQuote: (quote: ConversationQuote) => void
}) {
  const name = (id: string) =>
    targets.find((target) => target.type === 'agent' && target.id === id)?.displayName ?? 'Agent'
  if (row.kind === 'run' && row.run.executorType === 'workflow')
    return (
      <li data-message-anchor>
        <WorkflowRunCard tid={tid} issueId={issueId} run={row.run} workspaceSlug={workspaceSlug} />
      </li>
    )
  if (row.kind === 'run')
    return (
      <RunConversation
        run={row.run}
        name={name(row.run.executorId)}
        tid={tid}
        issueId={issueId}
        members={members}
        workspaceSlug={workspaceSlug}
        currentUserId={currentUserId}
        onQuote={onQuote}
      />
    )
  if (row.kind === 'message')
    return (
      <WaitingMessage
        author={
          row.message.userId === currentUserId
            ? '你'
            : (memberNameById(members).get(row.message.userId) ?? '用户')
        }
        message={row.message}
        name={name(row.message.agentId)}
        own={row.message.userId === currentUserId}
        tid={tid}
        issueId={issueId}
        onQuote={onQuote}
      />
    )
  return (
    <li data-message-anchor data-conversation-output={row.id} className="space-y-2 py-3">
      <MessageAuthor
        name={
          row.comment.authorUserId === currentUserId
            ? '你'
            : (memberNameById(members).get(row.comment.authorUserId ?? '') ??
              row.comment.authorType)
        }
        type={row.comment.authorType === 'agent' ? 'agent' : 'user'}
        at={row.comment.createdAt}
      />
      <ConversationMarkdown text={row.comment.body ?? ''} />
    </li>
  )
}

function ConversationRows({
  rows,
  tid,
  issueId,
  members,
  targets,
  workspaceSlug,
  currentUserId,
  onQuote,
}: {
  rows: ConversationRow[]
  tid: string
  issueId: string
  members: TenantMember[]
  targets: CollaborationTargetSummary[]
  workspaceSlug: string | undefined
  currentUserId: string | undefined
  onQuote: (quote: ConversationQuote) => void
}) {
  return (
    <ol aria-label="任务对话" className="space-y-5">
      {rows.map((row) => (
        <ConversationRowItem
          key={`${row.kind}:${row.id}`}
          row={row}
          targets={targets}
          tid={tid}
          issueId={issueId}
          members={members}
          workspaceSlug={workspaceSlug}
          currentUserId={currentUserId}
          onQuote={onQuote}
        />
      ))}
    </ol>
  )
}

function OlderHistoryControls({
  olderRuns,
  loadRuns,
  conversation,
}: {
  olderRuns: boolean
  loadRuns: () => void
  conversation: ReturnType<typeof useTaskConversation>
}) {
  return (
    <>
      {olderRuns && (
        <Button variant="ghost" className="w-full" onClick={loadRuns}>
          加载更早的执行记录
        </Button>
      )}
      {conversation.hasNextPage && (
        <Button
          variant="ghost"
          className="w-full"
          disabled={conversation.isFetchingNextPage}
          onClick={() => void conversation.fetchNextPage()}
        >
          加载更早的任务消息
        </Button>
      )}
    </>
  )
}

function ConversationFeedback({
  empty,
  pending,
  failed,
}: {
  empty: boolean
  pending: boolean
  failed: boolean
}) {
  return (
    <>
      {empty && !pending && (
        <p className="rounded-xl border border-dashed p-8 text-center text-sm text-muted-foreground">
          还没有对话。选择 Agent 发送消息，或选择工作流配置并确认执行。
        </p>
      )}
      {failed && (
        <p role="alert" className="text-sm text-destructive">
          任务消息加载失败，请重试。
        </p>
      )}
    </>
  )
}

function useTaskDiscussion(tid: string, issueId: string, runs: IssueRun[]) {
  const draft = useConversationDraft()
  const agents = runs
    .filter((run) => run.executorType === 'agent')
    .toSorted((a, b) => a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id))
  const selectedAgentId =
    draft.mode === 'agent' ? (draft.selected ?? agents.at(-1)?.executorId) : undefined
  const executions = runs
    .filter((run) => run.executorType === 'agent' || run.executorType === 'workflow')
    .toSorted((a, b) => a.createdAt.localeCompare(b.createdAt) || a.id.localeCompare(b.id))
  const conversation = useTaskConversation(tid, issueId, selectedAgentId)
  const timeline = useTimeline(tid, issueId)
  const [visibleRuns, setVisibleRuns] = useState(5)
  const current = conversation.data?.pages[0]
  const shown = visibleAgentRuns(executions, visibleRuns, current?.activeRunId)
  const messages = loadedTaskMessages(conversation.data?.pages)
  const rows = conversationRows(shown, messages, timeline.data ?? [])
  return { conversation, draft, agents, executions, current, shown, messages, rows, setVisibleRuns }
}

function HistoricalExecution(props: {
  tid: string
  issueId: string
  members: TenantMember[]
  runs: IssueRun[]
  selectedRunId: string
  workspaceSlug: string | undefined
  currentUserId: string | undefined
  onQuote: (quote: ConversationQuote) => void
}) {
  const run = props.runs.find((candidate) => candidate.id === props.selectedRunId)
  if (!run) return <p role="alert">该执行记录不存在或已不可访问。</p>
  if (run.executorType === 'workflow')
    return (
      <WorkflowRunCard
        tid={props.tid}
        issueId={props.issueId}
        run={run}
        workspaceSlug={props.workspaceSlug}
      />
    )
  if (run.executorType !== 'agent') return <p>请从执行记录查看此类型的运行。</p>
  return (
    <IssueThreadPanel
      tid={props.tid}
      issueId={props.issueId}
      members={props.members}
      selectedRunId={run.id}
      readOnly
      showEndControl={false}
      endActorId={props.currentUserId}
      onQuote={(text, agentId) => props.onQuote({ text, agentId })}
    />
  )
}

type TaskConversationPanelProps = {
  tid: string
  issueId: string
  members: TenantMember[]
  targets: CollaborationTargetSummary[]
  runs: IssueRun[]
  selectedRunId?: string | undefined
  workspaceSlug?: string | undefined
  currentUserId?: string | undefined
  content?: React.ReactNode
  onLatest: () => void
}

/** Renders one durable task discussion across Run boundaries while leaving each Thread immutable. */
export function TaskConversationPanel({
  tid,
  issueId,
  members,
  targets,
  runs,
  selectedRunId,
  workspaceSlug,
  currentUserId,
  onLatest,
  content,
}: TaskConversationPanelProps) {
  const {
    conversation,
    draft,
    agents,
    executions,
    current,
    shown,
    messages,
    rows,
    setVisibleRuns,
  } = useTaskDiscussion(tid, issueId, runs)
  if (isUnauthorizedError(conversation.error) || isForbiddenError(conversation.error))
    return <p role="alert">任务对话不可访问，请重新确认权限。</p>
  // During a rolling upgrade an older Cloud may not have the task endpoint yet. Preserve its
  // original Thread composer until the authoritative task queue becomes available.
  if (conversation.isError && faultCode(conversation.error) === 'not_found') {
    return (
      <LegacyConversation
        tid={tid}
        issueId={issueId}
        members={members}
        selectedRunId={selectedRunId}
      />
    )
  }
  return (
    <div className="flex h-full min-h-0 flex-col">
      <ConversationViewport
        key={selectedRunId ?? (content ? 'details' : 'conversation')}
        followOutput={content === undefined}
      >
        {content ??
          (selectedRunId ? (
            <HistoricalExecution
              tid={tid}
              issueId={issueId}
              members={members}
              selectedRunId={selectedRunId}
              runs={runs}
              workspaceSlug={workspaceSlug}
              currentUserId={currentUserId}
              onQuote={draft.quote}
            />
          ) : (
            <>
              <OlderHistoryControls
                olderRuns={executions.length > shown.length}
                loadRuns={() => setVisibleRuns((count) => count + 5)}
                conversation={conversation}
              />
              <ConversationRows
                rows={rows}
                tid={tid}
                issueId={issueId}
                members={members}
                targets={targets}
                workspaceSlug={workspaceSlug}
                currentUserId={currentUserId}
                onQuote={draft.quote}
              />
              <ConversationFeedback
                empty={rows.length === 0}
                pending={conversation.isPending}
                failed={conversation.isError}
              />
            </>
          ))}
      </ConversationViewport>
      <ConversationFooter
        tid={tid}
        issueId={issueId}
        targets={targets}
        runs={agents}
        current={current}
        draft={draft}
        waiting={messages.some((message) => message.state === 'waiting')}
        selectedRunId={selectedRunId}
        currentUserId={currentUserId}
        workspaceSlug={workspaceSlug}
        onLatest={onLatest}
      />
    </div>
  )
}

function LegacyConversation(props: React.ComponentProps<typeof IssueThreadPanel>) {
  return (
    <div className="min-h-0 flex-1 overflow-y-auto p-4">
      <IssueThreadPanel {...props} />
    </div>
  )
}
