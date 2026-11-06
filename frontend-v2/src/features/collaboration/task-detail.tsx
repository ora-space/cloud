import { Bot, CirclePlay, CircleStop, RotateCcw, Users, Workflow } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import type { SquadSummary } from '@/lib/squads-api/generated'
import { formatTime } from './task-model'
import { TaskAssigneePicker } from '@/components/task-assignee-picker'
import { assigneeKindLabel, assigneeLabel, isTaskFinished, taskActions } from './task-model'
import { closeRefusal, runsAgent } from './task-views'
import type { Task, TaskAssigneeRef, TaskExecutor, TaskStatusProperty } from './tasks-api/generated'

/** The run controls' state, decided by the page from the task and its timeline. */
export interface RunControls {
  /** The executors that can run the task's Agent; empty while loading or for non-Agent tasks. */
  executors: TaskExecutor[]
  executorId: string
  onExecutor: (id: string) => void
  /** Whether the task already has runs, so starting again is a rerun. */
  hasRuns: boolean
  starting: boolean
  stopping: boolean
  onStart: () => void
  onStop: () => void
}

/**
 * One task's facts and the moves on it: who owns it, running its Agent, and the manual status
 * moves. While the Agent runs a turn, the run owns the status, so manual moves wait for it.
 */
export function TaskDetailView({ task, me, inSquad, canClose, projectName, squads, busy, error, run, onWorkflow, onStatus, onAssign }: {
  task: Task
  /** The caller's user id: a person's task runs an Agent only for that person. */
  me: string
  /** The caller is a member of the squad that owns the task. */
  inSquad: boolean
  /** The caller may end the task: confirm it or cancel it. */
  canClose: boolean
  /** The project's display name, resolved from the workspace list. */
  projectName: string
  /** Live squads the task may be reassigned to. */
  squads: SquadSummary[]
  /** A write is in flight. */
  busy: boolean
  /** Error from the last write attempt. */
  error: string
  run: RunControls
  /** Opens handling the task with a workflow; only the demo offers it, on a person's task. */
  onWorkflow?: () => void
  onStatus: (status: TaskStatusProperty) => void
  onAssign: (next: TaskAssigneeRef | null) => void
}) {
  const finished = isTaskFinished(task.status)
  const agentTask = task.assignee?.type === 'agent'
  // On their own or their squad's task a person picks which Agent does the work; the executor names it.
  const personal = task.assignee?.type === 'human' || task.assignee?.type === 'squad'
  const running = task.activeRun?.state === 'running'
  // An Agent task enters 进行中 by running, not by hand.
  const actions = taskActions(task.status).filter(a => !(agentTask && a.status === 'running'))
  return <div className="mc-side">
    <section className="mc-section" aria-label="任务信息">
      <h3>任务信息</h3>
      <dl className="mc-facts-list">
        <dt>项目</dt><dd>{projectName}</dd>
        <dt>创建人</dt><dd>{task.createdByName}</dd>
        <dt>创建于</dt><dd>{formatTime(task.createdAt)}</dd>
        <dt>更新于</dt><dd>{formatTime(task.updatedAt)}</dd>
      </dl>
    </section>

    <section className="mc-section" aria-label="指派">
      <h3><Users size={14} />指派对象</h3>
      <TaskAssigneePicker tenantId={task.tenantId} squads={squads} value={task.assignee} disabled={busy || running} onAssign={onAssign} />
      <p className="mc-note">当前：{assigneeLabel(task)}{task.assignee && `（${assigneeKindLabel(task)}）`}。{task.assignee?.type === 'squad' && '每个队员都会收到，任一队员都能评论和用 Agent 处理。'}</p>
    </section>

    {runsAgent(task, me, inSquad) && <section className="mc-section" aria-label="执行">
      <h3><Bot size={14} />{personal ? '用 Agent 处理' : '执行'}</h3>
      {finished
        ? <p className="mc-empty-line">任务已结束，不能再执行。</p>
        : <>
          <label className="mc-field-label" htmlFor="mc-executor">{personal ? 'Agent 与模型' : '模型'}</label>
          <NativeSelect id="mc-executor" aria-label="执行模型" value={run.executorId} disabled={running || run.executors.length === 0} onChange={e => run.onExecutor(e.target.value)}>
            {run.executors.length === 0 && <NativeSelectOption value="">暂无可用的执行器</NativeSelectOption>}
            {run.executors.map(x => <NativeSelectOption key={x.id} value={x.id}>{x.name}</NativeSelectOption>)}
          </NativeSelect>
          <div className="mc-actions">
            <Button type="button" size="sm" disabled={running || run.starting || run.executors.length === 0} onClick={run.onStart}>
              {run.hasRuns ? <RotateCcw data-icon="inline-start" /> : <CirclePlay data-icon="inline-start" />}
              {run.starting ? '正在启动…' : run.hasRuns ? '重新执行' : personal ? '用 Agent 处理' : '开始处理'}
            </Button>
            {personal && onWorkflow && <Button type="button" size="sm" variant="outline" disabled={running} onClick={onWorkflow}>
              <Workflow data-icon="inline-start" />用工作流处理
            </Button>}
            {task.activeRun && <Button type="button" size="sm" variant="outline" disabled={run.stopping} onClick={run.onStop}>
              <CircleStop data-icon="inline-start" />{running ? '停止' : '结束会话'}
            </Button>}
          </div>
          <p className="mc-note">
            {running
              ? `${task.activeRun?.startedByName} 发起的运行正在执行；跑完任务自动进入待验收。`
              : task.activeRun
                ? `${task.activeRun.startedByName} 的会话在等追问；重新执行会结束它，用你的身份开一次新运行。`
                : personal
                  ? '选一个 Agent 替你处理这个任务；跑完任务自动进入待验收，全团队都能看到过程。'
                  : '用你的身份运行这个 Agent；跑完任务自动进入待验收，全团队都能看到过程。'}
          </p>
        </>}
    </section>}

    <section className="mc-section" aria-label="推进状态">
      <h3>推进任务</h3>
      {finished
        ? <p className="mc-empty-line">任务已结束，不能再改状态。</p>
        : running
          ? <p className="mc-empty-line">Agent 正在执行，结束后再推进。</p>
          : <>
            <div className="mc-actions">{actions.map(action => {
              const refused = isTaskFinished(action.status) && !canClose
              return <Button key={action.status} type="button" size="sm" variant={action.status === 'cancelled' ? 'outline' : 'default'} disabled={busy || refused} title={refused ? closeRefusal(task, action.label) : undefined} onClick={() => onStatus(action.status)}>{action.label}</Button>
            })}</div>
            {!canClose && <p className="mc-note">{task.status === 'review' ? `等待 ${task.createdByName} 验收。${closeRefusal(task, '确认完成或取消任务')}` : closeRefusal(task, '取消任务')}</p>}
          </>}
    </section>

    {error && <p role="alert" className="mc-error">{error}</p>}
  </div>
}
