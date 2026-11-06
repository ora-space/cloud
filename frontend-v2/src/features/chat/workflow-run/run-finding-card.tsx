import { useState, type ReactNode } from 'react'
import {
  ArrowUpRight,
  Check,
  CheckCircle2,
  ChevronDown,
  Link2,
  Plus,
  ShieldCheck,
  TriangleAlert,
  X,
  XCircle,
} from 'lucide-react'
import { Link } from 'react-router'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { TaskAssigneePicker } from '@/components/task-assignee-picker'
import type { SquadSummary } from '@/lib/squads-api/generated'
import type { TaskAssigneeRef } from '@/features/collaboration/tasks-api/generated'
import type {
  WorkflowFormExistingTask,
  WorkflowFormField,
  WorkflowFormFinding,
  WorkflowFormTaskResult,
} from '@/lib/workflows/api/generated'
import { cn } from '@/lib/utils'

/** One editable row: only the decision, its owner and the configured extra values. */
export type FormAnswer = {
  key: string
  decision: string
  assignee?: TaskAssigneeRef
  values: Record<string, string>
}

type Roster = { userId: string; displayName: string }

const text = (value: unknown) => (typeof value === 'string' ? value : '')

/** Severity stays legible without colour: the label carries the level. */
function severityTone(severity: string) {
  if (severity.includes('高') || /critical|high/i.test(severity)) return 'border-destructive/30 bg-destructive/10 text-destructive'
  if (severity.includes('中') || /medium/i.test(severity)) return 'border-warning/35 bg-warning/10 text-warning'
  if (severity.includes('低') || /low/i.test(severity)) return 'border-info/30 bg-info/10 text-info'
  return 'border-border bg-muted text-muted-foreground'
}

const decisionLabel = (decision: string) => decision === 'confirmed' ? '是问题' : decision === 'rejected' ? '不是问题' : '待确认'

type OwnerRef = { type?: string; id?: string; displayName?: string }

const existingStatus = (status: string) => status === 'pending' ? '待处理' : status === 'running' ? '进行中' : status === 'review' ? '待验收' : status === 'done' ? '已完成' : status === 'cancelled' ? '已取消' : ''

/** Names an existing task's owner from the frozen read first, then this workspace's roster. */
function assigneeName(assignee: OwnerRef | null | undefined, roster: Roster[], squads: SquadSummary[]) {
  if (!assignee) return '未指派'
  if (assignee.displayName) return assignee.displayName
  if (assignee.type === 'human') return roster.find(member => member.userId === assignee.id)?.displayName ?? '工作区成员'
  if (assignee.type === 'squad') return squads.find(squad => squad.id === assignee.id)?.name ?? '小队'
  return '空间 Agent'
}

/** Two explicit buttons instead of a select: the primary decision is one click, always visible. */
function DecisionToggle({ decision, disabled, onDecide }: { decision: string; disabled: boolean; onDecide: (decision: string) => void }) {
  const option = (value: 'confirmed' | 'rejected', label: string, Icon: typeof Check) => {
    const active = decision === value
    return <button
      type="button"
      aria-pressed={active}
      disabled={disabled}
      onClick={() => onDecide(value)}
      className={cn(
        'inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium transition-colors',
        'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50',
        active
          ? value === 'confirmed'
            ? 'bg-success/15 text-success shadow-xs ring-1 ring-success/25'
            : 'bg-secondary text-secondary-foreground shadow-xs ring-1 ring-border/70'
          : 'text-muted-foreground hover:text-foreground',
      )}
    >
      <Icon className="size-3" aria-hidden />{label}
    </button>
  }
  return <div role="group" aria-label="是否为问题" className="inline-flex shrink-0 items-center gap-0.5 rounded-lg border bg-muted/40 p-0.5">
    {option('confirmed', '是问题', Check)}
    {option('rejected', '不是问题', X)}
  </div>
}

function DecisionBadge({ decision }: { decision: string }) {
  if (decision === 'confirmed') return <Badge variant="outline" className="shrink-0 gap-1 border-success/30 bg-success/10 text-success"><CheckCircle2 />是问题</Badge>
  if (decision === 'rejected') return <Badge variant="outline" className="shrink-0 gap-1 border-border text-muted-foreground"><XCircle />不是问题</Badge>
  return <Badge variant="outline" className="shrink-0 gap-1 border-warning/35 bg-warning/10 text-warning">{decisionLabel(decision)}</Badge>
}

