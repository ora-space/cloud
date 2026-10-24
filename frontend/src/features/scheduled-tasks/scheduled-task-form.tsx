import { useState } from 'react'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import type {
  CollaborationTarget,
  Issue,
  ScheduledTaskSchedule,
  ScheduledTaskScheduleKind,
  ScheduledTaskScheduleWeekdaysItem,
} from '@/api/generated.schemas'
import {
  scheduledTaskFailure,
  useSaveScheduledTask,
  useScheduledTaskAgents,
  useScheduledTaskIssues,
  type ScheduledTask,
} from './api'

const selectClass =
  'flex h-9 w-full rounded-md border border-input bg-transparent px-3 py-1 text-sm shadow-xs'

/** D5 无人值守授权的风险说明：必须显式同意后才能启用。 */
const CONSENT_RISK_STATEMENT =
  '我已知悉并同意：定时运行在一次性隔离 Workspace 中全自动执行（非用户可见的数据边界承诺），Agent 可执行任意工具调用，消耗创建者个人模型额度，并以创建者 Git 身份提交。'

const KIND_OPTIONS: readonly { value: ScheduledTaskScheduleKind; label: string }[] = [
  { value: 'daily', label: '每天' },
  { value: 'weekly', label: '每周' },
  { value: 'every_hours', label: '每 N 小时' },
]

const WEEKDAY_OPTIONS: readonly {
  value: ScheduledTaskScheduleWeekdaysItem
  label: string
}[] = [
  { value: 'sunday', label: '周日' },
  { value: 'monday', label: '周一' },
  { value: 'tuesday', label: '周二' },
  { value: 'wednesday', label: '周三' },
  { value: 'thursday', label: '周四' },
  { value: 'friday', label: '周五' },
  { value: 'saturday', label: '周六' },
]

function Field({
  label,
  htmlFor,
  children,
}: {
  label: string
  htmlFor: string
  children: React.ReactNode
}) {
  return (
    <div className="space-y-1.5">
      <Label htmlFor={htmlFor}>{label}</Label>
      {children}
    </div>
  )
}

function ScheduleEditor({
  schedule,
  onChange,
}: {
  schedule: ScheduledTaskSchedule
  onChange: (schedule: ScheduledTaskSchedule) => void
}) {
  const pick = (value: string) => {
    if (value === 'every_hours') onChange({ kind: 'every_hours', intervalHours: 1 })
    else if (value === 'weekly')
      onChange({ kind: 'weekly', hour: 9, minute: 0, weekdays: ['monday'] })
    else onChange({ kind: 'daily', hour: 9, minute: 0 })
  }
  const toggleWeekday = (value: ScheduledTaskScheduleWeekdaysItem) => {
    const current = schedule.weekdays ?? []
    const next = current.includes(value)
      ? current.filter((day) => day !== value)
      : [...current, value]
    onChange({ ...schedule, weekdays: next })
  }
  return (
    <>
      <Field label="计划" htmlFor="st-kind">
        <select
          id="st-kind"
          className={selectClass}
          value={schedule.kind}
          onChange={(e) => pick(e.target.value)}
        >
          {KIND_OPTIONS.map((option) => (
            <option key={option.value} value={option.value}>
              {option.label}
            </option>
          ))}
        </select>
      </Field>

      {schedule.kind === 'every_hours' && (
        <Field label="间隔（小时）" htmlFor="st-interval">
          <Input
            id="st-interval"
            type="number"
            min={1}
            value={schedule.intervalHours ?? 1}
            onChange={(e) =>
              onChange({ kind: 'every_hours', intervalHours: Number(e.target.value) })
            }
          />
        </Field>
      )}

      {schedule.kind !== 'every_hours' && (
        <div className="flex gap-3">
          <div className="flex-1 space-y-1.5">
            <Label htmlFor="st-hour">时</Label>
            <Input
              id="st-hour"
              type="number"
              min={0}
              max={23}
              value={schedule.hour ?? 0}
              onChange={(e) => onChange({ ...schedule, hour: Number(e.target.value) })}
            />
          </div>
          <div className="flex-1 space-y-1.5">
            <Label htmlFor="st-minute">分</Label>
            <Input
              id="st-minute"
              type="number"
              min={0}
              max={59}
              value={schedule.minute ?? 0}
              onChange={(e) => onChange({ ...schedule, minute: Number(e.target.value) })}
            />
          </div>
        </div>
      )}

      {schedule.kind === 'weekly' && (
        <Field label="星期" htmlFor="st-weekdays">
          <div className="flex flex-wrap gap-2">
            {WEEKDAY_OPTIONS.map((day) => (
              <label
                key={day.value}
                className="flex cursor-pointer items-center gap-1.5 rounded-md border px-2 py-1 text-xs"
              >
                <input
                  type="checkbox"
                  checked={(schedule.weekdays ?? []).includes(day.value)}
                  onChange={() => toggleWeekday(day.value)}
                />
                {day.label}
              </label>
            ))}
          </div>
        </Field>
      )}
    </>
  )
}

