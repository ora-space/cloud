import { useTranslation } from 'react-i18next'
import { Plus, Trash2 } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Textarea } from '@/components/ui/textarea'
import {
  createDefaultWorkflowCapabilities,
  workflowChoiceLabel,
} from '@/features/workflows/runtime/capabilities'
import type {
  WorkflowGlobalVariable,
  WorkflowNodeData,
  WorkflowToolParameter,
} from '@/features/workflows/runtime/types'
import { InspectorField } from '@/features/workflows/editor/workflow-inspector-fields'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import type { TranslationKey } from '@/i18n/i18n-instance'
import { LocalizedChoiceSelect } from '@/features/workflows/editor/workflow-choice-select'
import { LocalizedSelectValue } from '@/features/workflows/editor/workflow-variable-select'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'

/**
 * Everything the kind-specific panels may read beyond the selected node itself.
 *
 * Only Condition and Iteration panels read the catalog and the graph; the rest ignore what
 * they do not need, so one shared props shape keeps the dispatcher and the panels aligned.
 */
export interface WorkflowKindFieldsProps {
  /** Id of the node the panel edits; the root of an Iteration region-membership lookup. */
  nodeId: string
  data: WorkflowNodeData
  onChange: (data: WorkflowNodeData) => void
  /** Variables the selected node may reference, globals first, already decorated. */
  catalog: readonly WorkflowDecoratedVariable[]
  /** Whole-graph nodes, for region-member lookups the Iteration panel needs. */
  graphNodes: readonly WorkflowCanvasNode[]
  /** Workflow-wide declarations, normalized to include the required system globals. */
  globalVariables: readonly WorkflowGlobalVariable[]
}

