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
import {
  createDefaultWorkflowCapabilities,
  type WorkflowChoice,
} from '@/features/workflows/runtime/capabilities'
import type {
  WorkflowConditionComparison,
  WorkflowGlobalVariable,
  WorkflowLoopConfig,
  WorkflowLoopVariable,
  WorkflowNodeData,
  WorkflowOutputBinding,
} from '@/features/workflows/runtime/types'
import type { TranslationKey } from '@/i18n/i18n-instance'
import { InspectorField } from '@/features/workflows/editor/workflow-inspector-fields'
import { loopExitInsertion } from '@/features/workflows/runtime/loop-graph'
import {
  LocalizedChoiceSelect,
  WorkflowChoiceSelect,
} from '@/features/workflows/editor/workflow-choice-select'
import {
  WorkflowCatalogSelector,
  WorkflowVariableTypeMenu,
  comparisonValueToText,
  parseComparisonValue,
} from '@/features/workflows/editor/workflow-variable-select'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import {
  decorateWorkflowVariableCatalog,
  deriveWorkflowVariableCatalog,
  normalizeWorkflowGlobalVariables,
} from '@/features/workflows/runtime/variable-catalog'

/** Everything the Loop panels read: the node, its writer and the graph the catalog derives from. */
interface LoopFieldsProps {
  data: WorkflowNodeData
  onChange: (data: WorkflowNodeData) => void
  /** The loop node's id, which owns carried variables and collects member roots. */
  nodeId: string
  /** Whole-graph nodes and edges; the selector catalog derives from them. */
  graphNodes: readonly WorkflowCanvasNode[]
  edges: readonly WorkflowCanvasEdge[]
  globalVariables: readonly WorkflowGlobalVariable[]
}
/** Exhaustion policies of a Loop, keyed to the graph's enum names. */
const WORKFLOW_LOOP_BEHAVIOR_CHOICES = [
  { value: 'fail', labelKey: 'workflows.loop.behavior.fail' },
  { value: 'succeed', labelKey: 'workflows.loop.behavior.succeed' },
] as const satisfies readonly WorkflowChoice[]

/** Edits a Loop container: budget, carried initial value, and the behavior note. */
export function LoopNodeFields({
  data,
  onChange,
  nodeId,
  graphNodes,
  edges,
  globalVariables,
  onAddExit,
}: LoopFieldsProps & { onAddExit?: () => void }) {
  const { t } = useTranslation()
  const loopConfig = data.loopConfig
  const normalizedGlobals = normalizeWorkflowGlobalVariables(globalVariables)
  const initialChoices = decorateWorkflowVariableCatalog(
    deriveWorkflowVariableCatalog(graphNodes, edges, nodeId, normalizedGlobals),
    graphNodes,
    normalizedGlobals,
  )
  const choices = resolveLoopChoices(graphNodes, edges, {
    loopId: nodeId,
    globalVariables,
    ...(loopConfig === undefined ? {} : { config: loopConfig }),
  })
  return (
    <>
      <LoopBudgetFields data={data} onChange={onChange} />
      <LoopVariablesFields
        data={data}
        onChange={onChange}
        choices={choices}
        initialChoices={initialChoices}
      />
      <LoopUntilFields data={data} onChange={onChange} choices={choices} />
      <LoopOutputsFields
        data={data}
        onChange={onChange}
        nodeId={nodeId}
        graphNodes={graphNodes}
        edges={edges}
        globalVariables={globalVariables}
        choices={choices}
      />
      {onAddExit === undefined ? null : (
        <LoopExitAction
          nodeId={nodeId}
          graphNodes={graphNodes}
          edges={edges}
          onAddExit={onAddExit}
        />
      )}
      <p className="rounded-lg border border-border bg-muted/25 px-3 py-2 text-[11px] leading-5 text-muted-foreground">
        {loopConfig === undefined ? t('workflows.loop.legacyUnsupported') : null}
      </p>
    </>
  )
}

/**
 * The inspector's "add early exit" action, offered only where it has a seam.
 *
 * The exit is appended after the right-most body member that does round work; with only the body
 * Start and Conditions there is none (an exit after Start would end every round at once, and one
 * after a Condition would silently take its ELSE). The action is then disabled and says where a
 * branch's exit comes from instead.
 */
