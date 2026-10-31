import { Bot, MessageSquare, Pencil, Users } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import type { SquadSummary } from './api/generated'
import { formatTime } from './squad-model'
import { TaskAssigneePicker } from './task-assignee'
import { assigneeKindLabel, assigneeLabel, isTaskFinished, taskActions, taskStatusLabel } from './task-model'
import type { Task, TaskAssigneeRef, TaskStatusProperty } from './tasks-api/generated'

/**
 * One task: what it is, where it lives, who owns it and the moves a person may still make. The
 * status moves are manual here; running an agent stays out of this release, so the panel says so
 * instead of offering a button that would not execute anything.
 */
export function TaskDetailView({ task, projectName, squads, busy, error, onEdit, onStatus, onAssign }: {
  task: Task
  /** The project's display name, resolved from the workspace list. */
  projectName: string
  /** Live squads the task may be reassigned to. */
  squads: SquadSummary[]
  /** A write is in flight. */
  busy: boolean
  /** Error from the last write attempt. */
  error: string
  onEdit: () => void
  onStatus: (status: TaskStatusProperty) => void
  onAssign: (next: TaskAssigneeRef | null) => void
}) {
  const actions = taskActions(task.status)
  const finished = isTaskFinished(task.status)
  return <div className="mc-detail">
    <div className="mc-detail-scroll">
      <div className="mc-task-head">
        <span className={`mc-status ${task.status}`}>{taskStatusLabel(task.status)}</span>
        <span className="mc-task-meta">创建人 {task.createdByName} · 更新于 {formatTime(task.updatedAt)}</span>
      </div>

      <p className="mc-description">{task.description || '还没有描述。'}</p>

      <div className="mc-facts">
        <span>项目 <b>{projectName}</b></span>
        <span>指派 <b>{assigneeLabel(task)}</b>{task.assignee && <em>（{assigneeKindLabel(task)}）</em>}</span>
        <span>创建于 <b>{formatTime(task.createdAt)}</b></span>
      </div>

      <section className="mc-section" aria-label="指派">
        <h3><Users size={14} />指派对象</h3>
        <TaskAssigneePicker tenantId={task.tenantId} squads={squads} value={task.assignee} disabled={busy} onAssign={onAssign} />
        <p className="mc-note"><Users size={14} />指派给小队只表示这个队负责，不会自动分派给成员；队里的人可以再把任务转给具体的人或 Agent。</p>
        {task.assignee?.type === 'agent' && <p className="mc-note"><Bot size={14} />指派给 Agent 后需要人点「开始处理」；真正启动 Agent 执行、跑完自动进入待验收尚未接入。</p>}
      </section>

      <section className="mc-section" aria-label="推进状态">
        <h3>推进任务</h3>
        {finished
          ? <p className="mc-empty-line">任务已结束，不能再改状态。</p>
          : <div className="mc-actions">{actions.map(action => <Button key={action.status} type="button" size="sm" variant={action.status === 'cancelled' ? 'outline' : 'default'} disabled={busy} onClick={() => onStatus(action.status)}>{action.label}</Button>)}</div>}
      </section>

      <section className="mc-section" aria-label="评论">
        <h3><MessageSquare size={14} />评论 <Badge variant="secondary">0</Badge></h3>
        <p className="mc-comment-placeholder">评论区留在这里；评论写入、执行记录与运行详情在后续版本接入。</p>
      </section>

      {error && <p role="alert" className="mc-error">{error}</p>}
    </div>
    <footer className="mc-detail-footer">
      <span className="mc-muted">版本 {task.version}</span>
      <Button type="button" variant="outline" size="sm" onClick={onEdit} disabled={busy}><Pencil size={14} />编辑任务</Button>
    </footer>
  </div>
}
