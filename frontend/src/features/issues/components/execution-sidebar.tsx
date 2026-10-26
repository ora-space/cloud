import { Link } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { useTaskConversation } from '@/features/issues/conversation/api'
import { useThread } from '@/features/issues/thread/thread-api'
import { threadStateLabel } from '@/features/issues/thread/thread-entries'
import { memberNameById } from '@/features/issues/present'
import type { CollaborationTargetSummary, IssueRun, TenantMember } from '@/features/issues/types'
import { SessionStatisticsSummary } from '@/features/sessions/session-statistics-summary'
import { workspacePaths } from '@/lib/paths'
import { PullRequestRefs } from './pull-request-refs'

/** Localized execution state; never derives task completion from a reply. */
function executionLabel(run: IssueRun) {
  const labels: Record<string, string> = {
    queued: '排队中',
    dispatched: '准备中',
    running: '执行中',
    completed: '已完成',
    failed: '失败',
    cancelled: '已取消',
  }
  return labels[run.status] ?? '状态未知'
}

function ActiveExecution({
  tid,
  issueId,
  run,
  members,
}: {
  tid: string
  issueId: string
  run: IssueRun
  members: TenantMember[]
}) {
  const thread = useThread({ tid, issueId, runId: run.id }, false)
  if (thread.isError) return <p role="alert">当前会话读取失败</p>
  const data = thread.data
  return (
    <div className="space-y-1 text-xs text-muted-foreground">
      <p>
        {executionLabel(run)}
        {data?.declared && ` · ${threadStateLabel(data.threadState)}`}
      </p>
      {data?.declared && (
        <>
          <p>发起者：{memberNameById(members).get(data.initiatorUserId ?? '') ?? '用户'}</p>
          {data.model && (
            <p className="break-words">
              {data.model.connectionName} · {data.model.modelName}
            </p>
          )}
        </>
      )}
      {run.preparation && <p>正在准备执行环境</p>}
    </div>
  )
}

/** Compact run navigation shared by the right rail; historical selection never controls active work. */
export function ExecutionSidebar({
  tid,
  issueId,
  runs,
  targets,
  members,
  selected,
  workspaceSlug,
  onOpen,
  onAll,
  failed,
}: {
  tid: string
  issueId: string
  runs: IssueRun[]
  targets: CollaborationTargetSummary[]
  members: TenantMember[]
  selected: string | undefined
  workspaceSlug: string | undefined
  onOpen: (id: string) => void
  onAll: () => void
  failed: boolean
}) {
  const conversation = useTaskConversation(tid, issueId)
  const current = conversation.data?.pages[0]
  const active = runs.find((run) => run.id === current?.activeRunId)
  const ordered = runs.toSorted(
    (a, b) => b.createdAt.localeCompare(a.createdAt) || b.id.localeCompare(a.id),
  )
  const chosen = selectedExecution(selected, ordered, active)
  const name = (run: IssueRun) =>
    targets.find((target) => target.type === run.executorType && target.id === run.executorId)
      ?.displayName ?? run.executorType
  const paths = workspaceSlug ? workspacePaths(workspaceSlug) : undefined
  if (failed) return <p role="alert">执行记录加载失败，请稍后重试。</p>
  return (
    <>
      <section aria-label="当前执行" className="space-y-2 border-t pt-4">
        <h2 className="font-medium">当前执行</h2>
        {conversation.isError ? (
          <p role="alert">当前执行状态读取失败</p>
        ) : (
          <>
            {active ? (
              <>
                <p>{name(active)}</p>
                <ActiveExecution tid={tid} issueId={issueId} run={active} members={members} />
              </>
            ) : (
              <p className="text-xs text-muted-foreground">
                {conversation.isPending ? '加载中…' : '当前没有执行'}
              </p>
            )}
            {current?.blockedRunId && (
              <p className="text-xs text-destructive">成果交付阻塞，请在输入区处理。</p>
            )}
            {current?.pausedRunId && (
              <p className="text-xs text-muted-foreground">后续消息已暂停，等待明确恢复。</p>
            )}
          </>
        )}
      </section>
      <PullRequestRefs tid={tid} issueId={issueId} />
      <RecentExecutions ordered={ordered} name={name} paths={paths} onOpen={onOpen} onAll={onAll} />
      {chosen?.executorType === 'agent' && (
        <SessionStatisticsSummary
          key={chosen.id}
          sessionRef={{ tid, issueId, runId: chosen.id }}
          detailsHref={paths?.agentSession(issueId, chosen.id)}
        />
      )}
    </>
  )
}

function RecentExecutions({
  ordered,
  name,
  paths,
  onOpen,
  onAll,
}: {
  ordered: IssueRun[]
  name: (run: IssueRun) => string
  paths: ReturnType<typeof workspacePaths> | undefined
  onOpen: (id: string) => void
  onAll: () => void
}) {
  return (
    <section aria-label="最近执行" className="space-y-3 border-t pt-4">
      <h2 className="flex items-center justify-between font-medium">
        执行记录 <span className="text-xs text-muted-foreground">{ordered.length} 次</span>
      </h2>
      {ordered.slice(0, 5).map((run) => (
        <div key={run.id} className="space-y-1 text-xs">
          {run.executorType === 'workflow' && run.workflowInvocationRef && paths ? (
            <Link
              className="font-medium hover:underline"
              to={paths.workflowRun(run.executorId, run.workflowInvocationRef)}
            >
              {name(run)} · 查看工作流
            </Link>
          ) : (
            <button
              className="w-full text-left font-medium hover:underline"
              onClick={() => (run.executorType === 'agent' ? onOpen(run.id) : onAll())}
            >
              {name(run)} · {executionLabel(run)}
            </button>
          )}
          {typeof run.input['task'] === 'string' && (
            <p className="line-clamp-2 break-words text-muted-foreground">{run.input['task']}</p>
          )}
          <time className="text-muted-foreground" dateTime={run.createdAt}>
            {new Date(run.createdAt).toLocaleString()}
          </time>
          {run.revision && <p>已保存成果 · {run.revision.finalCommit.slice(0, 7)}</p>}
          {run.result?.deliveryState === 'failed' && (
            <p className="text-destructive">成果保存失败</p>
          )}
        </div>
      ))}
      {!ordered.length && <p className="text-xs text-muted-foreground">暂无执行记录</p>}
      <Button size="xs" variant="ghost" onClick={onAll}>
        查看全部执行
      </Button>
    </section>
  )
}

function selectedExecution(
  selected: string | undefined,
  ordered: IssueRun[],
  active: IssueRun | undefined,
) {
  if (selected) return ordered.find((run) => run.id === selected)
  return active ?? ordered.find((run) => run.executorType === 'agent')
}