function LoopExitAction({
  nodeId,
  graphNodes,
  edges,
  onAddExit,
}: Pick<LoopFieldsProps, 'nodeId' | 'graphNodes' | 'edges'> & { onAddExit: () => void }) {
  const { t } = useTranslation()
  const available =
    loopExitInsertion({ nodes: [...graphNodes], edges: [...edges] }, nodeId) !== null
  // With a Condition in the body the exit belongs on one of its branches; without one there is
  // simply no step yet to end the round after.
  const hasBranch = graphNodes.some(
    (node) => node.data.kind === 'condition' && (node.data.containerId ?? node.parentId) === nodeId,
  )
  return (
    <div className="space-y-1.5">
      <Button type="button" variant="outline" size="sm" disabled={!available} onClick={onAddExit}>
        <Plus />
        {t('workflows.loop.addExitNode')}
      </Button>
      {!available && (
        <p className="text-[11px] leading-5 text-muted-foreground">
          {t(hasBranch ? 'workflows.loop.addExitFromBranch' : 'workflows.loop.addExitNeedsStep')}
        </p>
      )}
    </div>
  )
}

/** Edits the round ceiling and what an exhausted budget does. */
function LoopBudgetFields({ data, onChange }: Pick<LoopFieldsProps, 'data' | 'onChange'>) {
  const { t } = useTranslation()
  const loopConfig = data.loopConfig
  const updateMaxIterations = (value: number): void => {
    if (loopConfig === undefined) {
      return
    }
    onChange({
      ...data,
      loopConfig: {
        ...loopConfig,
        maxIterations: Math.min(100, Math.max(1, Math.trunc(value))),
      },
    })
  }
  const updateMaxIterationsBehavior = (value: string | null): void => {
    if (loopConfig === undefined || (value !== 'fail' && value !== 'succeed')) {
      return
    }
    onChange({
      ...data,
      loopConfig: { ...loopConfig, maxIterationsBehavior: value },
    })
  }
  return (
    <>
      <InspectorField
        label={t('workflows.loop.field.maxIterations')}
        htmlFor="workflow-node-max-iterations"
      >
        <Input
          id="workflow-node-max-iterations"
          type="number"
          min={1}
          max={100}
          value={loopConfig?.maxIterations ?? 3}
          disabled={loopConfig === undefined}
          onChange={(event) => {
            const parsed = Number(event.target.value)
            if (Number.isFinite(parsed)) {
              updateMaxIterations(parsed)
            }
          }}
        />
      </InspectorField>
      <LocalizedChoiceSelect
        id="workflow-node-max-iterations-behavior"
        label={t('workflows.loop.field.maxIterationsBehavior')}
        value={loopConfig?.maxIterationsBehavior ?? 'fail'}
        options={WORKFLOW_LOOP_BEHAVIOR_CHOICES}
        disabled={loopConfig === undefined}
        onChange={updateMaxIterationsBehavior}
      />
    </>
  )
}

/** Parses a typed constant: text stays text for string types, otherwise it is JSON-read. */
function loopConstantValue(text: string, valueType: string): unknown {
  return valueType === 'string' || valueType === 'secret' ? text : parseComparisonValue(text)
}

/** Builds the empty carried variable a new row starts from. */
function blankLoopVariable(): WorkflowLoopVariable {
  return { name: '', valueType: 'string', initial: { kind: 'constant', value: '' }, feedback: [] }
}

/** Edits every carried variable: its name, type, first-round value and feedback selector. */
function LoopVariablesFields({
  data,
  onChange,
  choices,
  initialChoices,
}: Pick<LoopFieldsProps, 'data' | 'onChange'> & {
  initialChoices: readonly WorkflowDecoratedVariable[]
  choices: readonly WorkflowDecoratedVariable[]
}) {
  const { t } = useTranslation()
  const loopConfig = data.loopConfig
  if (loopConfig === undefined) {
    return null
  }
  const update = (variables: WorkflowLoopVariable[]): void => {
    onChange({ ...data, loopConfig: { ...loopConfig, variables } })
  }
  return (
    <InspectorField label={t('workflows.loop.field.variables')} htmlFor="workflow-loop-variables">
      <div className="space-y-2" id="workflow-loop-variables">
        {/* oxlint-disable react/no-array-index-key -- a carried variable has no stable id; its name is the editable identity, so a content key collides on a blank new row. */}
        {loopConfig.variables.map((variable, index) => (
          <LoopVariableRow
            key={`loop-variable-${index}`}
            index={index}
            variable={variable}
            choices={choices}
            initialChoices={initialChoices}
            onChange={(next) =>
              update(loopConfig.variables.map((current, i) => (i === index ? next : current)))
            }
            onRemove={() => update(loopConfig.variables.filter((_, i) => i !== index))}
          />
        ))}
        {/* oxlint-enable react/no-array-index-key */}
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => update([...loopConfig.variables, blankLoopVariable()])}
      >
        <Plus />
        {t('workflows.loop.addVariable')}
      </Button>
    </InspectorField>
  )
}

