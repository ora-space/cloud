import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Workflow } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldGroup, FieldLabel } from '@/components/ui/field'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { PanelDialog } from '@/components/panel/panel-dialog'
import { DEMO_HANDLING_WORKFLOW, demoPromptFor, demoTaskRunsKey, startDemoTaskWorkflow } from '@/lib/demo/task-workflows'
import { listWorkflows } from '@/lib/workflows/api/generated'
import type { Task } from './tasks-api/generated'

/**
 * Pick a workflow, write the requirement it works from, and start it on the task. Only the demo offers
 * this (see `lib/demo/task-workflows.ts`); the assignee reviews the result and submits it by hand.
 */
export function TaskWorkflowDialog({ tenantId, task, onClose, onStarted }: {
  tenantId: string
  task: Task
  onClose: () => void
  onStarted: (workflowRunId: string) => void
}) {
  const client = useQueryClient()
  const workflows = useQuery({ queryKey: ['workflows', tenantId], queryFn: () => listWorkflows(tenantId), enabled: !!tenantId })
  const published = (workflows.data?.workflows ?? []).filter(w => w.publishedVersion)
  const [picked, setPicked] = useState('')
  const workflowId = picked || (published.find(w => w.name === DEMO_HANDLING_WORKFLOW) ?? published[0])?.id || ''
  const [prompt, setPrompt] = useState(() => demoPromptFor(task))
  const start = useMutation({
    mutationFn: () => startDemoTaskWorkflow(tenantId, task, workflowId, prompt.trim()),
    onSuccess: async runId => {
      await client.invalidateQueries({ queryKey: demoTaskRunsKey(tenantId, task.id) })
      onStarted(runId)
    },
  })
  const submit = (event: FormEvent<HTMLFormElement>) => { event.preventDefault(); start.mutate() }
  return <PanelDialog icon={<Workflow size={14} />} eyebrow="用工作流处理" title={`处理 ${task.title}`}
    subtitle="选一个团队工作流，写清楚需求。工作流按需求执行，你来判断结果。" onClose={onClose}>
    <form className="mc-form" onSubmit={submit} noValidate>
      <div className="mc-form-scroll"><FieldGroup>
        <Field>
          <FieldLabel htmlFor="mc-handle-workflow">工作流</FieldLabel>
          <NativeSelect id="mc-handle-workflow" value={workflowId} disabled={published.length === 0} onChange={e => setPicked(e.target.value)}>
            {published.length === 0 && <NativeSelectOption value="">{workflows.isLoading ? '正在加载…' : '没有可用的工作流'}</NativeSelectOption>}
            {published.map(w => <NativeSelectOption key={w.id} value={w.id}>{w.name}（{w.publishedVersion}）</NativeSelectOption>)}
          </NativeSelect>
        </Field>
        <Field>
          <FieldLabel htmlFor="mc-handle-prompt">需求</FieldLabel>
          <Textarea id="mc-handle-prompt" rows={10} value={prompt} onChange={e => setPrompt(e.target.value)} />
          <FieldDescription>工作流的每一步都按这份需求执行。</FieldDescription>
        </Field>
      </FieldGroup></div>
      {start.isError && <p role="alert" className="mc-form-error">工作流启动失败，请重试。</p>}
      <footer className="mc-form-actions">
        <Button type="button" variant="outline" onClick={onClose} disabled={start.isPending}>取消</Button>
        <Button type="submit" disabled={start.isPending || !workflowId || !prompt.trim()}>{start.isPending ? '启动中…' : '启动工作流'}</Button>
      </footer>
    </form>
  </PanelDialog>
}
