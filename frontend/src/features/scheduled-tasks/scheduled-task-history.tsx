import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import type { ScheduledTaskOccurrence } from '@/api/generated.schemas'
import { useScheduledTaskHistory } from './api'
import { ScheduledRunArtifacts } from './scheduled-run-artifacts'

const OUTCOME_LABELS: Record<string, string> = {
  admitted: '待执行',
  missed: '已错过',
  skipped_busy: '运行中，跳过',
  failed: '失败',
  queued: '排队中',
  dispatched: '已派发',
  running: '运行中',
  completed: '已完成',
  cancelled: '已取消',
}

const OUTCOME_TONE: Record<string, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  completed: 'default',
  missed: 'secondary',
  skipped_busy: 'outline',
  failed: 'destructive',
  cancelled: 'secondary',
}

const VERDICT_LABELS: Record<string, string> = {
  pass: '通过',
  warn: '需注意',
  fail: '未通过',
  blocked: '受阻',
  idle: '无改动',
  range_unresolved: '范围未确定',
}

// Admission refusals the creator resolves in the issue's task conversation, not in the task or
// Agent configuration; other reasons keep their raw code.
const FAILURE_REASON_LABELS: Record<string, string> = {
  task_paused: '任务对话已暂停或有失败消息，请先在任务对话中处理',
  task_handoff_pending: '任务对话中有等待执行的消息，本次未运行',
}

function failureReasonLabel(reason: string): string {
  return FAILURE_REASON_LABELS[reason] ?? reason
}

/** The run's own business verdict; a run that never reported one is shown as 未报告. */
function verdictLabel(verdict?: string | null): string {
  return verdict ? (VERDICT_LABELS[verdict] ?? verdict) : '未报告'
}
function outcomeLabel(outcome: string): string {
  return OUTCOME_LABELS[outcome] ?? outcome
}

function timestamp(value: string): string {
  return new Date(value).toLocaleString()
}

/** Displays the read-only occurrence history for one definition without inferring a live run state. */
export function ScheduledTaskHistory({
  tid,
  taskId,
  issueId,
  title,
  onClose,
}: {
  tid: string
  taskId: string
  issueId: string
  title: string
  onClose: () => void
}) {
  const history = useScheduledTaskHistory(tid, taskId)
  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>运行历史 · {title}</DialogTitle>
          <DialogDescription>每次到期点的接纳结果，按时间倒序。</DialogDescription>
        </DialogHeader>
        {history.isPending && <Skeleton className="h-32 w-full" />}
        {history.isError && (
          <p role="alert" className="text-sm text-destructive">
            历史加载失败
          </p>
        )}
        {history.data && history.data.entries.length === 0 && (
          <p className="text-sm text-muted-foreground">尚无运行记录</p>
        )}
        {history.data && history.data.entries.length > 0 && (
          <ul className="space-y-2">
            {history.data.entries.map((entry: ScheduledTaskOccurrence) => (
              <li
                key={entry.scheduledAt}
                className="flex items-center justify-between gap-2 rounded-md border px-3 py-2"
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-2">
                    <Badge variant={OUTCOME_TONE[entry.outcome] ?? 'secondary'}>
                      {outcomeLabel(entry.outcome)}
                    </Badge>
                    {entry.failureReason && (
                      <span className="truncate text-xs text-muted-foreground">
                        {failureReasonLabel(entry.failureReason)}
                      </span>
                    )}
                  </div>{' '}
                  <p className="mt-1 text-xs text-muted-foreground">
                    结论 {verdictLabel(entry.verdict)}
                    {entry.verdictDetail ? `：${entry.verdictDetail}` : ''}
                    {entry.runId
                      ? `${entry.artifactsExpired ? ' · 产物已过期' : ` · 产物 ${entry.artifactCount ?? 0} 项`}${
                          entry.rejectedArtifactCount
                            ? `（${entry.rejectedArtifactCount} 项被拒）`
                            : ''
                        }`
                      : ''}
                  </p>
                  <p className="mt-1 text-xs text-muted-foreground">
                    计划 {timestamp(entry.scheduledAt)} · 观察 {timestamp(entry.observedAt)} · 版本{' '}
                    {entry.taskRevision}
                  </p>
                  {entry.runId && (
                    <ScheduledRunArtifacts tid={tid} issueId={issueId} runId={entry.runId} />
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            关闭
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