/** One carried variable's fields; the index only labels the controls for assistive technology. */
function LoopVariableRow({
  index,
  variable,
  choices,
  initialChoices,
  onChange,
  onRemove,
}: {
  index: number
  variable: WorkflowLoopVariable
  initialChoices: readonly WorkflowDecoratedVariable[]
  choices: readonly WorkflowDecoratedVariable[]
  onChange: (variable: WorkflowLoopVariable) => void
  onRemove: () => void
}) {
  const { t } = useTranslation()
  const label = (key: TranslationKey): string => `${t(key)} ${index + 1}`
  return (
    <div className="space-y-2 rounded-lg border border-border p-2">
      <div className="flex items-center gap-2">
        <Input
          aria-label={label('workflows.loop.field.variableName')}
          value={variable.name}
          onChange={(event) => onChange({ ...variable, name: event.target.value })}
        />
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label={label('workflows.loop.removeVariable')}
          onClick={onRemove}
        >
          <Trash2 />
        </Button>
      </div>
      <div className="flex gap-2">
        <Select
          value={variable.valueType}
          onValueChange={(valueType) => {
            if (valueType !== null) {
              onChange(loopVariableWithType(variable, valueType))
            }
          }}
        >
          <SelectTrigger
            aria-label={label('workflows.loop.field.variableType')}
            className="h-8 w-full"
          >
            <SelectValue />
          </SelectTrigger>
          <WorkflowVariableTypeMenu />
        </Select>
        <Select
          value={variable.initial.kind}
          onValueChange={(kind) => {
            if (kind === 'constant') {
              onChange({ ...variable, initial: { kind: 'constant', value: '' } })
            } else if (kind === 'variable') {
              onChange({ ...variable, initial: { kind: 'variable', selector: [] } })
            }
          }}
        >
          <SelectTrigger
            aria-label={label('workflows.loop.field.variableInitialKind')}
            className="h-8 w-full"
          >
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="constant">{t('workflows.loop.initialKind.constant')}</SelectItem>
            <SelectItem value="variable">{t('workflows.loop.initialKind.variable')}</SelectItem>
          </SelectContent>
        </Select>
      </div>
      <LoopInitialValueInput
        index={index}
        variable={variable}
        choices={initialChoices}
        onChange={onChange}
      />
      <WorkflowCatalogSelector
        htmlId={`workflow-loop-variable-feedback-${index}`}
        value={variable.feedback}
        placeholder={t('workflows.loop.selectorPlaceholder')}
        choices={choices}
        catalog={choices}
        ariaLabel={label('workflows.loop.field.variableFeedback')}
        onPick={(picked) => onChange({ ...variable, feedback: picked })}
      />
    </div>
  )
}

/**
 * The first-round value: a typed constant, or a reference resolved once before the loop starts.
 * Switching kinds replaces the value, because a constant and a selector cannot be converted into
 * each other without guessing what the author meant.
 */
function LoopInitialValueInput({
  index,
  variable,
  choices,
  onChange,
}: {
  index: number
  variable: WorkflowLoopVariable
  choices: readonly WorkflowDecoratedVariable[]
  onChange: (variable: WorkflowLoopVariable) => void
}) {
  const { t } = useTranslation()
  const label = `${t('workflows.loop.field.variableInitial')} ${index + 1}`
  if (variable.initial.kind === 'constant') {
    return (
      <Input
        aria-label={label}
        value={comparisonValueToText(variable.initial.value)}
        onChange={(event) =>
          onChange({
            ...variable,
            initial: {
              kind: 'constant',
              value: loopConstantValue(event.target.value, variable.valueType),
            },
          })
        }
      />
    )
  }
  return (
    <WorkflowCatalogSelector
      htmlId={`workflow-loop-variable-initial-${index}`}
      value={variable.initial.selector}
      placeholder={t('workflows.loop.selectorPlaceholder')}
      choices={choices}
      catalog={choices}
      ariaLabel={label}
      onPick={(picked) =>
        onChange({ ...variable, initial: { kind: 'variable', selector: picked } })
      }
    />
  )
}

/** How a Loop's round-end rules combine. */
const LOOP_UNTIL_LOGIC_CHOICES = [
  { value: 'and', labelKey: 'workflows.condition.logicAnd' },
  { value: 'or', labelKey: 'workflows.condition.logicOr' },
] as const satisfies readonly WorkflowChoice[]

