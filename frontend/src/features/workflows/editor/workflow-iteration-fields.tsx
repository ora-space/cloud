import { useTranslation } from 'react-i18next'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { InspectorField } from '@/features/workflows/editor/workflow-inspector-fields'
import { DEFAULT_ITERATION_MAX_ITERATIONS } from '@/features/workflows/runtime/iteration-defaults'
import {
  decorateWorkflowVariableCatalog,
  deriveWorkflowVariableCatalog,
  normalizeWorkflowGlobalVariables,
} from '@/features/workflows/runtime/variable-catalog'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import type {
  WorkflowGlobalVariable,
  WorkflowIterationConfig,
  WorkflowNodeData,
} from '@/features/workflows/runtime/types'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'
import { WorkflowCatalogSelector } from '@/features/workflows/editor/workflow-variable-select'

/** Everything the iteration panel reads beyond the selected node itself. */
export interface WorkflowIterationFieldsProps {
  /** Id of the iteration node the panel edits; the root of the region-membership lookup. */
  nodeId: string
  data: WorkflowNodeData
  onChange: (data: WorkflowNodeData) => void
  /** Variables the iteration node may reference; only array-typed ones drive the rounds. */
  catalog: readonly WorkflowDecoratedVariable[]
  /** Whole-graph nodes, for the region-member collect lookup. */
  graphNodes: readonly WorkflowCanvasNode[]
  /** Workflow-wide declarations, normalized to include the required system globals. */
  globalVariables: readonly WorkflowGlobalVariable[]
}

/**
 * Edits one Iteration node's foreach configuration.
 *
 * The iterator names the array variable driving the rounds, the collect target names the
 * region member whose stable outputs a round produces, the error strategy chooses fail-fast
 * vs. collect-and-continue, and the safety ceiling bounds the rounds.
 */
export function IterationNodeFields({
  nodeId,
  data,
  onChange,
  catalog,
  graphNodes,
  globalVariables,
}: WorkflowIterationFieldsProps) {
  const { t } = useTranslation()
  const config: WorkflowIterationConfig = data.iterationConfig ?? {
    iteratorSelector: [],
    collectSelector: [],
    errorStrategy: 'fail',
    maxIterations: DEFAULT_ITERATION_MAX_ITERATIONS,
  }
  const updateConfig = (patch: Partial<WorkflowIterationConfig>): void => {
    onChange({ ...data, iterationConfig: { ...config, ...patch } })
  }
  // Iterator choices: every array-typed variable the node can reference, globals included.
  const iteratorChoices = catalog.filter((variable) => variable.valueType.startsWith('array'))
  const collectChoices = resolveCollectChoices(graphNodes, globalVariables, nodeId)
  return (
    <>
      <p className="text-xs leading-relaxed text-muted-foreground">
        {t('workflows.iteration.hint')}
      </p>
      <InspectorField
        label={t('workflows.iteration.field.iterator')}
        htmlFor="workflow-node-iteration-iterator"
      >
        <WorkflowCatalogSelector
          htmlId="workflow-node-iteration-iterator"
          value={config.iteratorSelector}
          placeholder={t('workflows.iteration.iteratorPlaceholder')}
          choices={iteratorChoices}
          catalog={catalog}
          onPick={(selector) => updateConfig({ iteratorSelector: selector })}
          ariaLabel={t('workflows.iteration.field.iterator')}
        />
        {config.iteratorSelector.length === 0 && (
          <p className="text-xs text-amber-600 dark:text-amber-400">
            {t('workflows.iteration.iteratorEmpty')}
          </p>
        )}
      </InspectorField>
      <InspectorField
        label={t('workflows.iteration.field.collect')}
        htmlFor="workflow-node-iteration-collect"
      >
        <WorkflowCatalogSelector
          htmlId="workflow-node-iteration-collect"
          value={config.collectSelector}
          placeholder={t('workflows.iteration.collectPlaceholder')}
          choices={collectChoices}
          catalog={catalog}
          onPick={(selector) => updateConfig({ collectSelector: selector })}
          ariaLabel={t('workflows.iteration.field.collect')}
        />
        {config.collectSelector.length === 0 && (
          <p className="text-xs text-amber-600 dark:text-amber-400">
            {t('workflows.iteration.collectEmpty')}
          </p>
        )}
      </InspectorField>
      <IterationLimitsFields config={config} onConfigChange={updateConfig} />
    </>
  )
}

/** The error strategy choice and the per-round safety ceiling. */
function IterationLimitsFields({
  config,
  onConfigChange,
}: {
  config: WorkflowIterationConfig
  onConfigChange: (patch: Partial<WorkflowIterationConfig>) => void
}) {
  const { t } = useTranslation()
  return (
    <>
      <InspectorField
        label={t('workflows.iteration.field.errorStrategy')}
        htmlFor="workflow-node-iteration-error-strategy"
      >
        <Select
          value={config.errorStrategy}
          onValueChange={(value) => {
            if (value === 'fail' || value === 'continue') {
              onConfigChange({ errorStrategy: value })
            }
          }}
        >
          <SelectTrigger
            className="h-8 w-full bg-background"
            id="workflow-node-iteration-error-strategy"
            aria-label={t('workflows.iteration.field.errorStrategy')}
          >
            <SelectValue>
              {config.errorStrategy === 'continue'
                ? t('workflows.iteration.continueStrategy')
                : t('workflows.iteration.failStrategy')}
            </SelectValue>
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="fail">{t('workflows.iteration.failStrategy')}</SelectItem>
            <SelectItem value="continue">{t('workflows.iteration.continueStrategy')}</SelectItem>
          </SelectContent>
        </Select>
      </InspectorField>
      <InspectorField
        label={t('workflows.loop.field.maxIterations')}
        htmlFor="workflow-node-iteration-max"
      >
        <Input
          id="workflow-node-iteration-max"
          type="number"
          min={1}
          max={100}
          value={config.maxIterations}
          onChange={(event) => {
            const parsed = Number(event.target.value)
            if (
              event.target.value !== '' &&
              Number.isFinite(parsed) &&
              parsed >= 1 &&
              parsed <= 100
            ) {
              onConfigChange({ maxIterations: Math.floor(parsed) })
            }
          }}
        />
      </InspectorField>
    </>
  )
}

/**
 * Resolves the collect target choices for an Iteration region.
 *
 * Only direct members may be collected: a node inside the region whose root selector is
 * `output` or `structured_output`. The derivation without a consumer enumerates every node,
 * so the member filter is the only narrowing that matters here.
 */
function resolveCollectChoices(
  graphNodes: readonly WorkflowCanvasNode[],
  globalVariables: readonly WorkflowGlobalVariable[],
  iterationNodeId: string,
): WorkflowDecoratedVariable[] {
  const memberIds = new Set(
    graphNodes
      .filter((candidate) => candidate.parentId === iterationNodeId)
      .map((candidate) => candidate.id),
  )
  const normalized = normalizeWorkflowGlobalVariables(globalVariables)
  return decorateWorkflowVariableCatalog(
    deriveWorkflowVariableCatalog(graphNodes, [], undefined, normalized),
    graphNodes,
    normalized,
  ).filter(
    (variable) =>
      memberIds.has(variable.sourceNodeId) &&
      variable.selector.length === 2 &&
      (variable.variableName === 'output' || variable.variableName === 'structured_output'),
  )
}
