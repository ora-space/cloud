import { Link } from 'react-router-dom'
import { useWorkflowRun } from '@/features/workflows/api'
import { useTimeline } from '@/features/issues/api'
import type { IssueRun, TimelineEntry } from '@/features/issues/types'
import { ConversationMarkdown } from '@/features/conversation-content/markdown'
import { workspacePaths } from '@/lib/paths'

const labels: Record<string, string> = {
  pending: '等待执行',
  queued: '等待执行',
  dispatched: '准备中',
  running: '执行中',
  awaiting_input: '等待输入',
  succeeded: '执行完成',
  completed: '执行完成',
  failed: '执行失败',
  cancelled: '已取消',
}

function WorkflowOutputs({ entries, runId }: { entries: TimelineEntry[]; runId: string }) {
  return entries
    .filter((entry) => entry.action === 'run.message' && entry.details?.['runId'] === runId)
    .map((entry) =>
      typeof entry.details?.['message'] === 'string' ? (
        <ConversationMarkdown key={entry.id} text={entry.details['message']} />
      ) : null,
    )
}

function useWorkflowCard(tid: string, issueId: string, run: IssueRun) {
  const invocation = run.workflowInvocationRef ?? undefined
  const detail = useWorkflowRun(tid, run.executorId, invocation)
  const timeline = useTimeline(tid, issueId)
  const value = detail.isError ? undefined : detail.data
  return {
    invocation,
    failed: detail.isError,
    name: value?.name ?? '工作流运行',
    snapshot: value?.snapshotId ?? run.input['workflowSnapshotId'],
    status: value?.status ?? run.status,
    error: value?.error,
    entries: timeline.isError || detail.isError ? [] : (timeline.data ?? []),
  }
}

/** Reads the linked WorkflowRun without issuing Agent Thread reads or inventing a session identity. */
export function WorkflowRunCard({
  tid,
  issueId,
  run,
  workspaceSlug,
}: {
  tid: string
  issueId: string
  run: IssueRun
  workspaceSlug: string | undefined
}) {
  const { invocation, failed, name, snapshot, status, error, entries } = useWorkflowCard(
    tid,
    issueId,
    run,
  )
  return (
    <article
      aria-label="工作流执行"
      className="space-y-2 rounded-lg border p-3"
      data-conversation-output={`${run.id}:${status}`}
    >
      <div className="flex flex-wrap justify-between gap-2 text-sm">
        <span className="font-medium">工作流 · {name}</span>
        <span>{labels[status] ?? '状态未知'}</span>
      </div>
      {typeof snapshot === 'string' && (
        <p className="break-all text-xs text-muted-foreground">发布版本：{snapshot}</p>
      )}
      {failed && <p role="alert">工作流记录不可访问或加载失败，请重试。</p>}
      <WorkflowOutputs entries={entries} runId={run.id} />
      {error && <p role="alert">{error}</p>}
      {invocation && workspaceSlug && (
        <Link
          className="text-xs underline"
          to={workspacePaths(workspaceSlug).workflowRun(run.executorId, invocation)}
        >
          工作流运行详情
        </Link>
      )}
    </article>
  )
}
