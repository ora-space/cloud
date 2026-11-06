import { ArrowUpRight, CheckCircle2, ClipboardCheck, Link2, Loader2, RotateCcw, TriangleAlert } from 'lucide-react'
import { Link } from 'react-router'
import { useQuery } from '@tanstack/react-query'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import { getWorkflowForm } from '@/lib/workflows/api/generated'
import type { GraphWorkflowNodeState } from '@/lib/workflows/runtime'
import { cn } from '@/lib/utils'

type TaskResult = {
  key?: string
  taskId?: string
  action?: string
}

const text = (value: unknown) => typeof value === 'string' ? value : ''

/** Severity stays legible without colour: the label carries the level. */
function severityChip(severity: string) {
  if (severity.includes('高') || /critical|high/i.test(severity)) return 'border-destructive/30 bg-destructive/10 text-destructive'
  if (severity.includes('中') || /medium/i.test(severity)) return 'border-warning/35 bg-warning/10 text-warning'
  if (severity.includes('低') || /low/i.test(severity)) return 'border-info/30 bg-info/10 text-info'
  return 'border-border bg-muted text-muted-foreground'
}

function readTasks(state: GraphWorkflowNodeState | null): TaskResult[] {
  const raw = state?.output?.detail ?? state?.output?.summary ?? ''
  if (!raw) return []
  try {
    const parsed: unknown = JSON.parse(raw)
    const value = parsed !== null && typeof parsed === 'object' ? parsed as Record<string, unknown> : {}
    const rows = Array.isArray(value.tasks) ? value.tasks : Array.isArray(parsed) ? parsed : []
    return rows.filter((row): row is TaskResult => row !== null && typeof row === 'object')
  } catch {
    return []
  }
}

/**
 * CreateTasks owns the task outcome, so its stage lists exactly what was created or reused.
 * Titles come from the run's frozen form when it is readable; the finding key alone is not a
 * sentence anyone should have to read.
 */
export function RunTasksStage({ tenantId, runId, nodeTitle, state, onOpenForm }: {
  tenantId: string
  runId: string
  nodeTitle: string
  state: GraphWorkflowNodeState | null
  onOpenForm?: () => void
}) {
  const tasks = readTasks(state)
  const status = state?.status ?? 'idle'
  const pending = status === 'running' || status === 'awaiting_input' || status === 'retry_waiting'
  // Same key as the form stage: one read for the whole run, not a second source of truth.
  const form = useQuery({ queryKey: ['workflow-form', tenantId, runId], queryFn: () => getWorkflowForm(tenantId, runId), retry: false, enabled: tasks.length > 0 })
  const created = tasks.filter(task => task.action !== 'reused').length
  const reused = tasks.filter(task => task.action === 'reused').length
  const titleOf = (task: TaskResult) => form.data?.findings.find(finding => finding.key === task.key)?.title
  const severityOf = (task: TaskResult) => form.data?.findings.find(finding => finding.key === task.key)?.severity ?? ''

  const badge = pending
    ? <Badge variant="outline" className="gap-1 border-info/30 bg-info/10 text-info"><Loader2 className="size-3 animate-spin" />处理中</Badge>
    : status === 'failed'
      ? <Badge variant="outline" className="gap-1 border-destructive/30 bg-destructive/10 text-destructive"><TriangleAlert className="size-3" />失败</Badge>
      : status === 'succeeded'
        ? <Badge variant="outline" className="gap-1 border-success/30 bg-success/10 text-success"><CheckCircle2 className="size-3" />{tasks.length} 项</Badge>
        : <Badge variant="outline" className="text-muted-foreground">待执行</Badge>

  return <section className="flex w-full flex-col gap-4" aria-label="任务处理结果">
    <header className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
      <div className="min-w-0">
        <p className="text-xs font-medium text-muted-foreground">协作任务</p>
        <h2 className="mt-1 text-xl font-semibold tracking-tight">{nodeTitle || '创建或关联任务'}</h2>
        <p className="mt-1 text-sm text-muted-foreground">确认结果已写入这次运行；任务由表单提交后确定性创建或关联，不再经过模型。</p>
      </div>
      <div className="flex shrink-0 flex-wrap items-center gap-2">
        {created > 0 && <Badge variant="outline" className="gap-1 border-success/30 bg-success/10 text-success"><ClipboardCheck className="size-3" />新建 {created}</Badge>}
        {reused > 0 && <Badge variant="outline" className="gap-1 border-info/30 bg-info/10 text-info"><Link2 className="size-3" />关联 {reused}</Badge>}
        {status !== 'succeeded' || tasks.length === 0 ? badge : null}
      </div>
    </header>

    {tasks.length > 0 ? <ul className="flex flex-col gap-2">
      {tasks.map((task, index) => <li key={`${task.taskId ?? task.key ?? index}`} className="flex flex-wrap items-center gap-3 rounded-xl border bg-card px-4 py-3">
        <span className={cn('flex size-8 shrink-0 items-center justify-center rounded-lg', task.action === 'reused' ? 'bg-info-soft text-info' : 'bg-success-soft text-success')}>
          {task.action === 'reused' ? <Link2 className="size-4" aria-hidden /> : <ClipboardCheck className="size-4" aria-hidden />}
        </span>
        <div className="min-w-0 flex-1">
          <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
            {severityOf(task) && <span className={cn('inline-flex shrink-0 items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium', severityChip(severityOf(task)))}>
              <TriangleAlert className="size-3" aria-hidden />{severityOf(task)}
            </span>}
            <p className="min-w-0 truncate text-sm font-medium">{titleOf(task) ?? task.key ?? '未命名任务'}</p>
          </div>
          <p className="mt-0.5 truncate font-mono text-[11px] text-muted-foreground" title={task.taskId}>#{text(task.taskId).slice(0, 8) || '任务编号不可用'}</p>
        </div>
        <Badge variant="outline" className="shrink-0 text-[11px]">{task.action === 'reused' ? '已关联现有任务' : '已创建任务'}</Badge>
        {task.taskId ? <Link className={buttonVariants({ size: 'sm', variant: 'outline' })} to={`/collaboration/tasks/${task.taskId}`}>打开任务<ArrowUpRight aria-hidden /></Link> : null}
      </li>)}
    </ul> : <div className="rounded-xl border border-dashed bg-muted/20 px-5 py-10 text-center">
      <ClipboardCheck className="mx-auto size-6 text-muted-foreground" aria-hidden />
      <p className="mt-3 text-sm font-medium">{pending ? '正在创建或关联任务' : status === 'failed' ? '任务没有建成' : '没有需要创建的任务'}</p>
      <p className="mx-auto mt-1 max-w-md text-sm text-muted-foreground">{pending ? '表单提交后，这里会列出每个问题的处理结果。' : status === 'failed' ? state?.errorMessage || '确认已经保存；回到确认节点可以重试任务创建，不需要重新判定。' : '本次确认没有需要创建或关联的协作任务。'}</p>
      {status === 'failed' && onOpenForm && <Button className="mt-3" variant="outline" size="sm" onClick={onOpenForm}><RotateCcw />回到确认节点重试</Button>}
    </div>}
  </section>
}