/** Operators that answer a question about the value, so they take no expected value. */
const LOOP_OPERATORS_WITHOUT_VALUE = new Set(['exists', 'not_exists', 'empty', 'not_empty'])

/** Edits the round-end condition: how rules combine and the rules themselves. */
function LoopUntilFields({
  data,
  onChange,
  choices,
}: Pick<LoopFieldsProps, 'data' | 'onChange'> & {
  choices: readonly WorkflowDecoratedVariable[]
}) {
  const { t } = useTranslation()
  const loopConfig = data.loopConfig
  if (loopConfig === undefined) {
    return null
  }
  const until = loopConfig.until
  const update = (next: WorkflowLoopConfig['until']): void => {
    onChange({ ...data, loopConfig: { ...loopConfig, until: next } })
  }
  return (
    <InspectorField label={t('workflows.loop.field.until')} htmlFor="workflow-loop-until">
      <div className="space-y-2" id="workflow-loop-until">
        <WorkflowChoiceSelect
          id="workflow-loop-until-logic"
          ariaLabel={t('workflows.loop.field.until')}
          value={until.logic}
          options={LOOP_UNTIL_LOGIC_CHOICES}
          className="h-8 w-full"
          onChange={(logic) => {
            if (logic === 'and' || logic === 'or') {
              update({ ...until, logic })
            }
          }}
        />
        {/* oxlint-disable react/no-array-index-key -- a rule has no stable id; its selector is the editable identity, so a content key collides on a blank new rule. */}
        {until.conditions.map((condition, index) => (
          <LoopUntilRow
            key={`loop-until-${index}`}
            index={index}
            condition={condition}
            choices={choices}
            onChange={(next) =>
              update({
                ...until,
                conditions: until.conditions.map((current, i) => (i === index ? next : current)),
              })
            }
            onRemove={() =>
              update({
                ...until,
                conditions: until.conditions.filter((_, i) => i !== index),
              })
            }
          />
        ))}
        {/* oxlint-enable react/no-array-index-key */}
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() =>
          update({
            ...until,
            conditions: [...until.conditions, { variableSelector: [], operator: 'equals' }],
          })
        }
      >
        <Plus />
        {t('workflows.loop.addUntilRule')}
      </Button>
    </InspectorField>
  )
}

/** One round-end comparison: a variable, an operator and, for value operators, the expected value. */
function LoopUntilRow({
  index,
  condition,
  choices,
  onChange,
  onRemove,
}: {
  index: number
  condition: WorkflowConditionComparison
  choices: readonly WorkflowDecoratedVariable[]
  onChange: (condition: WorkflowConditionComparison) => void
  onRemove: () => void
}) {
  const { t } = useTranslation()
  const capabilities = createDefaultWorkflowCapabilities()
  return (
    <div className="space-y-2 rounded-lg border border-border p-2">
      <div className="flex items-center justify-between">
        <span className="text-[11px] font-medium text-muted-foreground">IF {index + 1}</span>
        <Button
          type="button"
          variant="ghost"
          size="icon"
          aria-label={`${t('workflows.loop.removeUntilRule')} ${index + 1}`}
          onClick={onRemove}
        >
          <Trash2 />
        </Button>
      </div>
      <WorkflowCatalogSelector
        htmlId={`workflow-loop-until-variable-${index}`}
        value={condition.variableSelector}
        placeholder={t('workflows.loop.selectorPlaceholder')}
        choices={choices}
        catalog={choices}
        ariaLabel={t('workflows.condition.field.variable', { index: index + 1 })}
        onPick={(picked) => onChange({ ...condition, variableSelector: picked })}
      />
      <WorkflowChoiceSelect
        id={`workflow-loop-until-operator-${index}`}
        ariaLabel={t('workflows.condition.field.operator', { index: index + 1 })}
        value={condition.operator}
        options={capabilities.conditionOperators}
        className="h-8 w-full"
        onChange={(operator) => {
          if (operator !== null) {
            onChange({ ...condition, operator })
          }
        }}
      />
      {LOOP_OPERATORS_WITHOUT_VALUE.has(condition.operator) ? null : (
        <Input
          aria-label={t('workflows.condition.field.value', { index: index + 1 })}
          value={comparisonValueToText(condition.value)}
          onChange={(event) =>
            onChange({ ...condition, value: parseComparisonValue(event.target.value) })
          }
        />
      )}
    </div>
  )
}

