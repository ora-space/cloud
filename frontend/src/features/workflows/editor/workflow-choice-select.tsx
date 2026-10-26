import { Select, SelectContent, SelectItem, SelectTrigger } from '@/components/ui/select'
import { workflowChoiceLabel, type WorkflowChoice } from '@/features/workflows/runtime/capabilities'
import { InspectorField } from '@/features/workflows/editor/workflow-inspector-fields'
import { LocalizedSelectValue } from '@/features/workflows/editor/workflow-variable-select'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'

/** A select whose menu text and closed-state label both come from one choice list. */
export function WorkflowChoiceSelect({
  id,
  value,
  options,
  disabled,
  ariaLabel,
  className,
  onChange,
}: {
  id: string
  value: string
  options: readonly WorkflowChoice[]
  disabled?: boolean
  ariaLabel?: string
  className?: string
  onChange: (value: string | null) => void
}) {
  const translate = useWorkflowTranslator()
  return (
    <Select value={value} disabled={disabled} onValueChange={onChange}>
      <SelectTrigger id={id} aria-label={ariaLabel} className={className}>
        <LocalizedSelectValue options={options} value={value} />
      </SelectTrigger>
      <SelectContent>
        {options.map((option) => (
          <SelectItem key={option.value} value={option.value}>
            {workflowChoiceLabel(option, translate)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

/** A {@link WorkflowChoiceSelect} wrapped in the inspector's label block. */
export function LocalizedChoiceSelect({
  id,
  label,
  value,
  options,
  disabled,
  onChange,
}: {
  id: string
  label: string
  value: string
  options: readonly WorkflowChoice[]
  disabled?: boolean
  onChange: (value: string | null) => void
}) {
  return (
    <InspectorField label={label} htmlFor={id}>
      <WorkflowChoiceSelect
        id={id}
        value={value}
        options={options}
        {...(disabled === undefined ? {} : { disabled })}
        className="w-full"
        onChange={onChange}
      />
    </InspectorField>
  )
}
