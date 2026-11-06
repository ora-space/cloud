import { lazy, Suspense } from 'react'
import { ChevronLeft, Workflow } from 'lucide-react'
import { formatTime } from './task-model'
import type { TaskDecomposition } from './tasks-api/generated'

const WorkflowRunView = lazy(() => import('@/features/chat/workflow-run/run-pane'))

const STATUS = {
  running: { label: '运行中', tone: 'running' },
  succeeded: { label: '已完成', tone: 'done' },
  failed: { label: '失败', tone: 'cancelled' },
}

/** What the run left on the task, in the words the page uses. */
function outcome(d: TaskDecomposition): string {
  if (d.status === 'succeeded') return `拆出 ${d.createdCount} 个子任务`
  if (d.status === 'failed') return d.failure || '工作流没有给出子任务。'
  return '正在拆分'
}

function WorkflowRow({ decomposition: d }: { decomposition: TaskDecomposition }) {
  const status = STATUS[d.status]
  return <>
    <span className={`mc-status ${status.tone}`}>{status.label}</span>
    <span className="mc-task-main">
      <strong>{d.workflowName}</strong>
      <small>拆分 · {d.startedByName} 发起 · {formatTime(d.createdAt)}</small>
    </span>
    <small className="mc-workflow-outcome">{outcome(d)}</small>
  </>
}

/**
 * The workflows run for this task, newest first, and one run opened in the same view the chat
 * page has. The view is read-only here: a run started from it would not belong to the task. A run
 * the workflow host refused has nothing to open, so its row only says why.
 */
export function TaskWorkflows({ tenantId, decompositions, selected, onSelect }: {
  tenantId: string
  decompositions: TaskDecomposition[]
  /** The run opened; undefined shows the list. */
  selected: TaskDecomposition | undefined
  onSelect: (workflowRunId?: string) => void
}) {
  if (selected) {
    const back = <button type="button" className="mc-workflow-back" aria-label="返回工作流列表" title="返回工作流列表" onClick={() => onSelect()}><ChevronLeft size={16} aria-hidden="true" /></button>
    return <section className="mc-workflow-run" aria-label="工作流运行">
      <Suspense fallback={<p role="status" className="mc-run-note">正在读取运行…</p>}>
        <WorkflowRunView runId={selected.workflowRunId} tenantId={tenantId} readOnly leading={back} />
      </Suspense>
    </section>
  }
  if (decompositions.length === 0) {
    return <div className="mc-run-empty"><Workflow size={22} aria-hidden="true" /><p>这个任务还没有运行过工作流。</p></div>
  }
  return <ul className="mc-tasks" aria-label="工作流运行记录">{decompositions.map(d => <li key={d.id}>
    {d.workflowRunId
      ? <button type="button" className="mc-task-row" aria-label={`查看工作流运行 ${d.workflowName}，${formatTime(d.createdAt)}`} onClick={() => onSelect(d.workflowRunId)}><WorkflowRow decomposition={d} /></button>
      : <div className="mc-task-row"><WorkflowRow decomposition={d} /></div>}
  </li>)}</ul>
}
