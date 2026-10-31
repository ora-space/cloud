import { codeOf, statusOf } from '@/lib/api-client'
import type { Task } from './tasks-api/generated'

/** Statuses in lifecycle order; the server enforces the same set. */
export const TASK_STATUSES = ['pending', 'running', 'review', 'done', 'cancelled'] as const

/** One task status, as the server names it. */
export type TaskStatus = (typeof TASK_STATUSES)[number]

const STATUS_LABELS: Record<TaskStatus, string> = {
  pending: '待处理',
  running: '进行中',
  review: '待验收',
  done: '已完成',
  cancelled: '已取消',
}

/** Assignee kinds in the words the page uses; unknown kinds fall back to the server value. */
const ASSIGNEE_LABELS: Record<string, string> = { human: '人', agent: 'Agent', squad: '小队' }

/** A status in Chinese; an unknown status is shown verbatim rather than guessed. */
export const taskStatusLabel = (status: string): string => STATUS_LABELS[status as TaskStatus] ?? status

/** Whether a task has reached a status it cannot leave. */
export const isTaskFinished = (status: string): boolean => status === 'done' || status === 'cancelled'

/** One legal manual move out of a status, with the label its button carries. */
export interface TaskAction {
  status: TaskStatus
  label: string
}

const ACTIONS: Record<TaskStatus, TaskAction[]> = {
  pending: [{ status: 'running', label: '开始处理' }, { status: 'cancelled', label: '取消任务' }],
  running: [{ status: 'review', label: '提交验收' }, { status: 'cancelled', label: '取消任务' }],
  review: [{ status: 'done', label: '确认完成' }, { status: 'cancelled', label: '取消任务' }],
  done: [],
  cancelled: [],
}

/**
 * The moves a person may make by hand. This mirrors the server's `CanTransitionTask`: review is
 * never skipped, and a finished task never reopens.
 */
export const taskActions = (status: string): TaskAction[] => ACTIONS[status as TaskStatus] ?? []

/** The assignee's display name, or 未指派; unassigned work stays visible to the whole team. */
export const assigneeLabel = (task: Task): string => task.assignee?.displayName ?? '未指派'

/** The assignee's kind in words, or '' when nothing is assigned. */
export const assigneeKindLabel = (task: Task): string => (task.assignee ? ASSIGNEE_LABELS[task.assignee.type] ?? task.assignee.type : '')

/** The project's name when the loaded list has it, otherwise the id the task carries. */
export const projectLabel = (projects: { id: string; name: string }[], projectId: string): string =>
  projects.find(project => project.id === projectId)?.name ?? projectId

/** Title and description limits, matching the server's boundary checks. */
export const MAX_TASK_TITLE = 200
export const MAX_TASK_DESCRIPTION = 4000

/** Field errors for a task draft; an empty title is the one thing a task cannot lose. */
export function validateTask(title: string, description: string): Record<string, string> {
  const errors: Record<string, string> = {}
  const trimmed = title.trim()
  if (!trimmed) errors.title = '请填写任务标题。'
  else if ([...trimmed].length > MAX_TASK_TITLE) errors.title = `标题最多 ${MAX_TASK_TITLE} 个字符。`
  if ([...description].length > MAX_TASK_DESCRIPTION) errors.description = `描述最多 ${MAX_TASK_DESCRIPTION} 个字符。`
  return errors
}

/**
 * A user-facing reason for a failed task request. A stale version is the common case, so it gets
 * the same wording everywhere: close the panel and reopen it to read the current task.
 */
export function taskMessageFor(error: unknown): string {
  switch (statusOf(error)) {
    case 401: return '登录已过期，请重新登录。'
    case 403: return '你不是这个工作区的成员。'
    case 404: return '任务不存在或你已无权查看。'
    case 409: return '任务已被其他人修改，请关闭后重新打开。'
    case 428: return '缺少版本号，请关闭后重新打开任务。'
  }
  switch (codeOf(error)) {
    case 'invalid_transition': return '这个状态不能直接切过去：按「待处理 → 进行中 → 待验收 → 已完成」推进。'
    case 'invalid_project': return '所选项目不在当前工作区，请重新选择。'
    case 'invalid_assignee': return '只能指派给当前工作区的成员、已安装的 Agent 或未归档的小队。'
    case 'invalid_title': return `请填写任务标题，最多 ${MAX_TASK_TITLE} 个字符。`
    case 'invalid_description': return `描述最多 ${MAX_TASK_DESCRIPTION} 个字符。`
    default: return '操作失败，请重试。'
  }
}
