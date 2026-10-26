import type { GetApiV1TenantsTidIssuesIidConversation200 } from '@/api/generated.schemas'
import type { CollaborationTargetSummary, IssueRun } from '@/features/issues/types'
import { useThread } from '@/features/issues/thread/thread-api'
import { threadStateLabel, isThreadClosed } from '@/features/issues/thread/thread-entries'
import { EndSessionControl } from '@/features/issues/thread/thread-composer'
import { workspacePaths } from '@/lib/paths'
import { ConversationComposer } from './conversation-composer'
import { TaskExecutionControls } from './task-stop'
import type { useConversationDraft } from './use-conversation-draft'

function ActiveSession({
  tid,
  issueId,
  run,
  userId,
}: {
  tid: string
  issueId: string
  run: IssueRun
  userId: string | undefined
}) {
  const threadRef = { tid, issueId, runId: run.id }
  const thread = useThread(threadRef, false)
  if (thread.isError)
    return (
      <p role="alert" className="text-xs">
        当前执行状态读取失败
      </p>
    )
  const snapshot = thread.data
  if (!snapshot?.declared) return <p className="text-xs text-muted-foreground">正在准备执行环境…</p>
  const canEnd =
    !!userId &&
    userId === snapshot.initiatorUserId &&
    snapshot.canEnd &&
    !isThreadClosed(snapshot.threadState)
  return (
    <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
      <p>当前会话 · {threadStateLabel(snapshot.threadState)}</p>
      {canEnd && <EndSessionControl threadRef={threadRef} closed={false} />}
    </div>
  )
}

/** Current execution controls and a persistent draft remain independent of historical selection. */
export function ConversationFooter({
  tid,
  issueId,
  targets,
  runs,
  current,
  draft,
  waiting,
  selectedRunId,
  currentUserId,
  workspaceSlug,
  onLatest,
}: {
  tid: string
  issueId: string
  targets: CollaborationTargetSummary[]
  runs: IssueRun[]
  current: GetApiV1TenantsTidIssuesIidConversation200 | undefined
  draft: ReturnType<typeof useConversationDraft>
  waiting: boolean
  selectedRunId: string | undefined
  currentUserId: string | undefined
  workspaceSlug: string | undefined
  onLatest: () => void
}) {
  const active = runs.find((run) => run.id === current?.activeRunId)
  return (
    <div className="shrink-0 border-t bg-background">
      <div className="mx-auto max-h-[45svh] max-w-[840px] overflow-y-auto px-4 py-3 sm:px-6">
        {selectedRunId && (
          <p className="mb-2 text-xs text-muted-foreground">正在查看历史；新消息发送到当前任务。</p>
        )}
        {active && (
          <ActiveSession tid={tid} issueId={issueId} run={active} userId={currentUserId} />
        )}
        <TaskExecutionControls
          tid={tid}
          issueId={issueId}
          current={current}
          runs={runs}
          waiting={waiting}
        />
        <ConversationComposer
          tid={tid}
          issueId={issueId}
          targets={targets}
          latestRun={runs.at(-1)}
          state={draft}
          onSent={onLatest}
          readiness={current?.agentReadiness}
          settingsPath={workspaceSlug ? workspacePaths(workspaceSlug).modelConnections : undefined}
          pluginsPath={workspaceSlug ? workspacePaths(workspaceSlug).plugins : undefined}
        />
      </div>
    </div>
  )
}
