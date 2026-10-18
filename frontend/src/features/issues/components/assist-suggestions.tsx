import { Sparkles } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import type {
  AssistSuggestion,
  ContextRefRef,
  FormDescriptor,
  FormField,
  FormValues,
} from '@/features/issues/types'

/** Renders a raw form value for side-by-side comparison. */
function display(value: unknown): string {
  if (typeof value === 'string') return value === '' ? '（空）' : value
  if (typeof value === 'number') return String(value)
  if (typeof value === 'boolean') return value ? '是' : '否'
  if (Array.isArray(value)) return value.length > 0 ? value.map(display).join('、') : '（空）'
  return '（空）'
}

/** A suggestion the provider proposed for a key the descriptor actually declares. */
interface PendingSuggestion {
  field: FormField
  current: unknown
  suggested: unknown
  reason: string | null
}

function pending(
  descriptor: FormDescriptor,
  values: FormValues,
  suggestion: AssistSuggestion,
): PendingSuggestion[] {
  const out: PendingSuggestion[] = []
  for (const field of descriptor.fields) {
    if (!Object.hasOwn(suggestion.suggestedValues, field.key)) continue
    out.push({
      field,
      current: values[field.key],
      suggested: suggestion.suggestedValues[field.key],
      reason: suggestion.explanations?.[field.key] ?? null,
    })
  }
  return out
}

function SuggestionRow({
  item,
  onApply,
  onIgnore,
}: {
  item: PendingSuggestion
  onApply: () => void
  onIgnore: () => void
}) {
  return (
    <div className="space-y-1 rounded-md border px-2 py-1.5 text-sm">
      <div className="flex items-center gap-2">
        <span className="font-medium">{item.field.label}</span>
        <span className="ml-auto flex items-center gap-1.5">
          <Button type="button" variant="outline" size="sm" onClick={onApply}>
            应用
          </Button>
          <Button type="button" variant="ghost" size="sm" onClick={onIgnore}>
            忽略
          </Button>
        </span>
      </div>
      <div className="flex flex-wrap items-center gap-2 text-xs">
        <span className="text-muted-foreground">当前：{display(item.current)}</span>
        <span className="text-muted-foreground">→</span>
        <Badge variant="secondary">建议：{display(item.suggested)}</Badge>
      </div>
      {item.reason ? <p className="text-xs text-muted-foreground">{item.reason}</p> : null}
    </div>
  )
}

function refKey(ref: ContextRefRef): string {
  return `${ref.refType}:${ref.refId}`
}

/** Suggested context refs: applying one only adds it to this run's confirm payload (Issue collaboration D5). */
function SuggestedRefs({
  refs,
  appliedRefs,
  onApplyRef,
  onIgnoreRef,
}: {
  refs: ContextRefRef[]
  appliedRefs: ContextRefRef[]
  onApplyRef: (ref: ContextRefRef) => void
  onIgnoreRef: (ref: ContextRefRef) => void
}) {
  if (refs.length === 0) return null
  const applied = new Set(appliedRefs.map(refKey))
  return (
    <div className="space-y-1 border-t pt-1.5">
      <p className="text-xs font-medium text-muted-foreground">
        建议的上下文引用（应用后仅用于本次执行，不会永久保存）
      </p>
      {refs.map((ref) => (
        <div key={refKey(ref)} className="flex items-center gap-2 text-xs">
          <Badge variant="secondary">{ref.refType}</Badge>
          <span className="min-w-0 flex-1 truncate">{ref.refId}</span>
          {applied.has(refKey(ref)) ? (
            <span className="text-muted-foreground">已应用</span>
          ) : (
            <span className="flex items-center gap-1">
              <Button type="button" variant="outline" size="sm" onClick={() => onApplyRef(ref)}>
                应用
              </Button>
              <Button type="button" variant="ghost" size="sm" onClick={() => onIgnoreRef(ref)}>
                忽略
              </Button>
            </span>
          )}
        </div>
      ))}
    </div>
  )
}

/**
 * AI Assist review panel: shows each suggestion next to the user's current value and lets the user
 * Apply or Ignore it. Assist never executes anything — applying only edits local form state, and
 * confirming stays a separate explicit action (Issue collaboration D9).
 */
export function AssistSuggestions({
  descriptor,
  values,
  suggestion,
  appliedRefs,
  onApply,
  onApplyAll,
  onIgnore,
  onApplyRef,
  onIgnoreRef,
  onDismiss,
}: {
  descriptor: FormDescriptor
  values: FormValues
  suggestion: AssistSuggestion
  appliedRefs: ContextRefRef[]
  onApply: (key: string, value: unknown) => void
  onApplyAll: (patch: FormValues) => void
  onIgnore: (key: string) => void
  onApplyRef: (ref: ContextRefRef) => void
  onIgnoreRef: (ref: ContextRefRef) => void
  onDismiss: () => void
}) {
  const items = pending(descriptor, values, suggestion)
  const patch: FormValues = {}
  for (const item of items) patch[item.field.key] = item.suggested
  return (
    <div className="space-y-1.5 rounded-md border border-dashed p-2">
      <div className="flex items-center gap-2">
        <Sparkles className="size-3.5 text-muted-foreground" />
        <span className="text-xs font-medium text-muted-foreground">
          AI 建议（仅建议，需你确认后才会执行）
        </span>
        <span className="ml-auto flex items-center gap-1.5">
          {items.length > 0 ? (
            <Button type="button" variant="outline" size="sm" onClick={() => onApplyAll(patch)}>
              全部应用
            </Button>
          ) : null}
          <Button type="button" variant="ghost" size="sm" onClick={onDismiss}>
            收起
          </Button>
        </span>
      </div>
      {items.length === 0 ? (
        <p className="text-xs text-muted-foreground">没有新的字段建议。</p>
      ) : null}
      {items.map((item) => (
        <SuggestionRow
          key={item.field.key}
          item={item}
          onApply={() => onApply(item.field.key, item.suggested)}
          onIgnore={() => onIgnore(item.field.key)}
        />
      ))}
      <SuggestedRefs
        refs={suggestion.suggestedContextRefs}
        appliedRefs={appliedRefs}
        onApplyRef={onApplyRef}
        onIgnoreRef={onIgnoreRef}
      />
    </div>
  )
}
