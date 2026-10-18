import { Button } from '@/components/ui/button'
import type { ContextRefRef, FormDescriptor, FormValues } from '@/features/issues/types'

function display(value: unknown): string {
  if (typeof value === 'string') return value === '' ? '—' : value
  if (typeof value === 'number') return String(value)
  if (typeof value === 'boolean') return value ? '是' : '否'
  if (Array.isArray(value)) return value.length > 0 ? value.map(display).join('、') : '—'
  return '—'
}

function optionLabel(descriptor: FormDescriptor, key: string, value: unknown): string {
  const field = descriptor.fields.find((candidate) => candidate.key === key)
  const match = field?.options?.find((option) => option.value === value)
  return match?.label ?? display(value)
}

/**
 * The Review step that must precede Confirm (Issue collaboration D9). It shows the target, the effective values and
 * the context that will travel with the run, and offers exactly two actions: go back and edit, or
 * confirm. Nothing here executes by itself.
 */
export function ConfirmReview({
  targetName,
  descriptor,
  values,
  contextRefs,
  appliedKeys,
  onBack,
  onConfirm,
  pending,
}: {
  targetName: string
  descriptor: FormDescriptor
  values: FormValues
  contextRefs: ContextRefRef[]
  appliedKeys: string[]
  onBack: () => void
  onConfirm: () => void
  pending: boolean
}) {
  return (
    <div className="space-y-2 rounded-md border p-2">
      <p className="text-xs font-medium text-muted-foreground">确认后才会创建执行（IssueRun）</p>
      <dl className="space-y-1 text-sm">
        <div className="flex gap-2">
          <dt className="w-24 shrink-0 text-muted-foreground">Workflow</dt>
          <dd className="font-medium">{targetName}</dd>
        </div>
        {descriptor.fields.map((field) => (
          <div key={field.key} className="flex gap-2">
            <dt className="w-24 shrink-0 text-muted-foreground">{field.label}</dt>
            <dd className="min-w-0 flex-1 break-words">
              {optionLabel(descriptor, field.key, values[field.key])}
              {appliedKeys.includes(field.key) ? (
                <span className="ml-1.5 text-xs text-muted-foreground">（来自 AI 建议）</span>
              ) : null}
            </dd>
          </div>
        ))}
        <div className="flex gap-2">
          <dt className="w-24 shrink-0 text-muted-foreground">上下文引用</dt>
          <dd className="min-w-0 flex-1">
            {contextRefs.length === 0
              ? '—'
              : contextRefs.map((ref) => ref.refId.slice(0, 8)).join('、')}
          </dd>
        </div>
      </dl>
      <div className="flex justify-end gap-1.5">
        <Button type="button" variant="outline" size="sm" onClick={onBack} disabled={pending}>
          返回修改
        </Button>
        <Button type="button" size="sm" onClick={onConfirm} disabled={pending}>
          {pending ? '确认中…' : '确认执行'}
        </Button>
      </div>
    </div>
  )
}
