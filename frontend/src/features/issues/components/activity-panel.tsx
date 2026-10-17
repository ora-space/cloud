import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { AgentRequestFailure } from './agent-request-failure'
import { ActorAvatar, type AvatarActor } from '@/components/common/actor-avatar'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { Textarea } from '@/components/ui/textarea'
import {
  useCollaborationTargets,
  useCreateComment,
  useInteractions,
  useRuns,
  useTimeline,
} from '@/features/issues/api'
import { PendingTargets } from '@/features/issues/components/pending-targets'
import { TargetPicker } from '@/features/issues/components/target-picker'
import { memberNameById } from '@/features/issues/present'
import { useWorkflowRun } from '@/features/workflows/api'
import { workspacePaths } from '@/lib/paths'
import { cn } from '@/lib/utils'
import type {
  CollaborationTargetSummary,
  CommentTargetInput,
  IssueInteraction,
  IssueRun,
  TenantMember,
  TimelineEntry,
} from '@/features/issues/types'

/** Human actor for a timeline entry author; only `user` resolves to a member name. */
function entryActor(
  entry: TimelineEntry,
  names: ReadonlyMap<string, string>,
): {
  name: string
  type: NonNullable<AvatarActor['type']>
} {
  if (entry.authorUserId) return { name: names.get(entry.authorUserId) ?? '用户', type: 'user' }
  if (entry.authorType === 'agent') return { name: 'Agent', type: 'agent' }
  if (entry.authorType === 'team') return { name: 'Team', type: 'team' }
  return { name: '系统', type: 'user' }
}

/** Localized labels for the run-backed activity actions; unknown actions pass through. */
const ACTIVITY_LABELS: Record<string, string> = {
  'run.enqueued': '排队执行',
  'run.started': '开始执行',
  'run.progress': '执行进度',
  'run.message': '执行结果',
  'run.completed': '执行完成',
  'run.failed': '执行失败',
  'run.cancelled': '已取消',
}

function activityLabel(action: string): string {
  return ACTIVITY_LABELS[action] ?? action
}

function CommentItem({
  entry,
  names,
}: {
  entry: TimelineEntry
  names: ReadonlyMap<string, string>
}) {
  const author = entryActor(entry, names)
  return (
    <div className={cn('space-y-1', entry.parentId && 'ml-8 border-l pl-3')}>
      <div className="flex items-center gap-2">
        <ActorAvatar actor={{ name: author.name, type: author.type }} size="sm" />
        <span className="text-sm font-medium">{author.name}</span>
        <span className="text-xs text-muted-foreground">#{entry.seq}</span>
      </div>
      <p className="whitespace-pre-wrap text-sm text-muted-foreground">{entry.body}</p>
    </div>
  )
}

/** A workflow's human-readable output travels as an activity detail, never as a comment body. */
function activityDetail(entry: TimelineEntry): string | null {
  const message = entry.details?.['message']
  if (typeof message === 'string' && message !== '') return message
  const reason = entry.details?.['failureReason']
  if (typeof reason === 'string' && reason !== '') return reason
  return null
}

function ActivityItem({
  entry,
  names,
}: {
  entry: TimelineEntry
  names: ReadonlyMap<string, string>
}) {
  const author = entryActor(entry, names)
  const detailsExecutor = entry.details?.['executorType']
  const executorType = typeof detailsExecutor === 'string' ? detailsExecutor : null
  const detail = activityDetail(entry)
  return (
    <div className="space-y-1">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <span className="text-xs text-muted-foreground">#{entry.seq}</span>
        <Badge variant="secondary">{activityLabel(entry.action ?? '')}</Badge>
        {executorType ? <span className="text-xs">{executorType}</span> : null}
        <span className="ml-auto flex items-center gap-1.5">
          <ActorAvatar actor={{ name: author.name, type: author.type }} size="sm" />
        </span>
      </div>
      {detail ? (
        <div className="whitespace-pre-wrap rounded-md border bg-muted/50 px-2.5 py-1.5 text-xs text-muted-foreground">
          {detail}
        </div>
      ) : null}
    </div>
  )
}

/** A confirmed workflow interaction: it is history now, and never offers a second configuration. */
function ConfirmedWorkflowRow({
  slug,
  name,
  run,
}: {
  slug: string
  name: string
  run: IssueRun | undefined
}) {
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const runId = run?.workflowInvocationRef ?? undefined
  const workflow = useWorkflowRun(slug, run?.executorId, runId)
  const status = runId ? workflow.data?.status : run?.status
  const path = workspacePaths(workspaceSlug ?? slug)
  return (
    <div className="flex items-center gap-2 rounded-md border px-2 py-1 text-sm">
      <Badge variant="secondary">Workflow</Badge>
      <span className="min-w-0 flex-1 truncate font-medium">{name}</span>
      <span className="shrink-0 text-xs text-muted-foreground">
        已确认{status ? ` · 运行 ${status}` : ''}
      </span>
      {workflow.isError ? <span className="text-xs text-destructive">运行状态加载失败</span> : null}
      {run && runId ? (
        <Link className="shrink-0 text-xs underline" to={path.workflowRun(run.executorId, runId)}>
          运行详情
        </Link>
      ) : null}
    </div>
  )
}

