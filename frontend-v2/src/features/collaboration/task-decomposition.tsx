import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router'
import { ChevronDown, ChevronRight, CircleAlert, CornerDownRight, ListTree, LoaderCircle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { NativeSelect, NativeSelectOptGroup, NativeSelectOption } from '@/components/ui/native-select'
import { listDecompositionWorkflows } from './decomposition-api/generated'
import { PanelDialog } from '@/components/panel/panel-dialog'
import { decompositionMessageFor, DEMO_DECOMPOSITION_WORKFLOW, taskStatusLabel } from './task-model'
import { startTaskDecomposition, type Task, type TaskDecomposition } from './tasks-api/generated'
import { useTaskList } from './workspace'

/** The select's group label for published workflows the server says cannot split a task. */
const BLOCKED_GROUP_LABEL = '不可拆分（缺少 tasks 输出）'

/**
 * Pick the workflow that splits the task and start it. Published workflows carry the server's
 * compatibility verdict: eligible ones come first, incompatible ones sit disabled in one group at
 * the end, and the details list states each server reason once followed by the names it blocks.
 * The demo's 「需求拆分」 is chosen when the workspace has a compatible one.
 */
export function DecomposeDialog({ tenantId, task, onClose, onStarted }: {
  tenantId: string
  task: Task
  onClose: () => void
  /** The split started; `created` says which run is doing it. */
  onStarted: (created: TaskDecomposition) => void
}) {
  const client = useQueryClient()
  const workflows = useQuery({ queryKey: ['decomposition-workflows', tenantId], queryFn: () => listDecompositionWorkflows(tenantId), enabled: !!tenantId })
  const published = (workflows.data?.items ?? []).filter(w => w.publishedVersion)
  const eligible = published.filter(w => w.canDecompose)
  const blocked = published.filter(w => !w.canDecompose)
  // Every published blocked workflow carries the same server requirement; group names by reason so
  // the text appears once however many workflows it blocks.
  const blockedByReason = new Map<string, string[]>()
  for (const w of blocked) blockedByReason.set(w.decompositionBlockedReason, [...(blockedByReason.get(w.decompositionBlockedReason) ?? []), w.name])
  const [picked, setPicked] = useState('')
  const workflowId = picked || (eligible.find(w => w.name === DEMO_DECOMPOSITION_WORKFLOW) ?? eligible[0])?.id || ''
  const [key] = useState(() => crypto.randomUUID())
  const [error, setError] = useState('')
  const start = useMutation({
    mutationFn: () => startTaskDecomposition(tenantId, task.id, { workflowId }, { headers: { 'Idempotency-Key': key } }),
    onSuccess: created => {
      client.setQueryData(['task-decompositions', tenantId, task.id], (before?: { items: TaskDecomposition[] }) => ({ items: [created, ...(before?.items ?? []).filter(d => d.id !== created.id)] }))
      void client.invalidateQueries({ queryKey: ['task-decompositions', tenantId, task.id] })
      onStarted(created)
    },
    onError: e => setError(decompositionMessageFor(e)),
  })
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); setError(''); start.mutate() }
  const hint = workflows.isLoading
    ? '正在加载工作流…'
    : workflows.isError
      ? '工作流加载失败，请关闭后重试。'
      : published.length === 0
        ? '工作区还没有已发布的工作流，先在工作流页发布一个。'
        : eligible.length === 0
          ? '已发布的工作流都不满足拆分输出要求，请先修改并重新发布。'
          : '工作流的输出 tasks 会变成这个任务的子任务。'
  return <PanelDialog icon={<ListTree size={14} />} eyebrow="拆分" title={`拆分 ${task.title}`}
    subtitle="用工作流把大需求拆成子任务：任务的标题和描述是输入，跑完后子任务出现在看板上，再分给人和 Agent。" onClose={onClose}>
    <form className="mc-form" onSubmit={submit} noValidate>
      <div className="mc-form-scroll"><FieldGroup>
        <Field>
          <FieldLabel htmlFor="mc-decompose-workflow">工作流</FieldLabel>
          <NativeSelect id="mc-decompose-workflow" value={workflowId} disabled={eligible.length === 0} onChange={e => setPicked(e.target.value)}>
            {eligible.length === 0 && <NativeSelectOption value="">{workflows.isLoading ? '正在加载…' : '没有可拆分的工作流'}</NativeSelectOption>}
            {eligible.map(w => <NativeSelectOption key={w.id} value={w.id}>{w.name}（{w.publishedVersion}）</NativeSelectOption>)}
            {blocked.length > 0 && <NativeSelectOptGroup label={BLOCKED_GROUP_LABEL}>
              {blocked.map(w => <NativeSelectOption key={w.id} value={w.id} disabled>{w.name}（{w.publishedVersion}）</NativeSelectOption>)}
            </NativeSelectOptGroup>}
          </NativeSelect>
          <FieldDescription>{hint}</FieldDescription>
          {blocked.length > 0 && <details className="text-sm">
            <summary className="cursor-pointer text-muted-foreground">有 {blocked.length} 个已发布工作流不能用于拆分</summary>
            <div className="mt-1 max-h-32 space-y-1 overflow-y-auto">
              {[...blockedByReason].map(([reason, names]) => <div key={reason}>
                <FieldDescription className="text-destructive">{reason}</FieldDescription>
                <FieldDescription>{names.join('、')}</FieldDescription>
              </div>)}
            </div>
          </details>}
        </Field>
      </FieldGroup></div>
      {error && <p role="alert" className="mc-form-error">{error}</p>}
      <footer className="mc-form-actions">
        <Button type="button" variant="outline" onClick={onClose} disabled={start.isPending}>取消</Button>
        <Button type="submit" disabled={start.isPending || !workflowId}>{start.isPending ? '启动中…' : '开始拆分'}</Button>
      </footer>
    </form>
  </PanelDialog>
}

