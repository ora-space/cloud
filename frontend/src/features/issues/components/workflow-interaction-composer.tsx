import { ChevronDown, ChevronUp, Sparkles, Workflow, X } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import {
  openWorkflowInteraction,
  useAssistWorkflow,
  useConfirmWorkflow,
  useFormDescriptor,
} from '@/features/issues/api'
import { AssistSuggestions } from '@/features/issues/components/assist-suggestions'
import { ConfirmReview } from '@/features/issues/components/confirm-review'
import { DynamicFormRenderer } from '@/features/issues/components/dynamic-form-renderer'
import type {
  AssistSuggestion,
  CollaborationTargetSummary,
  ContextRefRef,
  FormDescriptor,
  FormField,
  FormValues,
} from '@/features/issues/types'

/** Seeds the editable values from the descriptor's declared defaults. */
function seedValues(descriptor: FormDescriptor): FormValues {
  const values: FormValues = {}
  for (const field of descriptor.fields) {
    if (field.defaultValue !== undefined && field.defaultValue !== null) {
      values[field.key] = field.defaultValue
    }
  }
  return values
}

/** Required fields the user has not filled yet — the only client-side gate on Review (Issue collaboration D9). */
function missingRequired(descriptor: FormDescriptor, values: FormValues): FormField[] {
  return descriptor.fields.filter((field) => {
    if (!field.required) return false
    const value = values[field.key]
    if (value === undefined || value === null) return true
    if (typeof value === 'string') return value.trim() === ''
    if (Array.isArray(value)) return value.length === 0
    return false
  })
}

function sameRef(left: ContextRefRef, right: ContextRefRef): boolean {
  return left.refType === right.refType && left.refId === right.refId
}

/** The descriptor key whose value is read as context references rather than as workflow input (Issue collaboration D8). */
const CONTEXT_REFS_KEY = 'context_refs'

/**
 * The ref types that field can offer. A literal set rather than a cast, so the parsed value narrows to
 * a checked member of `ContextRef['refType']` instead of asserting its way in.
 */
const FIELD_REF_TYPES = ['project', 'parent_issue'] as const

function isFieldRefType(value: string): value is (typeof FIELD_REF_TYPES)[number] {
  return FIELD_REF_TYPES.some((candidate) => candidate === value)
}

/**
 * Reads the supplementary context-reference field: a list of `refType:refId` pairs, each of which the
 * run receives as a context reference. Anything else is dropped rather than forwarded — the server
 * validates the pairs again and would answer a malformed one with a 400, so a bad pair is better
 * dropped here than turned into a failed confirm.
 */
function contextRefsFromField(value: unknown): ContextRefRef[] {
  if (!Array.isArray(value)) return []
  const refs: ContextRefRef[] = []
  for (const item of value) {
    if (typeof item !== 'string') continue
    const separator = item.indexOf(':')
    if (separator <= 0) continue
    const refType = item.slice(0, separator)
    const refId = item.slice(separator + 1)
    if (refId === '' || !isFieldRefType(refType)) continue
    refs.push({ refType, refId })
  }
  return refs
}

/**
 * The references the run carries: the ones AI Assist applied and the ones the user picked, each once.
 * Deduplicated because the run appends what it is given without checking, so a suggestion the user
 * also ticked would otherwise travel twice.
 */
function mergeContextRefs(applied: ContextRefRef[], picked: ContextRefRef[]): ContextRefRef[] {
  const merged = [...applied]
  for (const ref of picked) {
    if (!merged.some((item) => sameRef(item, ref))) merged.push(ref)
  }
  return merged
}

