import { Button } from '@/components/ui/button'
import { taskConversationError, useStopTaskConversation } from './api'
import type { GetApiV1TenantsTidIssuesIidConversation200 } from '@/api/generated.schemas'
import type { IssueRun } from '@/features/issues/types'
import { TaskRecovery } from './task-recovery'

/** Keeps stop and resume attached to current task state while historical transcripts stay readable. */
export function TaskExecutionControls({
  tid,
  issueId,
  current,
  runs,
  waiting,
}: {
  tid: string
  issueId: string
  current: GetApiV1TenantsTidIssuesIidConversation200 | undefined
  runs: IssueRun[]
  waiting: boolean
}) {
  const paused = runs.find((run) => run.id === (current?.blockedRunId ?? current?.pausedRunId))
  return (
    <div className="space-y-3">
      <TaskRecovery
        tid={tid}
        issueId={issueId}
        last={paused ?? runs.at(-1)}
        waiting={waiting || !!current?.pausedRunId || !!current?.blockedRunId}
        manualPause={paused?.id === current?.pausedRunId && !!current?.pausedRunId}
      />
      <TaskStop
        tid={tid}
        issueId={issueId}
        activeRunId={current?.activeRunId}
        pausedRunId={current?.pausedRunId}
        canStop={current?.canStop === true}
      />
    </div>
  )
}

/** The current task stop remains visible independently of which historical Run is being read. */
function TaskStop({
  tid,
  issueId,
  activeRunId,
  pausedRunId,
  canStop,
}: {
  tid: string
  issueId: string
  activeRunId: string | null | undefined
  pausedRunId: string | null | undefined
  canStop: boolean
}) {
  const stop = useStopTaskConversation(tid, issueId)
  if (pausedRunId)
    return (
      <p role="status" className="rounded-lg border p-3 text-sm text-muted-foreground">
        后续消息已暂停。当前成果保存并释放后，需要明确恢复。
      </p>
    )
  if (!canStop || !activeRunId) return null
  return (
    <div className="space-y-2">
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={stop.isPending}
        onClick={() => stop.mutate({ runId: activeRunId })}
      >
        {stop.isPending ? '正在请求停止…' : '停止当前执行'}
      </Button>
      {stop.isError && (
        <p role="alert" className="text-xs text-destructive">
          {taskConversationError(stop.error)}
        </p>
      )}
    </div>
  )
}