function FieldControl({ field, value, invalid, disabled, onChange }: {
  field: WorkflowFormField
  value: string
  invalid: boolean
  disabled: boolean
  onChange: (value: string) => void
}) {
  const id = `field-${field.name}`
  if (field.type === 'select') return <NativeSelect id={id} aria-invalid={invalid} disabled={disabled} value={value} onChange={event => onChange(event.target.value)}>
    <NativeSelectOption value="">请选择</NativeSelectOption>
    {(Array.isArray(field.options) ? field.options : []).map(option => <NativeSelectOption key={String(option)} value={String(option)}>{String(option)}</NativeSelectOption>)}
  </NativeSelect>
  if (field.type === 'textarea') return <Textarea id={id} aria-invalid={invalid} disabled={disabled} value={value} onChange={event => onChange(event.target.value)} rows={2} className="min-h-16" />
  return <Input id={id} aria-invalid={invalid} disabled={disabled} value={value} onChange={event => onChange(event.target.value)} />
}

/**
 * One finding, in the shape it is being used: a decision row while the form is open, a
 * conclusion when it is not. Evidence folds away; the owner and the note appear only once
 * they mean something, so the list stays scannable at 3 or at 30 rows.
 */
export function RunFindingCard({
  finding, answer, fields, existing, result, receipt, errors, readOnly, editable, tenantId, squads, roster, onDecide, onPatch,
}: {
  finding: WorkflowFormFinding
  answer: FormAnswer
  fields: WorkflowFormField[]
  existing?: WorkflowFormExistingTask
  result?: WorkflowFormTaskResult
  /** A simulated external receipt that belongs to this row's outcome, when the run produced one. */
  receipt?: ReactNode
  errors: Record<string, string>
  /** A write is in flight or the form is no longer the caller's to edit. */
  readOnly: boolean
  /** The form is open; controls render even when the caller cannot submit. */
  editable: boolean
  tenantId: string
  squads: SquadSummary[]
  roster: Roster[]
  onDecide: (decision: string) => void
  onPatch: (change: Partial<FormAnswer>) => void
}) {
  const [revealFields, setRevealFields] = useState(false)
  const key = text(finding.key)
  const severity = text(finding.severity)
  const module = text(finding.module)
  const decision = answer.decision
  const confirmed = decision === 'confirmed'
  const rejected = decision === 'rejected'
  const pending = !confirmed && !rejected
  // A rejection needs no owner: the platform keeps the evidence, not a task.
  const showOwner = editable && confirmed
  const valueOf = (field: WorkflowFormField) => answer.values[text(field.name)] ?? ''
  const invalid = (field: WorkflowFormField) => Boolean(errors[`${key}:${text(field.name)}`])
  /** A submitted review is read as conclusions: values become text, not disabled inputs. */
  const conclusion = !editable
  const hidden = fields.filter(field => !conclusion && field.type === 'textarea' && !valueOf(field) && !invalid(field))
  const visible = conclusion
    ? fields.filter(field => field.type === 'readonly' || valueOf(field).length > 0)
    : fields.filter(field => !hidden.includes(field) || revealFields)
  const owner = existing?.assignee ?? answer.assignee

  return <article
    aria-label={text(finding.title) || '未命名问题'}
    className={cn(
      'overflow-hidden rounded-xl border bg-card transition-colors',
      pending && editable && 'border-warning/35',
      confirmed && 'border-success/25',
      readOnly && 'bg-card/80',
    )}
  >
    <div className="flex flex-col gap-2 px-4 pt-3.5 sm:flex-row sm:items-start sm:justify-between sm:gap-4">
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          <span className={cn('inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium', severityTone(severity))}>
            <TriangleAlert className="size-3" aria-hidden />{severity || '未定级'}
          </span>
          {module && <code className="max-w-[14rem] truncate rounded bg-muted px-1.5 py-0.5 font-mono text-[11px] text-muted-foreground">{module}</code>}
          {pending && editable && <span className="inline-flex items-center rounded-md border border-warning/35 bg-warning/10 px-1.5 py-0.5 text-[11px] font-medium text-warning">待确认</span>}
        </div>
        <h3 className="mt-1.5 text-sm font-semibold leading-snug">{text(finding.title) || '未命名问题'}</h3>
        {text(finding.description) && <p className="mt-1 line-clamp-2 text-sm leading-5 text-muted-foreground">{text(finding.description)}</p>}
      </div>
      {editable ? <DecisionToggle decision={decision} disabled={readOnly} onDecide={onDecide} /> : <DecisionBadge decision={decision} />}
    </div>

    <details className="group mx-4 mt-2.5 rounded-lg border bg-muted/15">
      <summary className="flex cursor-pointer list-none items-center gap-1.5 px-2.5 py-1.5 text-[11px] font-medium text-muted-foreground">
        <ShieldCheck className="size-3.5" aria-hidden />证据
        <ChevronDown className="ml-auto size-3.5 transition-transform group-open:rotate-180" aria-hidden />
      </summary>
      <p className="whitespace-pre-wrap border-t px-2.5 py-2 text-xs leading-5">{text(finding.evidence) || '这次没有提供可展开的证据。'}</p>
    </details>

    {visible.length > 0 && <div className={cn('mx-4 mt-2.5 grid gap-2', conclusion ? 'rounded-lg border bg-muted/15 px-3 py-2.5' : 'sm:grid-cols-2')}>
      {visible.map(field => {
        const name = text(field.name)
        if (conclusion) return <div key={name} className="min-w-0">
          <p className="text-[11px] font-medium text-muted-foreground">{text(field.label)}</p>
          <p className="mt-0.5 whitespace-pre-wrap text-sm">{field.type === 'readonly' ? text((finding as unknown as Record<string, unknown>)[name]) : valueOf(field)}</p>
        </div>
        return <div key={name} className={cn('min-w-0', field.type === 'textarea' && 'sm:col-span-2')}>
          <label htmlFor={`field-${name}`} className="text-[11px] font-medium text-muted-foreground">{text(field.label)}{field.required === true ? ' *' : ''}</label>
          <div className="mt-1">
            <FieldControl field={field} value={valueOf(field)} invalid={invalid(field)} disabled={readOnly} onChange={value => onPatch({ values: { ...answer.values, [name]: value } })} />
          </div>
          {invalid(field) && <p role="alert" className="mt-1 text-xs text-destructive">请填写这一项。</p>}
        </div>
      })}
    </div>}

    {(showOwner || (existing !== undefined && result === undefined && !rejected) || (conclusion && (confirmed || rejected)) || (hidden.length > 0 && !revealFields)) && <div className="mt-2.5 flex flex-wrap items-center gap-x-4 gap-y-2 border-t bg-muted/10 px-4 py-2.5">
      {showOwner && !existing && <div className="flex min-w-0 items-center gap-2">
        <span className="text-[11px] font-medium text-muted-foreground">负责人</span>
        <div className="w-40"><TaskAssigneePicker tenantId={tenantId} squads={squads} value={answer.assignee ?? null} disabled={readOnly} onAssign={assignee => onPatch({ assignee: assignee ?? undefined })} /></div>
        {errors[`${key}:assignee`] && <p role="alert" className="text-xs text-destructive">请选择负责人。</p>}
      </div>}
      {existing !== undefined && result === undefined && !rejected && <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1 text-xs">
        <span className="inline-flex items-center gap-1 rounded-md bg-info-soft px-2 py-1 font-medium text-info"><Link2 className="size-3" aria-hidden />关联到已有任务</span>
        <code className="font-mono text-[11px] text-muted-foreground">#{text(existing.taskId).slice(0, 8)}</code>
        {existing.status && <span className="text-muted-foreground">· {existingStatus(text(existing.status))}</span>}
        <span className="text-muted-foreground">· 负责人</span>
        <span className="font-medium">{assigneeName(existing.assignee, roster, squads)}</span>
        <Link className="inline-flex items-center gap-0.5 font-medium underline underline-offset-2" to={`/collaboration/tasks/${text(existing.taskId)}`}>查看任务<ArrowUpRight className="size-3" aria-hidden /></Link>
      </div>}
      {conclusion && confirmed && <p className="flex min-w-0 items-center gap-2 text-xs"><span className="text-muted-foreground">负责人</span><span className="font-medium">{assigneeName(owner, roster, squads)}</span></p>}
      {conclusion && rejected && <p className="text-xs text-muted-foreground">已确认无需处理，不会创建任务。</p>}
      {hidden.length > 0 && !revealFields && <button type="button" disabled={readOnly} onClick={() => setRevealFields(true)} className="ml-auto inline-flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground disabled:pointer-events-none disabled:opacity-50"><Plus className="size-3" aria-hidden />添加{text(hidden[0]?.label) || '备注'}</button>}
    </div>}

    {result && <div className="mx-4 mb-3 mt-2 flex flex-wrap items-center gap-2 rounded-lg border border-success/25 bg-success-soft px-3 py-2 text-xs">
      <CheckCircle2 className="size-3.5 text-success" aria-hidden />
      <span className="font-medium text-success">{result.action === 'created' ? '已创建任务' : '已关联现有任务'}</span>
      <Link className="inline-flex items-center gap-0.5 font-medium underline underline-offset-2" to={`/collaboration/tasks/${text(result.taskId)}`}>打开任务<ArrowUpRight className="size-3" aria-hidden /></Link>
      {receipt !== undefined && <div className="ml-auto">{receipt}</div>}
    </div>}
  </article>
}