/** Editable form state for one Workflow interaction. Pure UI state — nothing here is persisted. */
function useWorkflowForm(descriptor: FormDescriptor | undefined) {
  const [values, setValues] = useState<FormValues>({})
  const [contextRefs, setContextRefs] = useState<ContextRefRef[]>([])
  const [suggestion, setSuggestion] = useState<AssistSuggestion | null>(null)
  const [appliedKeys, setAppliedKeys] = useState<string[]>([])
  const seeded = useRef<string | null>(null)

  useEffect(() => {
    if (!descriptor || seeded.current === descriptor.formRef) return
    seeded.current = descriptor.formRef
    setValues(seedValues(descriptor))
  }, [descriptor])

  function setField(key: string, next: unknown) {
    setValues((current) => ({ ...current, [key]: next }))
  }

  function applySuggestion(key: string, next: unknown) {
    setField(key, next)
    setAppliedKeys((current) => (current.includes(key) ? current : [...current, key]))
  }

  function applyAll(patch: FormValues) {
    setValues((current) => ({ ...current, ...patch }))
    setAppliedKeys((current) => Array.from(new Set([...current, ...Object.keys(patch)])))
  }

  function ignoreSuggestion(key: string) {
    setSuggestion((current) => {
      if (!current) return current
      const remaining = { ...current.suggestedValues }
      delete remaining[key]
      return { ...current, suggestedValues: remaining }
    })
  }

  function applyRef(ref: ContextRefRef) {
    setContextRefs((current) =>
      current.some((item) => sameRef(item, ref)) ? current : [...current, ref],
    )
  }

  function ignoreRef(ref: ContextRefRef) {
    setSuggestion((current) =>
      current
        ? {
            ...current,
            suggestedContextRefs: current.suggestedContextRefs.filter(
              (item) => !sameRef(item, ref),
            ),
          }
        : current,
    )
  }

  return {
    values,
    contextRefs,
    suggestion,
    appliedKeys,
    setSuggestion,
    setField,
    applySuggestion,
    applyAll,
    ignoreSuggestion,
    applyRef,
    ignoreRef,
  }
}

/** The edit phase: the dynamic form, the optional assist panel, and the way into Review. */
function WorkflowFormBody(props: {
  descriptor: FormDescriptor
  form: ReturnType<typeof useWorkflowForm>
  assistPending: boolean
  missing: FormField[]
  onAssist: () => void
  onReview: () => void
}) {
  const { descriptor, form } = props
  return (
    <div className="space-y-2">
      <DynamicFormRenderer descriptor={descriptor} values={form.values} onChange={form.setField} />
      {form.suggestion ? (
        <AssistSuggestions
          descriptor={descriptor}
          values={form.values}
          suggestion={form.suggestion}
          appliedRefs={form.contextRefs}
          onApply={form.applySuggestion}
          onApplyAll={form.applyAll}
          onIgnore={form.ignoreSuggestion}
          onApplyRef={form.applyRef}
          onIgnoreRef={form.ignoreRef}
          onDismiss={() => form.setSuggestion(null)}
        />
      ) : null}
      <div className="flex items-center justify-end gap-1.5">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={props.onAssist}
          disabled={props.assistPending}
        >
          <Sparkles className="size-3.5" />
          {props.assistPending ? '生成建议…' : 'AI Assist'}
        </Button>
        <Button
          type="button"
          size="sm"
          onClick={props.onReview}
          disabled={props.missing.length > 0}
        >
          审阅
        </Button>
      </div>
      {props.missing.length > 0 ? (
        <p className="text-xs text-muted-foreground">
          请填写必填项：{props.missing.map((field) => field.label).join('、')}
        </p>
      ) : null}
    </div>
  )
}

/** The header bar: collapse on the left, workflow name, remove on the right. */
function ComposerHeader({
  title,
  collapsed,
  onToggle,
  onRemove,
}: {
  title: string
  collapsed: boolean
  onToggle: () => void
  onRemove: () => void
}) {
  return (
    <div className="flex items-center gap-2">
      <button
        type="button"
        className="rounded p-0.5 hover:bg-muted"
        onClick={onToggle}
        aria-label={collapsed ? '展开表单' : '折叠表单'}
      >
        {collapsed ? (
          <ChevronDown className="size-4 text-muted-foreground" />
        ) : (
          <ChevronUp className="size-4 text-muted-foreground" />
        )}
      </button>
      <Workflow className="size-4 text-muted-foreground" />
      <span className="text-sm font-medium">{title}</span>
      <Badge variant="secondary">Form Mode</Badge>
      <button
        type="button"
        className="ml-auto rounded p-0.5 hover:bg-muted"
        onClick={onRemove}
        aria-label="取消"
      >
        <X className="size-4 text-muted-foreground" />
      </button>
    </div>
  )
}

