import { useState } from 'react'
import { isAxiosError } from 'axios'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  CheckCheck,
  CircleDashed,
  ClipboardCheck,
  Info,
  RotateCcw,
  Save,
  Send,
  ShieldAlert,
  TriangleAlert,
  XCircle,
} from 'lucide-react'
import { useGetApiV1TenantsTidMembers } from '@/api/members/members'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { getWorkflowForm, saveWorkflowFormDraft, submitWorkflowForm, retryWorkflowFormTasks, type WorkflowForm } from '@/lib/workflows/api/generated'
import { listSquads } from '@/lib/squads-api/generated'
import type { TaskAssigneeRef } from '@/features/collaboration/tasks-api/generated'
import { DtsReceipt } from './dts-receipt'
import { RunFindingCard, type FormAnswer } from './run-finding-card'
import { RunReportPanel } from './run-report-panel'
import { cn } from '@/lib/utils'

type FindingFilter = 'all' | 'pending' | 'confirmed' | 'rejected'

const text = (value: unknown) => typeof value === 'string' ? value : ''

const assigneeRef = (value: { type?: string; id?: string } | null | undefined): TaskAssigneeRef | undefined => {
  if (value?.id === undefined || (value.type !== 'human' && value.type !== 'agent' && value.type !== 'squad')) return undefined
  return { type: value.type, id: value.id }
}

/** The wire accepts a typed reference only; display fields are server-side reads. */
const wireAnswers = (answers: FormAnswer[]) => answers.map(answer => ({
  key: answer.key,
  decision: answer.decision,
  ...(answer.assignee === undefined ? {} : { assignee: { type: answer.assignee.type, id: answer.assignee.id } }),
  values: answer.values,
}))

function readAnswers(form: WorkflowForm): FormAnswer[] {
  return form.findings.map(row => {
    const saved = form.answers.find(answer => answer.key === row.key)
    const existing = form.existingTasks?.find(task => task.key === row.key)
    return {
      key: row.key,
      decision: saved?.decision || 'pending',
      values: saved?.values ?? {},
      assignee: assigneeRef(saved?.assignee) ?? assigneeRef(existing?.assignee),
    }
  })
}

/** Splits the ≥4 same-severity rows into one readable line for the summary bar. */
function severityRollup(form: WorkflowForm) {
  const order = ['高', '中', '低', 'critical', 'high', 'medium', 'low']
  const buckets = new Map<string, number>()
  for (const finding of form.findings) {
    const severity = text(finding.severity).trim()
    if (severity) buckets.set(severity, (buckets.get(severity) ?? 0) + 1)
  }
  return [...buckets.entries()].sort((a, b) => {
    const left = order.findIndex(key => a[0].includes(key) || a[0].toLowerCase().includes(key))
    const right = order.findIndex(key => b[0].includes(key) || b[0].toLowerCase().includes(key))
    return (left < 0 ? order.length : left) - (right < 0 ? order.length : right)
  })
}

const severityChip = (severity: string) => {
  if (severity.includes('高') || /critical|high/i.test(severity)) return 'border-destructive/30 bg-destructive/10 text-destructive'
  if (severity.includes('中') || /medium/i.test(severity)) return 'border-warning/35 bg-warning/10 text-warning'
  if (severity.includes('低') || /low/i.test(severity)) return 'border-info/30 bg-info/10 text-info'
  return 'border-border bg-muted text-muted-foreground'
}

/** Reads a durable review; refreshing never invents a new form or task. */
export function RunForm({ tenantId, runId, runStatus, nodeError }: { tenantId: string; runId: string; runStatus: string; nodeError?: string }) {
  const query = useQuery({ queryKey: ['workflow-form', tenantId, runId], queryFn: () => getWorkflowForm(tenantId, runId), refetchInterval: 3000, retry: false })
  if (query.isPending) return <div className="flex min-h-48 items-center justify-center text-sm text-muted-foreground" role="status">正在读取确认表单…</div>
  if (!query.data) {
    // A form node that never produced one failed upstream, not in the reviewer's hands; say so
    // instead of asking them to refresh a page that cannot change.
    if (nodeError) return <section className="mx-auto flex w-full max-w-xl flex-col gap-3 rounded-xl border border-destructive/25 bg-destructive/5 p-5" aria-label="确认表单未生成">
      <p className="flex items-center gap-2 text-sm font-semibold text-destructive"><ShieldAlert className="size-4" aria-hidden />确认表单没有生成</p>
      <p className="text-sm text-muted-foreground">{nodeError}</p>
      <p className="text-xs text-muted-foreground">这条运行停在确认步骤之前：报告或问题列表没有产出，或表单配置的取值变量对不上。修正上游节点后重新运行即可。</p>
    </section>
    return <div className="flex min-h-48 flex-col items-center justify-center gap-3 text-sm text-muted-foreground" role="status"><CircleDashed className="size-6" />表单尚未生成，运行到确认步骤后会自动显示。<Button variant="outline" onClick={() => void query.refetch()}>刷新</Button></div>
  }
  return <FormEditor key={query.data.id} form={query.data} tenantId={tenantId} runStatus={runStatus} />
}