/** Confirmed workflow interactions, shown as status rows next to the history. */
function ConfirmedWorkflows(props: {
  slug: string
  interactions: IssueInteraction[]
  runs: IssueRun[]
  catalog: CollaborationTargetSummary[]
}) {
  const byId = new Map(props.catalog.map((target) => [target.id, target]))
  const confirmed = props.interactions.filter((item) => item.mode === 'form' && item.runId !== null)
  return (
    <>
      {confirmed.map((item) => (
        <ConfirmedWorkflowRow
          key={item.id}
          slug={props.slug}
          name={byId.get(item.targetId)?.displayName ?? 'Workflow'}
          run={props.runs.find((candidate) => candidate.id === item.runId)}
        />
      ))}
    </>
  )
}

/**
 * Issue activity column. It reads top-to-bottom in the order a person works: the history first, then
 * the place where new work is created. Everything below the history is a **draft** — a comment, a
 * mention, a task or a workflow configuration reaches the server only when its own button is pressed.
 */
type ActivityPanelProps = {
  slug: string
  issueId: string
  members: TenantMember[]
}

/** Keeps every activity draft within one tenant, space and issue, even for cached navigation. */
export function ActivityPanel(props: ActivityPanelProps) {
  const { space, isPending } = useCurrentSpace()
  if (isPending) return null
  return <IssueActivity key={`${props.slug}:${space?.id ?? ''}:${props.issueId}`} {...props} />
}

function IssueActivity({ slug, issueId, members }: ActivityPanelProps) {
  const { data: timeline, isPending } = useTimeline(slug, issueId)
  const { data: interactions } = useInteractions(slug, issueId)
  const { data: runs } = useRuns(slug, issueId)
  const { data: catalog = [] } = useCollaborationTargets(slug)
  const createComment = useCreateComment(slug, issueId)
  const [body, setBody] = useState('')
  const [targets, setTargets] = useState<CommentTargetInput[]>([])
  const names = memberNameById(members)

  function submitComment(e: React.FormEvent) {
    e.preventDefault()
    if (!body.trim()) return
    createComment.mutate({ body: body.trim() }, { onSuccess: () => setBody('') })
  }

  /** Commits one staged target: the comment and its interaction are created together, now. */
  async function submitTarget(target: CommentTargetInput, message: string) {
    const entry: CommentTargetInput =
      target.type === 'agent' || target.type === 'team'
        ? { type: target.type, id: target.id, task: message }
        : { type: target.type, id: target.id }
    await createComment.mutateAsync({ body: message, targets: [entry] })
    setTargets((current) => current.filter((item) => item.id !== target.id))
  }

  return (
    <section className="space-y-4 pt-4">
      <h2 className="text-sm font-semibold">活动</h2>
      {isPending && <Skeleton className="h-16 w-full" />}
      {!isPending && timeline?.length === 0 && (
        <p className="text-sm text-muted-foreground">暂无评论。</p>
      )}
      {timeline?.map((entry) =>
        entry.kind === 'comment' ? (
          <CommentItem key={entry.id} entry={entry} names={names} />
        ) : (
          <ActivityItem key={entry.id} entry={entry} names={names} />
        ),
      )}

      <form onSubmit={submitComment} className="space-y-2">
        <Textarea
          placeholder="添加评论…"
          value={body}
          onChange={(e) => setBody(e.target.value)}
          rows={2}
        />
        <div className="flex justify-end">
          <Button type="submit" size="sm" disabled={!body.trim() || createComment.isPending}>
            {createComment.isPending ? '提交中…' : '评论'}
          </Button>
        </div>
        {createComment.isError && !createComment.variables?.targets?.length && (
          <AgentRequestFailure error={createComment.error} />
        )}
      </form>

      <ConfirmedWorkflows
        slug={slug}
        interactions={interactions ?? []}
        runs={runs ?? []}
        catalog={catalog}
      />

      <div className="space-y-1.5">
        <TargetPicker slug={slug} targets={targets} onChange={setTargets} />
        <PendingTargets
          slug={slug}
          issueId={issueId}
          targets={targets}
          catalog={catalog}
          onChange={setTargets}
          onSubmit={submitTarget}
        />
      </div>
    </section>
  )
}
