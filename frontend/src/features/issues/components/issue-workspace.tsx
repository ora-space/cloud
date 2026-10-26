import { Link, useParams, useSearchParams } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useCollaborationTargets, useRuns } from '@/features/issues/api'
import { useSession } from '@/features/auth/session'
import { TaskConversationPanel } from '@/features/issues/conversation/conversation-panel'
import type { CollaborationTargetSummary, IssueRun, TenantMember } from '@/features/issues/types'
import { ActivityPanel } from './activity-panel'
import { workspacePaths } from '@/lib/paths'
import { TaskLayout } from './task-layout'
import { ExecutionSidebar } from './execution-sidebar'

const STATUS: Record<string, string> = {
  queued: '排队中',
  dispatched: '准备中',
  running: '执行中',
  completed: '已完成',
  failed: '失败',
  cancelled: '已取消',
}

function RunCard({ run, name, onOpen }: { run: IssueRun; name: string; onOpen: () => void }) {
  const task = run.input['task']
  return (
    <li className="rounded-xl border bg-card p-4">
      <div className="flex items-center justify-between gap-3">
        <span className="font-medium">{name}</span>
        <span className="text-xs text-muted-foreground">{STATUS[run.status] ?? '状态未知'}</span>
      </div>
      {typeof task === 'string' && (
        <p className="mt-2 line-clamp-2 break-words text-sm text-muted-foreground">{task}</p>
      )}
      <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <time dateTime={run.createdAt}>
          {run.createdAt.replace('T', ' ').replace(/\.\d+Z$|Z$/, ' UTC')}
        </time>
        <Button variant="outline" size="sm" onClick={onOpen}>
          查看执行详情
        </Button>
      </div>
      {run.revision && (
        <p className="mt-2 text-xs text-muted-foreground">
          已保存成果 · {run.revision.finalCommit.slice(0, 7)}
        </p>
      )}
      {run.result?.deliveryState === 'failed' && (
        <p className="mt-2 text-xs text-destructive">成果保存失败</p>
      )}
    </li>
  )
}

function RunHeading({
  run,
  targets,
  workspaceSlug,
  issueId,
}: {
  run: IssueRun | undefined
  targets: CollaborationTargetSummary[]
  workspaceSlug: string | undefined
  issueId: string
}) {
  if (!run) return null
  const agent = targets.find((target) => target.type === 'agent' && target.id === run.executorId)
  return (
    <div className="flex flex-wrap items-center justify-between gap-2 text-sm">
      <span className="font-medium">{agent?.displayName ?? 'Agent'}</span>
      {workspaceSlug && (
        <Link
          className="text-xs text-muted-foreground underline underline-offset-4"
          to={workspacePaths(workspaceSlug).agentSession(issueId, run.id)}
        >
          完整执行记录与归档
        </Link>
      )}
    </div>
  )
}

function useWorkspaceNavigation(tid: string, issueId: string) {
  const [params, setParams] = useSearchParams()
  const { session } = useSession()
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const { data: runs = [], isError } = useRuns(tid, issueId)
  const { data: targets = [] } = useCollaborationTargets(tid)
  const selected = params.get('run') ?? undefined
  const requestedTab = params.get('view')
  const tab = requestedTab === 'runs' || requestedTab === 'activity' ? requestedTab : 'conversation'
  const agents = runs
    .filter((run) => run.executorType === 'agent')
    .toSorted((a, b) => b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id))
  const viewedRun = selected ? agents.find((run) => run.id === selected) : undefined
  function navigate(view: string, runId?: string) {
    setParams((previous) => {
      const next = new URLSearchParams(previous)
      next.set('view', view)
      if (runId) next.set('run', runId)
      else next.delete('run')
      return next
    })
  }
  return { session, workspaceSlug, runs, isError, targets, selected, tab, viewedRun, navigate }
}