/** The expanded body: either the edit form or the Review step, plus the mutation error hints. */
function ComposerBody(props: {
  descriptor: FormDescriptor
  form: ReturnType<typeof useWorkflowForm>
  targetName: string
  reviewing: boolean
  missing: FormField[]
  assistPending: boolean
  confirmPending: boolean
  assistFailed: boolean
  confirmFailed: boolean
  onAssist: () => void
  onReview: () => void
  onBack: () => void
  onConfirm: () => void
}) {
  if (props.reviewing) {
    return (
      <>
        <ConfirmReview
          targetName={props.targetName}
          descriptor={props.descriptor}
          values={props.form.values}
          contextRefs={props.form.contextRefs}
          appliedKeys={props.form.appliedKeys}
          pending={props.confirmPending}
          onBack={props.onBack}
          onConfirm={props.onConfirm}
        />
        {props.confirmFailed ? (
          <p className="text-xs text-destructive">确认失败，请重试。</p>
        ) : null}
      </>
    )
  }
  return (
    <>
      <WorkflowFormBody
        descriptor={props.descriptor}
        form={props.form}
        assistPending={props.assistPending}
        missing={props.missing}
        onAssist={props.onAssist}
        onReview={props.onReview}
      />
      {props.assistFailed ? (
        <p className="text-xs text-muted-foreground">AI 建议当前不可用。</p>
      ) : null}
    </>
  )
}

/**
 * The Form Mode configuration panel for a workflow the user has selected but not yet submitted. It is
 * a **draft**: the comment, the interaction and the run are all created only when 确认执行 is pressed,
 * so an abandoned form leaves no trace in the Timeline. Selecting a workflow never executes it (Issue collaboration D9).
 */
export function WorkflowInteractionComposer({
  slug,
  issueId,
  target,
  onConfirmed,
  onRemove,
}: {
  slug: string
  issueId: string
  target: CollaborationTargetSummary
  onConfirmed: () => void
  onRemove: () => void
}) {
  const formRef = target.interactionDescriptor.formRef ?? undefined
  const descriptorQuery = useFormDescriptor(slug, formRef, issueId)
  const assist = useAssistWorkflow(slug, issueId)
  const confirm = useConfirmWorkflow(slug, issueId)
  const [reviewing, setReviewing] = useState(false)
  const [collapsed, setCollapsed] = useState(false)
  const descriptor = descriptorQuery.data
  const form = useWorkflowForm(descriptor)

  if (descriptorQuery.isPending) return <Skeleton className="h-24 w-full" />
  if (descriptorQuery.isError || !descriptor) {
    return (
      <p className="rounded-md border px-2 py-1.5 text-sm text-muted-foreground">
        当前部署无法加载该 Workflow 的表单（form descriptor 不可用）。
      </p>
    )
  }

  const contextRefs = mergeContextRefs(
    form.contextRefs,
    contextRefsFromField(form.values[CONTEXT_REFS_KEY]),
  )
  const confirmInput =
    contextRefs.length > 0 ? { values: form.values, contextRefs } : { values: form.values }

  /** Confirm is the first moment anything reaches the server: comment -> interaction -> run. */
  async function submit() {
    const interaction = await openWorkflowInteraction(
      slug,
      issueId,
      target.id,
      `@${target.displayName}`,
    )
    confirm.mutate({ interactionId: interaction.id, ...confirmInput }, { onSuccess: onConfirmed })
  }

  return (
    <section className="space-y-2 rounded-md border bg-muted/30 p-2">
      <ComposerHeader
        title={descriptor.title ?? target.displayName}
        collapsed={collapsed}
        onToggle={() => setCollapsed((current) => !current)}
        onRemove={onRemove}
      />
      {collapsed && <p className="text-xs text-muted-foreground">已折叠，点击左侧箭头展开配置。</p>}
      {!collapsed && descriptor.description ? (
        <p className="text-xs text-muted-foreground">{descriptor.description}</p>
      ) : null}
      {!collapsed && (
        <ComposerBody
          descriptor={descriptor}
          form={form}
          targetName={target.displayName}
          reviewing={reviewing}
          missing={missingRequired(descriptor, form.values)}
          assistPending={assist.isPending}
          confirmPending={confirm.isPending}
          assistFailed={assist.isError}
          confirmFailed={confirm.isError}
          onAssist={() =>
            assist.mutate(
              { targetId: target.id, values: form.values },
              { onSuccess: (data) => form.setSuggestion(data) },
            )
          }
          onReview={() => setReviewing(true)}
          onBack={() => setReviewing(false)}
          onConfirm={() => {
            void submit()
          }}
        />
      )}
    </section>
  )
}
