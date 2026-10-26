import { useTranslation } from 'react-i18next'
import {
  createDefaultWorkflowCapabilities,
  workflowChoiceLabel,
  type WorkflowCapabilities,
} from '@/features/workflows/runtime/capabilities'
import { resolveConditionCases } from '@/features/workflows/runtime/condition-cases'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { comparisonValueToText } from '@/features/workflows/editor/workflow-variable-select'

/** One labelled group of values shown on a card. */
export interface NodeParameter {
  label: string
  values: string[]
}

/**
 * Renders a card's saved configuration as the section below its header.
 *
 * Read-only by design: the card shows what is configured so a graph is readable
 * at a glance, and the inspector is the only place that edits it. The divider
 * lives here rather than in the card so an unconfigured node adds no empty
 * section to its own layout.
 *
 * @param data - Node data to summarize.
 * @returns The details section, or `null` when nothing is configured yet.
 */
export function NodeParameterSummary({ data }: { data: WorkflowNodeData }) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const parameters = configuredParameters(data, translate)
  if (parameters.length === 0) {
    return null
  }
  return (
    <>
      <div className="mx-auto w-4/5 border-t border-border" />
      <dl aria-label={t('workflows.inspector.nodeParameters')} className="space-y-2 px-3 pt-2 pb-3">
        {parameters.map((parameter) => (
          <div key={parameter.label} className="min-w-0">
            <dt className="mb-1 text-[9px] font-medium text-muted-foreground">{parameter.label}</dt>
            <div className="space-y-1">
              {parameter.values.map((value) => (
                <dd
                  key={value}
                  className="m-0 line-clamp-2 rounded-md bg-muted px-2 py-1 text-[10px] leading-4 break-words text-foreground/85 shadow-inner"
                >
                  {value}
                </dd>
              ))}
            </div>
          </div>
        ))}
      </dl>
    </>
  )
}

/**
 * Collects the fields worth showing, skipping empty ones.
 *
 * @param data - Node data to read.
 * @param translate - Resolves a translation key.
 * @returns The populated parameters, in display order.
 */
export function configuredParameters(
  data: WorkflowNodeData,
  translate: (key: string) => string,
): NodeParameter[] {
  const parameters: NodeParameter[] = []
  const capabilities = createDefaultWorkflowCapabilities()
  if (data.kind === 'start' && (data.input ?? '') !== '') {
    parameters.push({
      label: translate('workflows.inspector.field.initialPrompt'),
      values: [data.input ?? ''],
    })
  }
  if (data.kind === 'agent') {
    parameters.push(...agentParameters(data, capabilities, translate))
  }
  parameters.push(...kindSpecificParameters(data, capabilities, translate))
  return parameters
}

/**
 * Summaries for the kinds with their own configuration sections.
 *
 * Kinds that carry no editable configuration (start, output, subflow) fall into
 * the default branch and add nothing.
 *
 * @param data - Node data to read.
 * @param capabilities - The catalogs labels resolve against.
 * @param translate - Resolves a translation key.
 * @returns The populated parameter rows, empty when the kind has nothing to show.
 */
function kindSpecificParameters(
  data: WorkflowNodeData,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): NodeParameter[] {
  const instruction = data.instruction ?? ''
  switch (data.kind) {
    case 'condition': {
      const summary = conditionSummary(data, capabilities, translate)
      return summary === undefined
        ? []
        : [{ label: translate('workflows.summary.condition'), values: [summary] }]
    }
    case 'tool':
      return toolParameters(data, capabilities, translate)
    case 'junction':
      return junctionParameters(data, translate)
    case 'loop':
      return loopParameters(data, translate)
    case 'iteration':
      return iterationParameters(data, translate)
    case 'human':
      return instruction.trim() === ''
        ? []
        : [{ label: translate('workflows.human.field.approvalPrompt'), values: [instruction] }]
    default:
      return []
  }
}

/**
 * Reads the role and model an Agent node is bound to.
 *
 * @param data - Node data to read.
 * @param capabilities - The catalogs labels resolve against.
 * @param translate - Resolves a translation key.
 * @returns The role and model rows, empty when no contract is configured.
 */
function agentParameters(
  data: WorkflowNodeData,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): NodeParameter[] {
  const config = data.agentConfig
  if (config === undefined) {
    return []
  }
  // A node saved under the executor-slot schema carries `executorSlot` instead of
  // the legacy `executor` binding this build still summarizes. There is no model
  // row to read from it, so leave the config section empty rather than crash on
  // the missing `executor`; slot display belongs to the editor (#154).
  const executor = config.executor
  if (executor === undefined) {
    return []
  }
  const role = capabilities.roles.find((choice) => choice.value === config.roleId)
  const model = `${executor.agentCli} · ${executor.modelId}`
  return [
    {
      label: translate('workflows.inspector.field.role'),
      values: [role === undefined ? config.roleId : workflowChoiceLabel(role, translate)],
    },
    { label: translate('workflows.inspector.field.agentModel'), values: [model] },
  ]
}

/**
 * Compacts one comparison row into `变量 操作符 值`, joining a case by its logic.
 *
 * @param data - Node data to read.
 * @param capabilities - The operator catalog labels resolve against.
 * @param translate - Resolves a translation key.
 * @returns One joinable line per case, or `undefined` when nothing is authored.
 */
