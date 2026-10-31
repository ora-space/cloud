import { useState, type FormEvent, type ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { MAX_TASK_DESCRIPTION, MAX_TASK_TITLE, validateTask } from './task-model'

/** The editable task text; assignment has its own write, so it is not part of the draft. */
export interface TaskDraft {
  title: string
  description: string
  projectId: string
}

/**
 * Create or edit a task's own fields. A task always belongs to one project of this workspace, so
 * the project is a choice from the loaded list rather than a free-text id.
 */
export function TaskForm({ initial, projects, projectsLoading, submitLabel, saving, error, onSave, onCancel }: {
  initial?: TaskDraft
  /** Projects the task may belong to, already scoped to this workspace by the server. */
  projects: { id: string; name: string }[]
  /** The project list is still loading; the form waits for a real choice. */
  projectsLoading: boolean
  submitLabel: string
  saving: boolean
  /** Error from the last save attempt, shown above the actions. */
  error: string
  onSave: (draft: TaskDraft) => void
  onCancel: () => void
}) {
  // The project list may arrive after the form opens, so the first project is only a fallback.
  const [draft, setDraft] = useState<TaskDraft>(initial ?? { title: '', description: '', projectId: '' })
  const projectId = draft.projectId || projects[0]?.id || ''
  const [errors, setErrors] = useState<Record<string, string>>({})
  const patch = (changes: Partial<TaskDraft>) => {
    setDraft(current => ({ ...current, ...changes }))
    setErrors({})
  }
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const found = validateTask(draft.title, draft.description)
    if (!projectId) found.project = '请选择任务所属项目。'
    setErrors(found)
    if (Object.keys(found).length) return
    onSave({ title: draft.title.trim(), description: draft.description, projectId })
  }
  const unavailable = projectsLoading || projects.length === 0
  return <form className="mc-form" onSubmit={submit} noValidate>
    <div className="mc-form-scroll"><FieldGroup>
      <TaskField name="title" label="任务标题" error={errors.title} hint={`一句话说明要做什么，最多 ${MAX_TASK_TITLE} 个字符。`}>
        <Input id="mc-task-title" placeholder="例如：整理演示数据" value={draft.title} onChange={e => patch({ title: e.target.value })} aria-invalid={!!errors.title} aria-describedby={errors.title ? 'mc-error-title' : undefined} />
      </TaskField>
      <TaskField name="project" label="所属项目" error={errors.project} hint={projectsLoading ? '正在加载项目…' : projects.length === 0 ? '工作区还没有项目，先在聊天页创建或选择项目。' : '任务始终属于一个项目；执行会在这个项目里进行。'}>
        <NativeSelect id="mc-task-project" value={projectId} disabled={projectsLoading || projects.length === 0} onChange={e => patch({ projectId: e.target.value })}>
          {projects.length === 0 && <NativeSelectOption value="">{projectsLoading ? '正在加载…' : '没有可用的项目'}</NativeSelectOption>}
          {projects.map(project => <NativeSelectOption key={project.id} value={project.id}>{project.name}</NativeSelectOption>)}
        </NativeSelect>
      </TaskField>
      <TaskField name="description" label="描述（可选）" error={errors.description} hint={`写给执行者看的背景与验收要点，最多 ${MAX_TASK_DESCRIPTION} 个字符。`}>
        <Textarea id="mc-task-description" rows={5} placeholder="例如：把三条示例任务写好，并确认状态能正常流转。" value={draft.description} onChange={e => patch({ description: e.target.value })} aria-invalid={!!errors.description} aria-describedby={errors.description ? 'mc-error-description' : undefined} />
      </TaskField>
    </FieldGroup></div>
    {error && <p role="alert" className="mc-form-error">{error}</p>}
    <footer className="mc-form-actions">
      <Button type="button" variant="outline" onClick={onCancel} disabled={saving}>取消</Button>
      <Button type="submit" disabled={saving || unavailable}>{saving ? '保存中…' : submitLabel}</Button>
    </footer>
  </form>
}

function TaskField({ name, label, error, hint, children }: { name: string; label: string; error?: string; hint: string; children: ReactNode }) {
  return <Field data-invalid={!!error}>
    <FieldLabel htmlFor={`mc-task-${name}`}>{label}</FieldLabel>
    {children}
    <FieldDescription>{hint}</FieldDescription>
    <FieldError id={`mc-error-${name}`}>{error}</FieldError>
  </Field>
}