/**
 * The task's family on its page: for a big requirement, its latest decomposition and the subtasks
 * it wrote; for a subtask, the requirement it came from. The subtasks fold away, and start folded
 * when `folded` says the page needs the room; a person's own choice then holds.
 */
export function TaskFamily({ tenantId, task, latest, folded = false, onRetry }: { tenantId: string; task: Task; latest?: TaskDecomposition; folded?: boolean; onRetry: () => void }) {
  const list = useTaskList(tenantId)
  const [picked, setPicked] = useState<boolean>()
  const open = picked ?? !folded
  const all = list.data?.items ?? []
  if (task.parentId) {
    const parent = all.find(t => t.id === task.parentId)
    return <p className="mc-family-line"><CornerDownRight size={13} aria-hidden="true" />拆分自
      <Link className="mc-link" to={`/collaboration/tasks/${task.parentId}`}>{parent?.title ?? '大需求'}</Link></p>
  }
  // Subtasks are stamped newest-first in output order, so creation time keeps that order after moves.
  const subtasks = all.filter(t => t.parentId === task.id).sort((a, b) => b.createdAt.localeCompare(a.createdAt))
  if (!latest && subtasks.length === 0) return null
  return <section className="mc-family" aria-label="拆分">
    <div className="mc-family-head">
      {latest && <DecompositionStatus decomposition={latest} onRetry={onRetry} />}
      {subtasks.length > 0 && <button type="button" className="mc-family-toggle" aria-expanded={open} onClick={() => setPicked(!open)}>
        {open ? <ChevronDown size={14} aria-hidden="true" /> : <ChevronRight size={14} aria-hidden="true" />}{open ? '收起子任务' : '展开子任务'}
      </button>}
    </div>
    {open && subtasks.length > 0 && <ul className="mc-subtasks" aria-label="子任务">
      {subtasks.map(sub => <li key={sub.id}>
        <span className={`mc-status ${sub.status}`}>{taskStatusLabel(sub.status)}</span>
        <Link to={`/collaboration/tasks/${sub.id}`}>{sub.title}</Link>
        <small>{sub.assignee?.displayName ?? '未指派'}</small>
      </li>)}
    </ul>}
  </section>
}

function DecompositionStatus({ decomposition: d, onRetry }: { decomposition: TaskDecomposition; onRetry: () => void }) {
  switch (d.status) {
    case 'running':
      return <p className="mc-family-status" role="status"><LoaderCircle className="mc-spin" size={14} aria-hidden="true" />
        正在用「{d.workflowName}」拆分（{d.startedByName} 发起），跑完后子任务会出现在这里和看板上。</p>
    case 'succeeded':
      return <p className="mc-family-status"><ListTree size={14} aria-hidden="true" />「{d.workflowName}」拆出了 {d.createdCount} 个子任务，在看板上把它们分给人和 Agent。</p>
    default:
      return <div className="mc-family-status failed" role="alert"><CircleAlert size={14} aria-hidden="true" />
        <span>拆分失败：{d.failure || '工作流没有给出子任务。'}</span>
        <Button type="button" variant="outline" size="sm" onClick={onRetry}>重新拆分</Button>
      </div>
  }
}
