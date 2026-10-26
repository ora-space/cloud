import { Button } from '@/components/ui/button'
import type { IssueRun } from '@/features/issues/types'
import {
  taskConversationError,
  useResumeTaskConversation,
  useDiscardUndeliveredTaskRun,
} from './api'

function RecoveryButton({
  tid,
  issueId,
  runId,
  kind,
}: {
  tid: string
  issueId: string
  runId: string
  kind: 'resume' | 'discard'
}) {
  const resume = useResumeTaskConversation(tid, issueId)
  const discard = useDiscardUndeliveredTaskRun(tid, issueId)
  const action = kind === 'resume' ? resume : discard
  return (
    <>
      <Button
        className="mt-3"
        size="sm"
        variant="outline"
        disabled={action.isPending}
        onClick={() => action.mutate({ runId })}
      >
        {kind === 'resume' ? '恢复等待中的消息' : '放弃未交付成果并继续'}
      </Button>
      {action.isError && (
        <p role="alert" className="mt-2 text-destructive">
          {taskConversationError(action.error)}
        </p>
      )}
    </>
  )
}

function recoveryKind(run: IssueRun, paused: boolean): 'resume' | 'discard' | null {
  const delivery = run.result?.deliveryState ?? ''
  if (
    ['saved', 'unchanged'].includes(delivery) &&
    (paused || ['failed', 'cancelled'].includes(run.status))
  )
    return 'resume'
  if (
    ['failed', 'skipped'].includes(delivery) ||
    (delivery === '' && ['failed', 'cancelled'].includes(run.status))
  )
    return 'discard'
  return null
}

/** Recovery always refers to the current task pause rather than the selected historical Run. */
export function TaskRecovery({
  tid,
  issueId,
  last,
  waiting,
  manualPause,
}: {
  tid: string
  issueId: string
  last: IssueRun | undefined
  waiting: boolean
  manualPause: boolean
}) {
  if (!waiting || !last) return null
  const failedRun = ['failed', 'cancelled'].includes(last.status)
  const kind = recoveryKind(last, manualPause)
  if (!kind) return null
  let reason = failedRun ? '上次 Agent 执行未正常完成' : '上次 Agent 执行成果交付失败'
  if (manualPause) reason = '任务已手动暂停'
  return (
    <section className="rounded-xl border border-destructive/30 bg-destructive/5 p-4 text-sm">
      <p className="font-medium">{reason}，后续消息已暂停。</p>
      <p className="mt-1 text-muted-foreground">
        {kind === 'resume'
          ? '成果已保存并确认释放后，可明确恢复等待中的消息。'
          : '本次成果未交付。继续会放弃这次运行中未保存的文件修改；历史记录保留，下一轮从任务最近一次已保存成果开始。请先检查执行详情。'}
      </p>
      <RecoveryButton tid={tid} issueId={issueId} runId={last.id} kind={kind} />
    </section>
  )
}