/** Task-level navigation preserves historical URLs and a single mounted composer across views. */
export function IssueWorkspace({
  tid,
  issueId,
  members,
  heading,
  properties,
  details,
}: {
  tid: string
  issueId: string
  members: TenantMember[]
  heading?: React.ReactNode
  properties?: React.ReactNode
  details?: React.ReactNode
}) {
  const { session, workspaceSlug, runs, isError, targets, selected, tab, viewedRun, navigate } =
    useWorkspaceNavigation(tid, issueId)
  const records = (
    <RunList
      runs={runs}
      targets={targets}
      failed={isError}
      onOpen={(id) => navigate('conversation', id)}
      workspaceSlug={workspaceSlug}
    />
  )
  const activity = (
    <ActivityPanel
      slug={tid}
      issueId={issueId}
      members={members}
      onOpenRun={(id) => navigate('conversation', id)}
      onTaskStarted={() => navigate('conversation')}
    />
  )
  return (
    <TaskLayout
      sidebar={
        <>
          {properties}
          <ExecutionSidebar
            failed={isError}
            tid={tid}
            issueId={issueId}
            runs={runs}
            targets={targets}
            members={members}
            selected={selected}
            workspaceSlug={workspaceSlug}
            onOpen={(id) => navigate('conversation', id)}
            onAll={() => navigate('runs')}
          />
          {details}
        </>
      }
    >
      <Tabs
        value={tab}
        onValueChange={(value) => navigate(String(value))}
        className="min-h-0 min-w-0 flex-1 gap-0"
      >
        <div className="shrink-0 border-b">
          <div className="mx-auto max-w-[840px] space-y-3 px-4 pt-4 sm:px-6">
            {heading}
            <WorkspaceTabs count={runs.length} />
          </div>
        </div>
        <TabsContent value={tab} className="min-h-0">
          <div className="flex h-full min-h-0 flex-col">
            {selected && tab === 'conversation' && (
              <div className="mx-auto flex w-full max-w-[840px] shrink-0 flex-wrap items-center justify-between px-4 py-2">
                <RunHeading
                  run={viewedRun}
                  targets={targets}
                  workspaceSlug={workspaceSlug}
                  issueId={issueId}
                />
                <Button variant="ghost" size="sm" onClick={() => navigate('conversation')}>
                  返回最新执行
                </Button>
              </div>
            )}
            <TaskConversationPanel
              tid={tid}
              issueId={issueId}
              members={members}
              targets={targets}
              runs={runs}
              selectedRunId={selected}
              workspaceSlug={workspaceSlug}
              currentUserId={session.status === 'signed-in' ? session.user.id : undefined}
              onLatest={() => navigate('conversation')}
              content={{ runs: records, activity, conversation: undefined }[tab]}
            />
          </div>
        </TabsContent>
      </Tabs>
    </TaskLayout>
  )
}

function RunList({
  runs,
  targets,
  failed,
  onOpen,
  workspaceSlug,
}: {
  runs: IssueRun[]
  targets: CollaborationTargetSummary[]
  failed: boolean
  onOpen: (id: string) => void
  workspaceSlug: string | undefined
}) {
  const ordered = runs.toSorted(
    (a, b) => b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id),
  )
  return (
    <>
      {failed && <p role="alert">执行记录加载失败，请稍后重试。</p>}
      <ol aria-label="执行记录" className="space-y-3">
        {ordered.map((run) =>
          run.executorType === 'agent' ? (
            <RunCard
              key={run.id}
              run={run}
              name={
                targets.find((target) => target.type === 'agent' && target.id === run.executorId)
                  ?.displayName ?? 'Agent'
              }
              onOpen={() => onOpen(run.id)}
            />
          ) : (
            <li key={run.id} className="border-b py-3">
              <p>
                {targets.find(
                  (target) => target.type === run.executorType && target.id === run.executorId,
                )?.displayName ?? run.executorType}
              </p>
              {run.executorType === 'workflow' && run.workflowInvocationRef && workspaceSlug ? (
                <Link
                  className="text-sm underline"
                  to={workspacePaths(workspaceSlug).workflowRun(
                    run.executorId,
                    run.workflowInvocationRef,
                  )}
                >
                  查看工作流运行
                </Link>
              ) : (
                <p className="text-xs text-muted-foreground">{STATUS[run.status] ?? '状态未知'}</p>
              )}
            </li>
          ),
        )}
      </ol>
      {!runs.length && !failed && (
        <p className="py-6 text-sm text-muted-foreground">暂无执行记录</p>
      )}
    </>
  )
}

function WorkspaceTabs({ count }: { count: number }) {
  return (
    <TabsList variant="line">
      <TabsTrigger value="conversation">对话</TabsTrigger>
      <TabsTrigger value="runs">执行记录{count ? ` · ${count}` : ''}</TabsTrigger>
      <TabsTrigger value="activity">任务动态</TabsTrigger>
    </TabsList>
  )
}