function FormEditor({ form, tenantId, runStatus }: { form: WorkflowForm; tenantId: string; runStatus: string }) {
  const roster = useGetApiV1TenantsTidMembers(tenantId, undefined, { query: { enabled: !!tenantId } })
  const squads = useQuery({ queryKey: ['squads', tenantId], queryFn: () => listSquads(tenantId) })
  const client = useQueryClient()
  const [version, setVersion] = useState(form.version)
  const [answers, setAnswers] = useState<FormAnswer[]>(() => readAnswers(form))
  const [dirty, setDirty] = useState(false)
  const [locallySubmitted, setLocallySubmitted] = useState(false)
  if (!dirty && form.version > version) {
    setVersion(form.version)
    setAnswers(readAnswers(form))
  }
  const [notice, setNotice] = useState('')
  const [undo, setUndo] = useState<FormAnswer[] | null>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [filter, setFilter] = useState<FindingFilter>('all')
  const [submitKey] = useState(() => crypto.randomUUID())

  const people = roster.data?.items ?? []
  const submitting = form.status !== 'open'
  const displayAnswers = submitting ? readAnswers(form) : answers
  const editable = form.status === 'open' && form.canEdit
  const existingFor = (key: string) => form.existingTasks?.find(task => task.key === key)
  const reviewer = people.find(member => member.userId === form.reviewerId)
  /** A row marked as a real problem needs an owner; default to the person judging it. */
  const defaultAssignee: TaskAssigneeRef | undefined = reviewer ? { type: 'human', id: reviewer.userId } : undefined

  const mutation = useMutation({
    mutationFn: (final: boolean) => final
      ? submitWorkflowForm(tenantId, form.runId, { version, answers: wireAnswers(answers) }, { headers: { 'Idempotency-Key': submitKey } })
      : saveWorkflowFormDraft(tenantId, form.runId, { version, answers: wireAnswers(answers) }),
    onError: error => {
      if (isAxiosError(error)) {
        const data = error.response?.data as { key?: string; field?: string } | undefined
        if (data?.key && data.field) setErrors({ [`${data.key}:${data.field}`]: '请检查这一项的输入。' })
      }
    },
    onSuccess: (saved, final) => {
      setDirty(false)
      setUndo(null)
      if (final) setLocallySubmitted(true)
      setVersion(saved.version)
      setNotice(final ? '确认已提交，正在创建或关联任务。' : '草稿已保存。')
      void client.invalidateQueries({ queryKey: ['workflow-form', tenantId, form.runId] })
      void client.invalidateQueries({ queryKey: ['workflow-runs', tenantId] })
    },
  })
  const retry = useMutation({
    mutationFn: () => retryWorkflowFormTasks(tenantId, form.runId),
    onSuccess: () => client.invalidateQueries({ queryKey: ['workflow-form', tenantId, form.runId] }),
  })

  const readOnly = !editable || mutation.isPending || locallySubmitted
  const remaining = displayAnswers.filter(answer => answer.decision === 'pending').length
  const missingOwner = answers.filter(answer => answer.decision === 'confirmed' && !answer.assignee && !existingFor(answer.key)).length
  const complete = remaining === 0 && answers.every(answer => answer.decision !== 'confirmed' || answer.assignee || existingFor(answer.key))
  const newTasks = answers.filter(answer => answer.decision === 'confirmed' && existingFor(answer.key) === undefined).length
  const reusedTasks = answers.filter(answer => answer.decision === 'confirmed' && existingFor(answer.key) !== undefined).length
  const fields = form.config.fields
  const counts = {
    confirmed: displayAnswers.filter(answer => answer.decision === 'confirmed').length,
    rejected: displayAnswers.filter(answer => answer.decision === 'rejected').length,
    pending: remaining,
  }
  const total = displayAnswers.length
  const visibleFindings = form.findings.filter(row => filter === 'all' || (displayAnswers.find(answer => answer.key === row.key)?.decision || 'pending') === filter)
  const title = text(form.config.title) || '问题确认'
  const submittedBy = people.find(member => member.userId === form.submittedBy)?.displayName ?? '确认人'
  const created = form.results.filter(result => result.action === 'created').length
  const reused = form.results.filter(result => result.action === 'reused').length
  const rollup = severityRollup(form)

  function patch(key: string, change: Partial<FormAnswer>) {
    setDirty(true)
    setUndo(null)
    setErrors({})
    setAnswers(current => current.map(answer => answer.key === key ? { ...answer, ...change } : answer))
    setNotice('')
  }

  function decide(key: string, decision: string) {
    const answer = answers.find(item => item.key === key)
    const owner = decision === 'confirmed' && !answer?.assignee && !existingFor(key) ? defaultAssignee : answer?.assignee
    patch(key, { decision, assignee: owner })
  }

  function bulk(decision: 'confirmed' | 'rejected') {
    setUndo(answers)
    setDirty(true)
    setErrors({})
    setAnswers(current => current.map(answer => ({
      ...answer,
      decision,
      assignee: answer.assignee ?? (decision === 'confirmed' && !existingFor(answer.key) ? defaultAssignee : undefined),
    })))
    setNotice(decision === 'confirmed' ? `已把 ${total} 项都标为问题，可直接调整负责人。` : `已把 ${total} 项都标为不是问题。`)
  }

  function revertBulk() {
    if (undo === null) return
    setAnswers(undo)
    setUndo(null)
    setDirty(true)
    setNotice('已撤销批量判定。')
  }

  function save(final: boolean) {
    const found: Record<string, string> = {}
    if (final) {
      for (const answer of answers) {
        if (answer.decision === 'pending') found[`${answer.key}:decision`] = '请先判定这一项。'
        if (answer.decision === 'confirmed') {
          if (!answer.assignee && !existingFor(answer.key)) found[`${answer.key}:assignee`] = '请选择负责人。'
          for (const field of fields) if (field.required === true && field.type !== 'readonly' && !answer.values[text(field.name)]?.trim()) found[`${answer.key}:${text(field.name)}`] = '请填写这一项。'
        }
      }
    }
    setErrors(found)
    if (Object.keys(found).length) return
    mutation.mutate(final)
  }

  const hasReport = form.report.trim().length > 0
  const conflict = isAxiosError(mutation.error) && mutation.error.response?.status === 409
  const saveError = conflict
    ? '这份表单已被更新，载入最新草稿后再提交。'
    : '保存失败，请检查必填项或网络后重试；你填的内容还在这页上。'

  const status = form.status
  const statusLine = status === 'cancelled'
    ? '运行已取消，这份表单不能再提交。'
    : status === 'submitted'
      ? runStatus === 'failed'
        ? `已由 ${submittedBy} 确认 · 任务没有建成，可在下方重试`
        : runStatus === 'succeeded'
          ? `已由 ${submittedBy} 确认 · ${created > 0 ? `新建 ${created} 个任务` : ''}${created > 0 && reused > 0 ? '、' : ''}${reused > 0 ? `关联 ${reused} 个` : ''}${created + reused === 0 ? '无需处理，运行已完成' : ''}`
          : `已由 ${submittedBy} 确认 · 正在生成任务`
      : total === 0
        ? '没有需要判定的问题。'
        : editable
          ? `${total} 项发现待你判定；确认后由后续节点创建或关联协作任务。`
          : `${reviewer?.displayName ?? '指定的确认人'} 确认后才能继续；你可以先看证据。`

  const statusBadge = status === 'submitted'
    ? <Badge variant="outline" className="gap-1 border-success/30 bg-success/10 text-success"><CheckCheck className="size-3" />已提交</Badge>
    : status === 'cancelled'
      ? <Badge variant="outline" className="gap-1 border-muted-foreground/30 bg-muted text-muted-foreground"><XCircle className="size-3" />已取消</Badge>
      : <Badge variant="outline" className="gap-1 border-warning/35 bg-warning/10 text-warning"><CircleDashed className="size-3" />等待确认</Badge>

  return <section className="flex w-full flex-col gap-4" aria-label="报告与问题确认">
    <header className="flex flex-wrap items-start justify-between gap-x-4 gap-y-2">
      <div className="min-w-0">
        <p className="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"><ClipboardCheck className="size-3.5" aria-hidden />确认步骤</p>
        <h2 className="mt-1 text-xl font-semibold tracking-tight">{title}</h2>
        <p className="mt-1 text-sm text-muted-foreground">{statusLine}</p>
      </div>
      <div className="flex shrink-0 items-center gap-2">
        {statusBadge}
        {total > 0 && <Badge variant="secondary" className="tabular-nums">{total - remaining}/{total} 已处理</Badge>}
      </div>
    </header>

    {total > 0 && <div className="flex flex-col gap-2 rounded-xl border bg-card px-3 py-2 sm:flex-row sm:items-center sm:justify-between">
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
        {rollup.map(([severity, count]) => <span key={severity} className={cn('inline-flex items-center gap-1 rounded-md border px-1.5 py-0.5 text-[11px] font-medium', severityChip(severity))}>
          <TriangleAlert className="size-3" aria-hidden />{severity}<span className="tabular-nums">{count}</span>
        </span>)}
        <span className="mx-0.5 hidden h-4 w-px bg-border sm:block" aria-hidden />
        <div role="group" aria-label="筛选问题" className="flex flex-wrap items-center gap-0.5">
          {([
            ['all', '全部', total],
            ['pending', '待确认', counts.pending],
            ['confirmed', '是问题', counts.confirmed],
            ['rejected', '不是问题', counts.rejected],
          ] as const).map(([value, label, count]) => <button
            key={value}
            type="button"
            aria-pressed={filter === value}
            onClick={() => setFilter(value)}
            className={cn('inline-flex items-center gap-1 rounded-md px-2 py-1 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring',
              filter === value ? 'bg-secondary text-secondary-foreground' : 'text-muted-foreground hover:bg-muted hover:text-foreground')}
          >
            {label}<span className="tabular-nums text-muted-foreground">{count}</span>
          </button>)}
        </div>
      </div>
      {editable && total > 1 && <div className="flex shrink-0 items-center gap-1.5">
        {undo !== null && <Button variant="ghost" size="xs" onClick={revertBulk}><RotateCcw />撤销</Button>}
        <Button variant="outline" size="xs" disabled={readOnly} onClick={() => bulk('confirmed')}><CheckCheck />全部标为问题</Button>
        <Button variant="outline" size="xs" disabled={readOnly} onClick={() => bulk('rejected')}><XCircle />全部排除</Button>
      </div>}
    </div>}

    <div className={cn('grid min-w-0 gap-4', total > 0 && hasReport && 'lg:grid-cols-[minmax(0,22rem)_minmax(0,1fr)] lg:items-start xl:gap-5')}>
      {hasReport && <div className="order-last min-w-0 lg:order-none"><RunReportPanel report={form.report} count={total} /></div>}

      <section className="min-w-0" aria-label="问题列表">
        {total === 0 && <div className="flex flex-col items-center gap-2 rounded-xl border border-dashed bg-muted/20 px-4 py-10 text-center">
          <CheckCheck className="size-6 text-success" aria-hidden />
          <p className="text-sm font-medium">本次未发现问题</p>
          <p className="text-sm text-muted-foreground">确认后这一步直接完成，不会创建任务。</p>
        </div>}
        {total > 0 && visibleFindings.length === 0 && <p className="rounded-xl border border-dashed bg-muted/20 px-4 py-8 text-center text-sm text-muted-foreground">这个筛选下没有问题。</p>}

        <div className="flex flex-col gap-2.5">
          {visibleFindings.map(row => {
            const key = text(row.key)
            const answer = displayAnswers.find(item => item.key === key)!
            const result = form.results.find(item => item.key === key)
            return <div key={key} className="flex flex-col gap-2">
              <RunFindingCard
                finding={row}
                answer={answer}
                fields={fields}
                existing={existingFor(key)}
                result={result}
                errors={errors}
                readOnly={readOnly}
                editable={form.status === 'open'}
                tenantId={tenantId}
                squads={squads.data?.items ?? []}
                roster={people.map(member => ({ userId: member.userId, displayName: member.displayName }))}
                onDecide={value => decide(key, value)}
                onPatch={change => patch(key, change)}
                receipt={result ? <DtsReceipt taskId={text(result.taskId)} title={text(row.title)} description={text(row.description)} severity={text(row.severity)} module={text(row.module)} runId={form.runId} /> : undefined}
              />
            </div>
          })}
        </div>

        {dirty && form.version > version && <div role="alert" className="mt-3 rounded-xl border border-warning/35 bg-warning-soft p-3 text-sm">
          <p className="font-medium">其他确认人已更新这份表单，保存时会以服务端版本为准。</p>
          <details className="mt-1.5"><summary className="cursor-pointer text-xs text-muted-foreground">查看未保存的本地修改</summary>{answers.map(answer => <p key={answer.key} className="mt-1 text-xs">{form.findings.find(finding => finding.key === answer.key)?.title}：{answer.decision === 'confirmed' ? '是问题' : answer.decision === 'rejected' ? '不是问题' : '待确认'} · {Object.values(answer.values).join('；')}</p>)}</details>
          {form.status === 'open' && <Button className="mt-2" variant="outline" size="sm" onClick={() => { setAnswers(readAnswers(form)); setVersion(form.version); setDirty(false); setNotice('已载入最新草稿。') }}>载入最新草稿</Button>}
        </div>}

        {mutation.isError && <p role="alert" className="mt-3 flex items-start gap-2 rounded-lg border border-destructive/25 bg-destructive/5 px-3 py-2 text-sm text-destructive"><TriangleAlert className="mt-0.5 size-3.5 shrink-0" aria-hidden />{saveError}</p>}

        {status === 'submitted' && runStatus === 'failed' && form.results.length === 0 && displayAnswers.some(answer => answer.decision === 'confirmed') && <div className="mt-3 flex flex-wrap items-center gap-3 rounded-xl border border-warning/35 bg-warning-soft px-3 py-2.5">
          <p className="min-w-0 flex-1 text-sm">确认已保存，但任务没有建成。重试只会补建任务，不需要重新判定。</p>
          <Button variant="outline" size="sm" disabled={retry.isPending} onClick={() => retry.mutate()}><RotateCcw />{retry.isPending ? '正在重试…' : '重试任务创建'}</Button>
        </div>}
        {retry.isError && <p role="alert" className="mt-2 text-sm text-destructive">任务当前不可重试，请查看运行状态。</p>}

        {form.status === 'open' && !form.canEdit && <p className="mt-3 flex items-center gap-1.5 rounded-lg border bg-muted/25 px-3 py-2 text-sm text-muted-foreground"><Info className="size-3.5" aria-hidden />只有指定确认人或工作区管理员可以提交；你仍然可以查看证据。</p>}
        {notice && form.status === 'open' && <p role="status" className="mt-3 text-sm text-muted-foreground">{notice}</p>}
      </section>
    </div>

    {form.status === 'open' && <footer className="sticky bottom-0 z-20 -mx-6 flex flex-col gap-2 border-t bg-background/95 px-6 py-3 backdrop-blur supports-[backdrop-filter]:bg-background/85 sm:flex-row sm:items-center sm:gap-3">
      <div className="min-w-0 flex-1">
        <p className="text-sm font-medium">{total === 0 ? '确认后继续' : remaining > 0 ? `还有 ${remaining} 项待确认` : missingOwner > 0 ? `还有 ${missingOwner} 项没指定负责人` : newTasks > 0 && reusedTasks > 0 ? `将新建 ${newTasks} 个任务、关联 ${reusedTasks} 个已有任务` : newTasks > 0 ? `将新建 ${newTasks} 个任务` : `将关联 ${reusedTasks} 个已有任务`}</p>
        {total > 0 && <p className="text-xs text-muted-foreground">保存草稿不会创建任务；提交后由后续节点统一处理。</p>}
      </div>
      <div className="flex flex-wrap items-center gap-2 sm:shrink-0 sm:justify-end">
        <Button variant="outline" disabled={readOnly} onClick={() => save(false)}><Save />保存草稿</Button>
        <Button disabled={readOnly || !complete} onClick={() => save(true)}>
          {mutation.isPending ? <><span className="size-3.5 animate-spin rounded-full border-2 border-current border-t-transparent" />正在保存…</> : <><Send />{total === 0 ? '确认无问题并继续' : text(form.config.submitLabel) || '确认并生成任务'}</>}
        </Button>
      </div>
    </footer>}
  </section>
}