function Fields({
  isEdit,
  issues,
  agents,
  name,
  setName,
  issueId,
  setIssueId,
  agentId,
  setAgentId,
  prompt,
  setPrompt,
  timezone,
  setTimezone,
  schedule,
  setSchedule,
  enabled,
  setEnabled,
  consented,
  setConsented,
}: {
  isEdit: boolean
  issues: Issue[]
  agents: CollaborationTarget[]
  name: string
  setName: (value: string) => void
  issueId: string
  setIssueId: (value: string) => void
  agentId: string
  setAgentId: (value: string) => void
  prompt: string
  setPrompt: (value: string) => void
  timezone: string
  setTimezone: (value: string) => void
  schedule: ScheduledTaskSchedule
  setSchedule: (value: ScheduledTaskSchedule) => void
  enabled: boolean
  setEnabled: (value: boolean) => void
  consented: boolean
  setConsented: (value: boolean) => void
}) {
  return (
    <div className="space-y-4">
      <Field label="名称" htmlFor="st-name">
        <Input id="st-name" value={name} onChange={(e) => setName(e.target.value)} />
      </Field>

      <TargetSelectors
        isEdit={isEdit}
        issues={issues}
        agents={agents}
        issueId={issueId}
        setIssueId={setIssueId}
        agentId={agentId}
        setAgentId={setAgentId}
      />

      <Field label="提示词" htmlFor="st-prompt">
        <Textarea
          id="st-prompt"
          rows={4}
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
        />
      </Field>

      <Field label="时区（IANA）" htmlFor="st-timezone">
        <Input
          id="st-timezone"
          value={timezone}
          onChange={(e) => setTimezone(e.target.value)}
          placeholder="Asia/Shanghai"
        />
      </Field>

      <ScheduleEditor schedule={schedule} onChange={setSchedule} />

      {!isEdit && (
        <label className="flex items-center gap-2 text-sm">
          <Switch checked={enabled} onCheckedChange={setEnabled} />
          创建后立即启用
        </label>
      )}

      {enabled && <ConsentAgreement consented={consented} setConsented={setConsented} />}
    </div>
  )
}

/** The issue and agent target selectors; the issue selector is only shown on creation. */
function TargetSelectors({
  isEdit,
  issues,
  agents,
  issueId,
  setIssueId,
  agentId,
  setAgentId,
}: {
  isEdit: boolean
  issues: Issue[]
  agents: CollaborationTarget[]
  issueId: string
  setIssueId: (value: string) => void
  agentId: string
  setAgentId: (value: string) => void
}) {
  return (
    <>
      {!isEdit && (
        <Field label="绑定 issue" htmlFor="st-issue">
          <select
            id="st-issue"
            className={selectClass}
            value={issueId}
            onChange={(e) => setIssueId(e.target.value)}
          >
            <option value="">选择 issue…</option>
            {issues.map((issue) => (
              <option key={issue.id} value={issue.id}>
                #{issue.number} {issue.title}
              </option>
            ))}
          </select>
        </Field>
      )}
      <Field label="执行智能体" htmlFor="st-agent">
        <select
          id="st-agent"
          className={selectClass}
          value={agentId}
          onChange={(e) => setAgentId(e.target.value)}
        >
          <option value="">选择智能体…</option>
          {agents.map((agent) => (
            <option key={agent.id} value={agent.id}>
              {agent.displayName}
            </option>
          ))}
        </select>
      </Field>
    </>
  )
}

