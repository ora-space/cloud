import { isAxiosError } from 'axios'
import type { Occurrence, ExecutionResult } from './api/history/generated'
import type { ScheduleResources } from './api/generated'
import { isWorkflowTarget, type Catalog, type Task, type Run } from './schedule-model'

export const emptyResources: ScheduleResources = { projects: [], models: [], executors: [], workflows: [], available: { projects: false, models: false, executors: false, workflows: false } }
export function resourceCatalog(resources: ScheduleResources): Catalog { return { projects: resources.projects, models: resources.models, executors: resources.executors, workflows: resources.workflows, available: resources.available } }
export const runLabels: Record<string,string> = { recorded: '已记录', starting: '正在启动', reconciling: '正在核对', running: '进行中', cancelling: '正在取消', succeeded: '成功', success: '成功', failed: '失败', cancelled: '已取消', interrupted: '已中断', skipped: '已跳过' }
const reasons: Record<string,string> = {
  previous_unfinished: '上一次运行尚未结束', execution_busy: '执行器正忙', server_restarted: '服务重启，本次不补跑', execution_not_started: '本次执行未启动', lookup_required: '正在核对执行结果', configuration_changed: '配置已修改', paused: '计划已暂停', result_incomplete: '回答不完整', model_required: '尚未选择模型', executor_required: '尚未选择执行器', execution_unavailable: '执行服务尚未就绪', execution_target_unavailable: '执行目标已不可用', unsupported_schedule_target: '此执行方式尚未接入', execution_failed: '执行失败', cancelled: '运行已取消',
  workflow_run_failed: '工作流运行失败', workflow_run_not_startable: '工作流不能启动：图里有当前版本还不能执行的节点，或输入不满足开始节点', workflow_unavailable: '工作流或所选版本已不可用', project_unavailable: '项目已不可用', workflow_required: '尚未选择工作流', project_required: '工作流必须关联项目',
}
export function reasonText(reason: string) { return reasons[reason] ?? (reason ? `原因：${reason}` : '') }
export function scheduleError(error: unknown, fallback: string) {
  if (!isAxiosError(error)) return fallback
  const code: string = error.response?.data?.code ?? ''
  const messages: Record<string,string> = {
    schedule_execution_unavailable: '执行服务尚未就绪，请稍后重试。', schedule_resources_unavailable: '模型或执行资源尚未就绪，配置未保存。',
    stop_unconfirmed: '停止尚未确认，任务仍可能运行。请刷新状态或再次取消。', operation_in_progress: '此运行正在处理，请稍后重试同一操作。', operation_incomplete: '操作结果尚未确认，请重试同一请求，不要另建任务。',
    version_conflict: '任务已被其他操作修改，请关闭后重新打开。', execution_conflict: '运行状态已变化，请刷新后重试。', invalid_project_executor: '请选择该项目可用的执行器。',
    execution_busy: '执行器正忙，本次请求已跳过。', execution_target_unavailable: '所选模型或项目当前不可用，本次请求已跳过。',
    invalid_execution_request: '运行配置无效，请检查任务设置后重试。', execution_unsupported: '当前执行方式不受支持，请修改配置后重试。',
    schedule_target_unavailable: '工作流服务尚未就绪，配置未保存。', project_required: '工作流要在项目中运行，请选择项目。',
  }
  if (messages[code]) return messages[code]
  if (error.response?.status === 401) return '登录已失效，请重新登录。'
  if (error.response?.status === 404) return '资源或接口当前不可用，请刷新目录或稍后重试。'
  if (error.response?.status === 503) return '服务尚未就绪，请稍后重试。'
  if (!error.response) return '网络连接中断，结果尚未确认。请重试同一操作。'
  return fallback
}
export function errorOccurrenceId(error: unknown) {
  if (!isAxiosError(error)) return ''
  const id = error.response?.data?.params?.occurrenceId
  return typeof id === 'string' ? id : ''
}
export function isTerminalActionRejection(error: unknown) {
  if (!isAxiosError(error)) return false
  const code: string = error.response?.data?.code ?? ''
  return error.response?.status === 400 || code === 'execution_busy' || code === 'execution_target_unavailable'
}
export function runnable(task: Task, r: ScheduleResources) {
  const target=task.target
  if(isWorkflowTarget(target))return r.available.workflows&&r.available.projects&&r.projects.some(p=>p.id===task.projectId)&&r.workflows.some(w=>w.id===target.workflowId)
  if(target.kind!=='chat')return false
  if(!task.projectId)return r.available.models&&r.models.some(m=>m.id===target.model)
  return r.available.projects&&r.available.executors&&r.projects.some(p=>p.id===task.projectId)&&r.executors.some(e=>e.id===target.executorId&&e.modelId===target.model)
}
export function occurrenceRun(o: Occurrence): Run { return { id:o.id, startedAt:o.scheduledAt,status:o.status,reason:o.reason,released:o.released } }
/** The workflow run a workflow occurrence started, read from its execution reference. */
export function occurrenceWorkflowRun(o: Pick<Occurrence, 'executionRef'> | undefined): string {
  if (!o?.executionRef) return ''
  try {
    const ref: unknown = JSON.parse(o.executionRef)
    return ref !== null && typeof ref === 'object' && typeof (ref as { workflowRunId?: unknown }).workflowRunId === 'string' ? (ref as { workflowRunId: string }).workflowRunId : ''
  } catch {
    return ''
  }
}
export function completeResult(result: ExecutionResult | undefined) { return result?.result?.status === 'completed' && result.result.answerComplete }

const rfc3339Instant = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|[+-]\d{2}:\d{2})$/i

function epochNanos(value: string): bigint | null {
  const match = rfc3339Instant.exec(value)
  if (!match) {
    const ms = Date.parse(value)
    return Number.isNaN(ms) ? null : BigInt(ms) * 1_000_000n
  }
  const wholeMs = Date.parse(`${match[1]}${match[3]}`)
  if (Number.isNaN(wholeMs)) return null
  const subsecNanos = BigInt((match[2] ?? '').padEnd(9, '0'))
  return BigInt(wholeMs) * 1_000_000n + subsecNanos
}

/** Compare RFC3339 timestamps in descending instant order without losing sub-millisecond precision. */
export function compareTimestampsDesc(a: string, b: string): number {
  const na = epochNanos(a)
  const nb = epochNanos(b)
  if (na !== null && nb !== null) return nb > na ? 1 : nb < na ? -1 : 0
  return b > a ? 1 : b < a ? -1 : 0
}

/** Order schedule occurrences by (recordedAt DESC, id DESC) matching the server composite cursor. */
export function compareOccurrences(a: Pick<Occurrence, 'recordedAt' | 'id'>, b: Pick<Occurrence, 'recordedAt' | 'id'>): number {
  const byRecorded = compareTimestampsDesc(a.recordedAt, b.recordedAt)
  if (byRecorded !== 0) return byRecorded
  const aId = a.id.toLowerCase()
  const bId = b.id.toLowerCase()
  return bId > aId ? 1 : bId < aId ? -1 : 0
}

/** Keep a network retry identity across drawer changes and tab reloads, scoped to the user. */
export function manualRequestKey(scope:string,id:string) {
  const key=`ora.schedule.request:${scope}:${id}`
  const previous=sessionStorage.getItem(key)
  if(previous)return previous
  const value=crypto.randomUUID();sessionStorage.setItem(key,value);return value
}
export function finishManualRequest(scope:string,id:string){sessionStorage.removeItem(`ora.schedule.request:${scope}:${id}`)}
