import { Variable as VariableIcon } from 'lucide-react'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectLabel,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { workflowChoiceLabel, type WorkflowChoice } from '@/features/workflows/runtime/capabilities'
import { WORKFLOW_VARIABLE_VALUE_TYPES } from '@/features/workflows/runtime/types'
import type {
  WorkflowDecoratedVariable,
  WorkflowVariableCatalogEntry,
} from '@/features/workflows/runtime/variable-catalog'
import { groupWorkflowVariables } from '@/features/workflows/runtime/variable-groups'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { useTranslation } from 'react-i18next'

/**
 * One variable row inside a select popover: `{x}` mark, name, and type.
 *
 * Exported so the prompt editor's variable-insert menu renders the same rows
 * as every other selector.
 */
export function WorkflowVariableRowContent({
  variable,
}: {
  variable: WorkflowVariableCatalogEntry
}) {
  return (
    <span className="flex w-full min-w-0 items-center justify-between gap-3">
      <span className="flex min-w-0 items-center gap-1.5">
        <span
          aria-hidden="true"
          data-workflow-variable-part="variable-mark"
          className="shrink-0 font-medium text-blue-600 dark:text-blue-400"
        >
          {'{x}'}
        </span>
        <span
          data-workflow-variable-part="variable-name"
          className="truncate font-medium text-foreground"
        >
          {variable.variableName}
        </span>
      </span>
      <span
        data-workflow-variable-part="variable-type"
        className="shrink-0 text-[11px] capitalize text-muted-foreground"
      >
        {variable.valueType}
      </span>
    </span>
  )
}

/**
 * Renders the grouped variable menu a selector triggers open.
 *
 * Grouping is what keeps a flat catalog readable: each upstream node is its own
 * heading, so the author can tell which `output` belongs to which step.
 *
 * @param variables - Catalog entries to offer, in display order.
 * @param globalVariablesLabel - Heading for the workflow-wide declarations.
 * @returns Grouped `SelectItem` rows ready for a `SelectContent`.
 */
export function WorkflowVariableSelectContent({
  variables,
  globalVariablesLabel,
}: {
  variables: readonly WorkflowVariableCatalogEntry[]
  globalVariablesLabel: string
}) {
  return (
    <>
      {groupWorkflowVariables(variables, globalVariablesLabel).map((group) => (
        <SelectGroup key={group.label}>
          <SelectLabel className="px-2 pt-1.5 pb-0.5 text-[11px] font-medium text-muted-foreground">
            {group.label}
          </SelectLabel>
          {group.variables.map((variable) => {
            const selector = variable.selector.join('.')
            return (
              <SelectItem
                key={selector}
                value={selector}
                aria-label={selector}
                className="[&_[data-workflow-variable-part=variable-mark]]:text-blue-600! [&_[data-workflow-variable-part=variable-name]]:text-foreground! [&_[data-workflow-variable-part=variable-type]]:text-muted-foreground! dark:[&_[data-workflow-variable-part=variable-mark]]:text-blue-400!"
              >
                <WorkflowVariableRowContent variable={variable} />
              </SelectItem>
            )
          })}
        </SelectGroup>
      ))}
    </>
  )
}

/**
 * The catalog fields the display actually reads, so a token NodeView can feed a
 * partial decoration built from stored token meta instead of a full entry.
 */
export type WorkflowDisplayVariable = Pick<
  WorkflowDecoratedVariable,
  'variableName' | 'sourceNodeKind'
>

/** Renders a catalog entry the way Dify labels it: node identity then the `{x}` name. */
export function WorkflowVariableDisplay({
  variable,
  nodeName,
}: {
  variable: WorkflowDisplayVariable
  nodeName: string
}) {
  const kind = variable.sourceNodeKind
  const Icon = kind === undefined ? VariableIcon : workflowNodeMetadata(kind).icon
  return (
    <span className="inline-flex max-w-full min-w-0 items-center gap-1 align-middle text-xs">
      <Icon
        aria-hidden="true"
        data-workflow-variable-part="node-icon"
        className="size-3.5 shrink-0 text-foreground"
      />
      <span
        data-workflow-variable-part="node-name"
        className="truncate font-medium text-foreground"
      >
        {nodeName}
      </span>
      <span aria-hidden="true" className="shrink-0 text-muted-foreground">
        /
      </span>
      <span
        aria-hidden="true"
        data-workflow-variable-part="variable-mark"
        className="shrink-0 font-medium text-blue-600 dark:text-blue-400"
      >
        {'{x}'}
      </span>
      <span
        data-workflow-variable-part="variable-name"
        className="truncate font-medium text-blue-600 dark:text-blue-400"
      >
        {variable.variableName}
      </span>
    </span>
  )
}