function conditionSummary(
  data: WorkflowNodeData,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): string | undefined {
  const andConnector = translate('workflows.summary.connectorAnd')
  const orConnector = translate('workflows.summary.connectorOr')
  const caseConnector = translate('workflows.summary.connectorCases')
  const lines = resolveConditionCases(data).flatMap((conditionCase) => {
    const line = conditionCase.conditions
      .map((comparison) =>
        [
          comparison.variableSelector.join('.'),
          operatorLabel(comparison.operator, capabilities, translate),
          comparisonValueLabel(comparison.value),
        ]
          .filter((part) => part !== '')
          .join(' '),
      )
      .filter((part) => part !== '')
      .join(conditionCase.logic === 'or' ? orConnector : andConnector)
    return line === '' ? [] : line
  })
  return lines.length === 0 ? undefined : lines.join(caseConnector)
}

/** Resolves a comparison operator to its localized menu text. */
function operatorLabel(
  operator: string,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): string {
  const choice = capabilities.conditionOperators.find((candidate) => candidate.value === operator)
  return choice === undefined ? operator : workflowChoiceLabel(choice, translate)
}

/** Renders a comparison value as text, JSON for non-strings. */
function comparisonValueLabel(value: unknown): string {
  return comparisonValueToText(value)
}

/** Reads the tool, its derived operation, and the key/value call parameters. */
function toolParameters(
  data: WorkflowNodeData,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): NodeParameter[] {
  const parameters: NodeParameter[] = []
  if ((data.tool ?? '') !== '') {
    parameters.push({ label: translate('workflows.tool.field.tool'), values: [data.tool ?? ''] })
  }
  if ((data.operation ?? '') !== '') {
    const operation = operationLabel(data.operation ?? '', capabilities, translate)
    parameters.push({ label: translate('workflows.tool.field.operation'), values: [operation] })
  }
  const calls = (data.toolParameters ?? [])
    .filter((parameter) => parameter.key !== '' || parameter.value !== '')
    .map((parameter) => `${parameter.key} = ${parameter.value}`)
  if (calls.length > 0) {
    parameters.push({ label: translate('workflows.section.parameters'), values: calls })
  }
  return parameters
}

/** Resolves a tool operation to its localized menu text. */
function operationLabel(
  operation: string,
  capabilities: WorkflowCapabilities,
  translate: (key: string) => string,
): string {
  const operations = Object.values(capabilities.toolOperations).flat()
  const choice = operations.find((candidate) => candidate.value === operation)
  return choice === undefined ? operation : workflowChoiceLabel(choice, translate)
}

/** Reads the wait and failure strategies of a Junction node. */
function junctionParameters(
  data: WorkflowNodeData,
  translate: (key: string) => string,
): NodeParameter[] {
  return [
    {
      label: translate('workflows.junction.field.waitStrategy'),
      values: [junctionStrategyLabel(data.waitStrategy, translate)],
    },
    {
      label: translate('workflows.junction.field.failureStrategy'),
      values: [junctionFailureLabel(data.failureStrategy, translate)],
    },
  ]
}

/** Localizes the wait strategy enum name. */
function junctionStrategyLabel(
  strategy: string | undefined,
  translate: (key: string) => string,
): string {
  if (strategy === 'any') {
    return translate('workflows.junction.waitAny')
  }
  if (strategy === 'count') {
    return translate('workflows.junction.waitCount')
  }
  return translate('workflows.junction.waitAll')
}

/** Localizes the failure strategy enum name. */
function junctionFailureLabel(
  strategy: string | undefined,
  translate: (key: string) => string,
): string {
  return strategy === 'continue'
    ? translate('workflows.junction.collectResults')
    : translate('workflows.junction.failFast')
}

/** Reads the iteration ceiling and carried initial value of a Loop node. */
function loopParameters(
  data: WorkflowNodeData,
  translate: (key: string) => string,
): NodeParameter[] {
  const loopConfig = data.loopConfig
  if (loopConfig === undefined) {
    return []
  }
  const carriedVariable = loopConfig.variables[0]
  const initialValue =
    carriedVariable?.initial.kind === 'constant'
      ? comparisonValueToText(carriedVariable.initial.value)
      : carriedVariable?.initial.selector.join('.')
  const parameters: NodeParameter[] = [
    {
      label: translate('workflows.loop.field.maxIterations'),
      values: [String(loopConfig.maxIterations)],
    },
  ]
  if (initialValue !== undefined && initialValue !== '') {
    parameters.push({
      label: translate('workflows.loop.field.initialValue'),
      values: [initialValue],
    })
  }
  return parameters
}

/** Reads the iterator, collect target, and limits of an Iteration node. */
function iterationParameters(
  data: WorkflowNodeData,
  translate: (key: string) => string,
): NodeParameter[] {
  const config = data.iterationConfig
  if (config === undefined) {
    return []
  }
  return [
    {
      label: translate('workflows.iteration.field.iterator'),
      values: [config.iteratorSelector.join('.')],
    },
    {
      label: translate('workflows.iteration.field.collect'),
      values: [config.collectSelector.join('.')],
    },
    {
      label: translate('workflows.iteration.field.errorStrategy'),
      values: [
        config.errorStrategy === 'continue'
          ? translate('workflows.iteration.continueStrategy')
          : translate('workflows.iteration.failStrategy'),
      ],
    },
    {
      label: translate('workflows.loop.field.maxIterations'),
      values: [String(config.maxIterations)],
    },
  ]
}
