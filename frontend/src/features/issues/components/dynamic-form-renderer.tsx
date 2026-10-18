import { FormFieldRenderer } from '@/features/issues/components/form-field-renderer'
import type { FormDescriptor, FormFieldType, FormValues } from '@/features/issues/types'

/** The field types this renderer knows how to draw. Anything else fails closed. */
const SUPPORTED: ReadonlySet<string> = new Set<FormFieldType>([
  'text',
  'textarea',
  'number',
  'boolean',
  'select',
  'multi_select',
])

/**
 * Renders a Workflow form purely from its `FormDescriptor`. It never inspects the workflow's identity
 * and never hard-codes a field: the field set lives in the provider, not here (Issue collaboration D7). An unknown
 * field type is a descriptor bug, so it is surfaced as an explicit error state instead of guessed at.
 */
export function DynamicFormRenderer({
  descriptor,
  values,
  onChange,
}: {
  descriptor: FormDescriptor
  values: FormValues
  onChange: (key: string, next: unknown) => void
}) {
  const unsupported = descriptor.fields.filter((field) => !SUPPORTED.has(field.type))
  if (unsupported.length > 0) {
    return (
      <p className="rounded-md border border-destructive/40 bg-destructive/5 px-2 py-1.5 text-xs text-destructive">
        表单包含不支持的字段类型：
        {unsupported.map((field) => `${field.key}(${field.type})`).join('、')}
      </p>
    )
  }
  return (
    <div className="space-y-3">
      {descriptor.fields.map((field) => (
        <FormFieldRenderer
          key={field.key}
          field={field}
          value={values[field.key]}
          onChange={(next) => onChange(field.key, next)}
        />
      ))}
    </div>
  )
}