/**
 * Renders the selected variable in a trigger, or the raw selector text as a fallback.
 *
 * A saved graph may reference a selector the current catalog cannot resolve — an upstream
 * node was deleted, or the graph came from a different editor — and that selector must stay
 * visible so the author can repair it rather than have the trigger silently go blank.
 */
export function VariableSelectValue({
  catalog,
  selector,
  placeholder,
}: {
  catalog: readonly WorkflowDecoratedVariable[]
  selector: readonly string[]
  placeholder: string
}) {
  const selectorText = selector.join('.')
  if (selectorText === '') {
    return <SelectValue placeholder={placeholder} />
  }
  const variable = catalog.find((candidate) => candidate.selector.join('.') === selectorText)
  return (
    <SelectValue placeholder={placeholder}>
      {variable === undefined ? (
        selectorText
      ) : (
        <WorkflowVariableDisplay
          variable={variable}
          nodeName={variable.sourceNodeTitle ?? variable.sourceNodeId}
        />
      )}
    </SelectValue>
  )
}

/**
 * Renders the selected choice's localized label in a trigger.
 *
 * Base UI's value element shows the raw stored value, which equals the label for simple
 * catalogs but not for operator or operation choices, so the label is resolved explicitly here.
 */
export function LocalizedSelectValue({
  options,
  value,
  placeholder,
}: {
  options: readonly WorkflowChoice[]
  value: string
  placeholder?: string
}) {
  const translate = useWorkflowTranslator()
  if (value === '' && placeholder !== undefined) {
    return <SelectValue placeholder={placeholder} />
  }
  return (
    <SelectValue placeholder={placeholder}>
      {(selected) =>
        workflowChoiceLabel(
          // A choice omitted from the catalog still spells itself out, so a stored
          // option that left the catalog never renders as an empty or dangling value.
          options.find((option) => option.value === (selected ?? value)) ?? {
            value: selected ?? value,
          },
          translate,
        )
      }
    </SelectValue>
  )
}

/** Joins a selector array into its dotted text form for the editor input. */
export function selectorToText(selector: readonly string[]): string {
  return selector.join('.')
}

/** Splits dotted selector text into `[nodeId, root, ...nested]` parts. */
export function textToSelector(text: string): string[] {
  return text
    .split('.')
    .map((part) => part.trim())
    .filter((part) => part !== '')
}

/** Coerces a comparison value's text form into a JSON-like value for the backend. */
export function parseComparisonValue(text: string): unknown {
  const trimmed = text.trim()
  if (trimmed === 'true') {
    return true
  }
  if (trimmed === 'false') {
    return false
  }
  if (trimmed !== '' && Number.isFinite(Number(trimmed))) {
    return Number(trimmed)
  }
  return text
}

/**
 * A catalog-backed selector menu: the trigger shows the resolved selector and the menu groups every
 * variable the consumer may reference. Shared by the iteration region panel and the Loop/Aggregator
 * inspectors so all three pick from the same catalog.
 */
export function WorkflowCatalogSelector({
  htmlId,
  value,
  placeholder,
  choices,
  catalog,
  onPick,
  ariaLabel,
}: {
  htmlId: string
  value: readonly string[]
  placeholder: string
  choices: readonly WorkflowDecoratedVariable[]
  catalog: readonly WorkflowDecoratedVariable[]
  onPick: (selector: string[]) => void
  ariaLabel: string
}) {
  const { t } = useTranslation()
  return (
    <Select
      value={value.join('.') === '' ? null : value.join('.')}
      onValueChange={(picked) => {
        if (picked !== null) {
          onPick(textToSelector(picked))
        }
      }}
    >
      <SelectTrigger className="h-8 w-full bg-background" id={htmlId} aria-label={ariaLabel}>
        <VariableSelectValue catalog={catalog} selector={value} placeholder={placeholder} />
      </SelectTrigger>
      <SelectContent alignItemWithTrigger={false} align="start" className="w-70 min-w-70 max-w-70">
        <WorkflowVariableSelectContent
          variables={choices}
          globalVariablesLabel={t('workflows.variable.global')}
        />
      </SelectContent>
    </Select>
  )
}

/** The type menu every workflow variable declaration shares: globals and Loop carried values. */
export function WorkflowVariableTypeMenu() {
  return (
    <SelectContent>
      {WORKFLOW_VARIABLE_VALUE_TYPES.map((valueType) => (
        <SelectItem key={valueType} value={valueType}>
          {valueType}
        </SelectItem>
      ))}
    </SelectContent>
  )
}

/** Renders a comparison value back into its editable text form. */
export function comparisonValueToText(value: unknown): string {
  if (value === undefined || value === null) {
    return ''
  }
  return typeof value === 'string' ? value : JSON.stringify(value)
}