/** Edits a Tool node: the tool, its derived operation, and key/value parameters. */
export function ToolNodeFields({
  data,
  onChange,
}: Pick<WorkflowKindFieldsProps, 'data' | 'onChange'>) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const capabilities = createDefaultWorkflowCapabilities()
  const selectedTool = data.tool ?? capabilities.defaultTool
  const operations = capabilities.toolOperations[selectedTool] ?? []
  const firstOperation = operations[0]?.value
  return (
    <>
      <InspectorField label={t('workflows.tool.field.tool')} htmlFor="workflow-node-tool">
        <Select
          value={selectedTool}
          onValueChange={(tool) => {
            if (tool !== null) {
              const firstOperationOf = capabilities.toolOperations[tool]?.[0]?.value
              onChange({
                ...data,
                tool,
                // Switch to the first operation of the newly selected tool.
                ...(firstOperationOf === undefined ? {} : { operation: firstOperationOf }),
              })
            }
          }}
        >
          <SelectTrigger id="workflow-node-tool" className="w-full">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {capabilities.tools.map((tool) => (
              <SelectItem key={tool.value} value={tool.value}>
                {workflowChoiceLabel(tool, translate)}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </InspectorField>
      {operations.length > 0 ? (
        <InspectorField
          label={t('workflows.tool.field.operation')}
          htmlFor="workflow-node-operation"
        >
          <Select
            value={firstOperation === undefined ? '' : (data.operation ?? firstOperation)}
            onValueChange={(operation) => {
              if (operation !== null) {
                onChange({ ...data, operation })
              }
            }}
          >
            <SelectTrigger id="workflow-node-operation" className="w-full">
              <LocalizedSelectValue
                options={operations}
                value={firstOperation === undefined ? '' : (data.operation ?? firstOperation)}
              />
            </SelectTrigger>
            <SelectContent>
              {operations.map((operation) => (
                <SelectItem key={operation.value} value={operation.value}>
                  {workflowChoiceLabel(operation, translate)}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </InspectorField>
      ) : (
        <p className="text-[11px] text-muted-foreground">{t('workflows.tool.noOperations')}</p>
      )}
      <ToolParametersField data={data} onChange={onChange} />
    </>
  )
}

/** The key/value call parameters of a Tool node. */
function ToolParametersField({
  data,
  onChange,
}: {
  data: WorkflowNodeData
  onChange: (data: WorkflowNodeData) => void
}) {
  const { t } = useTranslation()
  const toolParameters = data.toolParameters ?? []
  const updateParameters = (parameters: WorkflowToolParameter[]): void => {
    onChange({ ...data, toolParameters: parameters })
  }
  return (
    <InspectorField label={t('workflows.section.parameters')} htmlFor="workflow-tool-parameters">
      <div className="space-y-2" id="workflow-tool-parameters">
        {toolParameters.map((parameter, index) => (
          <div
            key={toolParameterKey(parameter)}
            className="grid grid-cols-[minmax(0,1fr)_minmax(0,1fr)_auto] items-center gap-2"
          >
            <Input
              value={parameter.key}
              aria-label={t('workflows.tool.parameterName', { index: index + 1 })}
              placeholder={t('workflows.tool.parameterName')}
              className="h-8"
              onChange={(event) =>
                updateParameters(
                  toolParameters.map((candidate, candidateIndex) =>
                    candidateIndex === index
                      ? { ...candidate, key: event.target.value }
                      : candidate,
                  ),
                )
              }
            />
            <Input
              value={parameter.value}
              aria-label={t('workflows.tool.parameterValue', { index: index + 1 })}
              placeholder={t('workflows.tool.parameterValue')}
              className="h-8"
              onChange={(event) =>
                updateParameters(
                  toolParameters.map((candidate, candidateIndex) =>
                    candidateIndex === index
                      ? { ...candidate, value: event.target.value }
                      : candidate,
                  ),
                )
              }
            />
            <Button
              type="button"
              variant="ghost"
              size="icon-sm"
              className="shrink-0 text-muted-foreground hover:bg-destructive/10 hover:text-destructive"
              aria-label={t('workflows.tool.removeParameter', { index: index + 1 })}
              onClick={() =>
                updateParameters(
                  toolParameters.filter((_, candidateIndex) => candidateIndex !== index),
                )
              }
            >
              <Trash2 className="size-3.5" />
            </Button>
          </div>
        ))}
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="w-full justify-start"
          onClick={() => updateParameters([...toolParameters, { key: '', value: '' }])}
        >
          <Plus />
          {t('workflows.tool.addParameter')}
        </Button>
      </div>
    </InspectorField>
  )
}

/** Edits the merge behavior of a Junction node: wait and failure strategies. */
export function JunctionNodeFields({
  data,
  onChange,
}: Pick<WorkflowKindFieldsProps, 'data' | 'onChange'>) {
  const { t } = useTranslation()
  return (
    <>
      <LocalizedChoiceSelect
        id="workflow-node-wait-strategy"
        label={t('workflows.junction.field.waitStrategy')}
        value={data.waitStrategy ?? 'all'}
        options={WORKFLOW_JUNCTION_WAIT_CHOICES}
        onChange={(strategy) => {
          if (strategy === 'all' || strategy === 'any' || strategy === 'count') {
            onChange({ ...data, waitStrategy: strategy })
          }
        }}
      />
      {data.waitStrategy === 'count' && (
        <InspectorField
          label={t('workflows.junction.field.waitCount')}
          htmlFor="workflow-node-wait-count"
        >
          <Input
            id="workflow-node-wait-count"
            type="number"
            min={1}
            value={data.waitCount ?? 1}
            onChange={(event) => {
              const parsed = Number(event.target.value)
              // Dropping the count removes the key rather than writing `undefined`,
              // which would trip `exactOptionalPropertyTypes` on the wire format.
              const next: WorkflowNodeData = { ...data }
              if (event.target.value !== '' && Number.isFinite(parsed)) {
                next.waitCount = parsed
              } else {
                delete next.waitCount
              }
              onChange(next)
            }}
          />
        </InspectorField>
      )}
      <LocalizedChoiceSelect
        id="workflow-node-failure-strategy"
        label={t('workflows.junction.field.failureStrategy')}
        value={data.failureStrategy ?? 'fail'}
        options={WORKFLOW_JUNCTION_FAILURE_CHOICES}
        onChange={(strategy) => {
          if (strategy === 'fail' || strategy === 'continue') {
            onChange({ ...data, failureStrategy: strategy })
          }
        }}
      />
    </>
  )
}

/** Junction wait strategy choices keyed to the graph's enum names. */
const WORKFLOW_JUNCTION_WAIT_CHOICES: readonly {
  value: 'all' | 'any' | 'count'
  labelKey: TranslationKey
}[] = [
  { value: 'all', labelKey: 'workflows.junction.waitAll' },
  { value: 'any', labelKey: 'workflows.junction.waitAny' },
  { value: 'count', labelKey: 'workflows.junction.waitCount' },
]

/** Junction failure strategy choices keyed to the graph's enum names. */
const WORKFLOW_JUNCTION_FAILURE_CHOICES: readonly {
  value: 'fail' | 'continue'
  labelKey: TranslationKey
}[] = [
  { value: 'fail', labelKey: 'workflows.junction.failFast' },
  { value: 'continue', labelKey: 'workflows.junction.collectResults' },
]

/** Edits the approval prompt a Human node shows to the reviewer. */
export function HumanNodeFields({
  data,
  onChange,
}: Pick<WorkflowKindFieldsProps, 'data' | 'onChange'>) {
  const { t } = useTranslation()
  return (
    <InspectorField
      label={t('workflows.human.field.approvalPrompt')}
      htmlFor="workflow-node-instruction"
    >
      <Textarea
        id="workflow-node-instruction"
        className="min-h-32 resize-none text-xs leading-5"
        value={data.instruction ?? ''}
        onChange={(event) => onChange({ ...data, instruction: event.target.value })}
      />
    </InspectorField>
  )
}

/** Explains the loopExit marker: it ends the round and carries no configuration of its own. */
export function LoopExitNodeFields() {
  const { t } = useTranslation()
  return (
    <p className="rounded-lg border border-border bg-muted/25 px-3 py-2 text-[11px] leading-5 text-muted-foreground">
      {t('workflows.node.loopExit.description')}
    </p>
  )
}

/** Notes that a Subflow node is a placeholder until the V2 execution engine lands. */
export function SubflowNodeFields() {
  const { t } = useTranslation()
  return (
    <p className="rounded-lg border border-border bg-muted/25 px-3 py-2 text-[11px] leading-5 text-muted-foreground">
      {t('workflows.subflow.hint')}
    </p>
  )
}

/** A stable per-row key for one tool call parameter, built from its content. */
function toolParameterKey(parameter: WorkflowToolParameter): string {
  return `${parameter.key}::${parameter.value}`
}
