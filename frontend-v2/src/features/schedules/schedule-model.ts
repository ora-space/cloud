/** Schedule-owned configuration and read-only legacy target metadata. */
export type Rule = { kind: 'daily' | 'weekdays' | 'weekly' | 'hours'; time: string; days: number[]; hours: number; timezone: string }
export type ChatTarget = { kind: 'chat'; prompt: string; agent: string; model: string; executorId?: string }
/** A workflow run in the schedule's project: the published snapshot frozen at save, and its kickoff input. */
export type WorkflowTarget = { kind: 'workflow'; workflowId: string; snapshotId: string; input: string }
export type LegacyTarget = { kind: 'workflow' | 'task'; [key:string]:unknown }
export type Target = ChatTarget | WorkflowTarget | LegacyTarget
/** A workflow target this version can run, as opposed to an older reference kept read-only. */
export function isWorkflowTarget(target: Target): target is WorkflowTarget {
  return target.kind === 'workflow' && typeof target.workflowId === 'string' && typeof target.snapshotId === 'string'
}
export type Run = { id: string; startedAt: string; status: 'recorded'|'starting'|'reconciling'|'running'|'cancelling'|'succeeded'|'success'|'failed'|'cancelled'|'interrupted'|'skipped'; reason?:string;released?:boolean }
export type Task = { version?: number; id: string; name: string; projectId: string; target: Target; rule: Rule; enabled: boolean; createdAt: string; runs: Run[] }
export type Draft = { name:string;kind:Target['kind'];projectId:string;prompt:string;agent:string;model:string;executorId?:string;workflowId:string;snapshotId:string;input:string;rule:Rule }
export type Catalog = { projects:{id:string;name:string;description:string}[];models?:{id:string;name:string}[];executors?:{id:string;name:string;agentName:string;modelId:string}[];workflows?:{id:string;name:string;version:string;snapshotId:string}[];available?:{projects:boolean;models:boolean;executors:boolean;workflows?:boolean} }
export const catalog: Catalog = {projects:[]}
export const weekdayNames = ['日', '一', '二', '三', '四', '五', '六']
export function emptyDraft(): Draft {
  return { name: '', kind: 'chat', projectId: '', prompt: '', agent: '', model: '', workflowId: '', snapshotId: '', input: '', rule: { kind: 'daily', time: '09:00', days: [1], hours: 6, timezone: 'Asia/Shanghai' } }
}
export function taskDraft(task: Task): Draft {
  const d = emptyDraft()
  const t = task.target
  const target = t.kind === 'chat' ? t : isWorkflowTarget(t) ? { workflowId: t.workflowId, snapshotId: t.snapshotId, input: t.input ?? '' } : {}
  return { ...d, name: task.name, projectId: task.projectId, kind: t.kind, rule: structuredClone(task.rule), ...target }
}
/** Whether a draft's target can be saved: a conversation, or a workflow; older references stay read-only. */
export function editableDraft(d: Draft, task?: Task) {
  return d.kind === 'chat' || (d.kind === 'workflow' && (!task || task.target.kind !== 'workflow' || isWorkflowTarget(task.target)))
}
export function validateDraft(d: Draft, data: Catalog = catalog): Record<string, string> {
  const errors: Record<string, string> = {}
  if (!d.name.trim()) errors.name = '请输入任务名称'
  if (d.kind === 'chat' && !d.prompt.trim()) errors.prompt = '请告诉 AI 每次需要做什么'
  if (d.kind === 'task') errors.target = '此执行方式尚未接入，原配置仅供查看'
  if (d.kind === 'workflow') {
    if (!d.workflowId) errors.workflowId = '请选择要运行的工作流'
    else if (!data.workflows?.some(w => w.id === d.workflowId)) errors.workflowId = '该工作流已不可用或没有发布版本，请重新选择'
    if (!d.projectId) errors.projectId = '工作流要在项目中运行，请选择项目'
  }
  if (d.kind !== 'task' && d.projectId && !data.projects.some(p => p.id === d.projectId)) errors.projectId = '该项目已不可用，请重新选择'
  if (d.rule.kind === 'weekly' && !d.rule.days.length) errors.days = '请至少选择一天'
  if (d.rule.kind !== 'hours' && !/^([01]\d|2[0-3]):[0-5]\d$/.test(d.rule.time)) errors.time = '请输入有效时间'
  if (d.rule.kind === 'hours' && (!Number.isInteger(d.rule.hours) || d.rule.hours < 1 || d.rule.hours > 24)) errors.hours = '间隔应为 1–24 小时的整数'
  try { new Intl.DateTimeFormat('zh-CN', { timeZone: d.rule.timezone }).format() } catch { errors.timezone = '请选择有效时区' }
  return errors
}
/** The target to save. A workflow freezes the version published right now; a later one applies once the task is saved again. */
export function targetFromDraft(d: Draft, data: Catalog = catalog): ChatTarget | WorkflowTarget {
  if (d.kind === 'workflow') {
    const workflow = data.workflows?.find(w => w.id === d.workflowId)
    if (!workflow) throw new Error('所选工作流已不可用')
    return { kind: 'workflow', workflowId: workflow.id, snapshotId: workflow.snapshotId, input: d.input }
  }
  if(d.kind!=='chat')throw new Error('引用目标只读，等待提供方接口')
  return {kind:'chat',prompt:d.prompt.trim(),agent:d.agent,model:d.model,...(d.executorId?{executorId:d.executorId}:{})}
}
export function projectName(id: string, data:Catalog=catalog) { return !id?'不关联项目':data.projects.find(p => p.id === id)?.name ?? '项目暂不可用' }
export function ruleText(rule: Rule) {
  if (rule.kind === 'hours') return `每隔 ${rule.hours} 小时`
  const prefix = rule.kind === 'daily' ? '每天' : rule.kind === 'weekdays' ? '每个工作日' : `每周${[...rule.days].sort((a,b) => ((a+6)%7)-((b+6)%7)).map(d => weekdayNames[d]).join('、')}`
  return `${prefix} ${rule.time}`
}
export function summary(d:Draft,data:Catalog=catalog) {
  if(d.kind==='workflow'){
    const workflow=data.workflows?.find(w=>w.id===d.workflowId)
    return workflow?`${ruleText(d.rule)}，在「${projectName(d.projectId,data)}」中运行工作流「${workflow.name}」（${workflow.version}）。`:'选择项目和已发布的工作流后预览。'
  }
  if(d.kind!=='chat')return '原引用配置已保留；此执行方式尚未接入。'
  return `${ruleText(d.rule)}，${d.projectId?`在「${projectName(d.projectId,data)}」中`:'不关联项目，'}新建对话执行任务指令。`
}
function parts(date: Date, timezone: string) {
  const p = new Intl.DateTimeFormat('en-CA', { timeZone: timezone, year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23' }).formatToParts(date)
  const n = (key: string) => Number(p.find(v => v.type === key)?.value)
  return { year: n('year'), month: n('month'), day: n('day'), hour: n('hour'), minute: n('minute'), second: n('second') }
}
/** Calendar schedules use wall time in the selected zone; gaps at DST are skipped. */
export function nextTimes(rule: Rule, now = new Date(), anchor = now): Date[] {
  if (rule.kind === 'hours') {
    const interval = rule.hours * 3_600_000
    if (!Number.isFinite(interval) || interval < 3_600_000) return []
    const first = anchor.getTime() + Math.max(1, Math.floor((now.getTime() - anchor.getTime()) / interval) + 1) * interval
    return [0, 1, 2].map(i => new Date(first + i * interval))
  }
  if (!/^([01]\d|2[0-3]):[0-5]\d$/.test(rule.time) || (rule.kind === 'weekly' && !rule.days.length)) return []
  const local = parts(now, rule.timezone)
  const [hour, minute] = rule.time.split(':').map(Number)
  const result: Date[] = []
  for (let day = 0; day < 24 && result.length < 3; day++) {
    const calendar = new Date(Date.UTC(local.year, local.month - 1, local.day + day, hour, minute))
    const weekday = calendar.getUTCDay()
    if (rule.kind === 'weekdays' && (weekday === 0 || weekday === 6)) continue
    if (rule.kind === 'weekly' && !rule.days.includes(weekday)) continue
    let candidate = calendar.getTime()
    for (let i = 0; i < 4; i++) {
      const p = parts(new Date(candidate), rule.timezone)
      const wall = Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second)
      candidate += calendar.getTime() - wall
    }
    const p = parts(new Date(candidate), rule.timezone)
    if (p.day === calendar.getUTCDate() && p.hour === hour && p.minute === minute && candidate > now.getTime()) result.push(new Date(candidate))
  }
  return result
}
export function formatTime(value: Date | string, timezone = 'Asia/Shanghai') {
  return new Intl.DateTimeFormat('zh-CN', { timeZone: timezone, month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23' }).format(new Date(value))
}