/** The D5 unattended-consent risk statement and its required agreement checkbox. */
function ConsentAgreement({
  consented,
  setConsented,
}: {
  consented: boolean
  setConsented: (value: boolean) => void
}) {
  return (
    <label className="flex items-start gap-2 rounded-md border border-muted p-3 text-xs text-muted-foreground">
      <Checkbox
        checked={consented}
        onCheckedChange={(checked) => setConsented(checked)}
        aria-label="同意无人值守授权"
      />
      <span>{CONSENT_RISK_STATEMENT}</span>
    </label>
  )
}

function toDraft(task: ScheduledTask | undefined) {
  return {
    name: task ? task.name : '',
    issueId: task ? task.issueId : '',
    agentId: task ? task.agentId : '',
    prompt: task ? task.prompt : '',
    timezone: task ? task.timezone : 'Asia/Shanghai',
    schedule: task
      ? task.schedule
      : ({ kind: 'daily', hour: 9, minute: 0 } satisfies ScheduledTaskSchedule),
  }
}

function isDraftInvalid(
  isEdit: boolean,
  fields: { name: string; prompt: string; agentId: string; issueId: string },
) {
  return (
    fields.name.trim() === '' ||
    fields.prompt.trim() === '' ||
    fields.agentId === '' ||
    (!isEdit && fields.issueId === '')
  )
}

/** Collects one definition and its consent, then closes only after Cloud accepts the save. */
export function ScheduledTaskForm({
  tid,
  task,
  onClose,
}: {
  tid: string
  task: ScheduledTask | undefined
  onClose: () => void
}) {
  const isEdit = task != null
  const issues = useScheduledTaskIssues(tid)
  const agents = useScheduledTaskAgents(tid)
  const save = useSaveScheduledTask(tid, task)

  const draft = toDraft(task)
  const [name, setName] = useState(draft.name)
  const [issueId, setIssueId] = useState(draft.issueId)
  const [agentId, setAgentId] = useState(draft.agentId)
  const [prompt, setPrompt] = useState(draft.prompt)
  const [timezone, setTimezone] = useState(draft.timezone)
  const [schedule, setSchedule] = useState<ScheduledTaskSchedule>(draft.schedule)
  const [enabled, setEnabled] = useState(false)
  const [consented, setConsented] = useState(false)

  const submit = () =>
    save.mutate(
      { issueId, agentId, name, prompt, schedule, timezone, enabled },
      { onSuccess: onClose },
    )
  const invalid =
    isDraftInvalid(isEdit, { name, prompt, agentId, issueId }) || (enabled && !consented)
  let submitLabel = '创建'
  if (save.isPending) submitLabel = '保存中…'
  else if (isEdit) submitLabel = '保存'

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent className="max-h-[85vh] overflow-y-auto sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>{isEdit ? '编辑定时任务' : '新建定时任务'}</DialogTitle>
          <DialogDescription>
            定时任务在到达计划时刻时，以创建者的无人值守授权在一次性隔离运行中执行绑定 issue。
          </DialogDescription>
        </DialogHeader>

        <Fields
          isEdit={isEdit}
          issues={issues.data ?? []}
          agents={agents.data ?? []}
          name={name}
          setName={setName}
          issueId={issueId}
          setIssueId={setIssueId}
          agentId={agentId}
          setAgentId={setAgentId}
          prompt={prompt}
          setPrompt={setPrompt}
          timezone={timezone}
          setTimezone={setTimezone}
          schedule={schedule}
          setSchedule={setSchedule}
          enabled={enabled}
          setEnabled={setEnabled}
          consented={consented}
          setConsented={setConsented}
        />

        {save.isError && (
          <p role="alert" className="text-xs text-destructive">
            {scheduledTaskFailure(save.error)}
          </p>
        )}

        <DialogFooter>
          <Button type="button" variant="outline" onClick={onClose}>
            取消
          </Button>
          <Button type="button" disabled={invalid || save.isPending} onClick={submit}>
            {submitLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