/** Edits the named results the loop publishes when it succeeds. */
function LoopOutputsFields({
  data,
  onChange,
  choices,
}: LoopFieldsProps & { choices: readonly WorkflowDecoratedVariable[] }) {
  const { t } = useTranslation()
  const loopConfig = data.loopConfig
  if (loopConfig === undefined) {
    return null
  }
  const update = (outputs: WorkflowOutputBinding[]): void => {
    onChange({ ...data, loopConfig: { ...loopConfig, outputs } })
  }
  return (
    <InspectorField label={t('workflows.loop.field.outputs')} htmlFor="workflow-loop-outputs">
      <div className="space-y-2" id="workflow-loop-outputs">
        {/* oxlint-disable react/no-array-index-key -- a named output has no stable id; its name is the editable identity, so a content key collides on a blank new row. */}
        {loopConfig.outputs.map((output, index) => (
          // oxlint-disable-next-line react/no-array-index-key -- a named output has no stable id; its name is the editable identity, so a content key collides on a blank new row.
          <div key={`loop-output-${index}`} className="flex items-center gap-2">
            <Input
              aria-label={`${t('workflows.loop.field.outputName')} ${index + 1}`}
              value={output.name}
              onChange={(event) =>
                update(
                  loopConfig.outputs.map((current, i) =>
                    i === index ? { ...current, name: event.target.value } : current,
                  ),
                )
              }
            />
            <WorkflowCatalogSelector
              htmlId={`workflow-loop-output-selector-${index}`}
              value={output.variableSelector}
              placeholder={t('workflows.loop.selectorPlaceholder')}
              choices={choices}
              catalog={choices}
              ariaLabel={`${t('workflows.loop.field.outputSelector')} ${index + 1}`}
              onPick={(picked) =>
                update(
                  loopConfig.outputs.map((current, i) =>
                    i === index ? { ...current, variableSelector: picked } : current,
                  ),
                )
              }
            />
            <Button
              type="button"
              variant="ghost"
              size="icon"
              aria-label={`${t('workflows.loop.removeOutput')} ${index + 1}`}
              onClick={() => update(loopConfig.outputs.filter((_, i) => i !== index))}
            >
              <Trash2 />
            </Button>
          </div>
        ))}
      </div>
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => update([...loopConfig.outputs, { name: '', variableSelector: [] }])}
      >
        <Plus />
        {t('workflows.loop.addOutput')}
      </Button>
    </InspectorField>
  )
}

/** Every root a loop may read: outer ancestors, region member roots and the carried variables. */
function resolveLoopChoices(
  graphNodes: readonly WorkflowCanvasNode[],
  edges: readonly WorkflowCanvasEdge[],
  scope: {
    loopId: string
    globalVariables: readonly WorkflowGlobalVariable[]
    config?: WorkflowLoopConfig
  },
): WorkflowDecoratedVariable[] {
  const { loopId, globalVariables, config } = scope
  const normalized = normalizeWorkflowGlobalVariables(globalVariables)
  const visible = decorateWorkflowVariableCatalog(
    deriveWorkflowVariableCatalog(graphNodes, edges, loopId, normalized),
    graphNodes,
    normalized,
  )
  const everything = decorateWorkflowVariableCatalog(
    deriveWorkflowVariableCatalog(graphNodes, [], undefined, normalized),
    graphNodes,
    normalized,
  )
  const memberIDs = new Set(
    graphNodes
      .filter((node) => (node.data.containerId || node.parentId) === loopId)
      .map((node) => node.id),
  )
  const members = everything.filter((entry) => memberIDs.has(entry.sourceNodeId))
  const carried: WorkflowDecoratedVariable[] = (config?.variables ?? [])
    .filter((variable) => variable.name.trim() !== '')
    .map((variable) => ({
      selector: [loopId, variable.name],
      sourceNodeId: loopId,
      variableName: variable.name,
      valueType: variable.valueType,
      scope: 'node',
      sourceNodeTitle: 'Loop',
    }))
  const seen = new Set<string>()
  return [...visible, ...members, ...carried].filter((entry) => {
    const key = entry.selector.join('.')
    if (seen.has(key)) {
      return false
    }
    seen.add(key)
    return true
  })
}

/** Reinterprets a constant immediately when its authored type changes. */
function loopVariableWithType(
  variable: WorkflowLoopVariable,
  valueType: WorkflowLoopVariable['valueType'],
): WorkflowLoopVariable {
  const initial =
    variable.initial.kind === 'constant'
      ? {
          kind: 'constant' as const,
          value: loopConstantValue(comparisonValueToText(variable.initial.value), valueType),
        }
      : variable.initial
  return { ...variable, valueType, initial }
}
