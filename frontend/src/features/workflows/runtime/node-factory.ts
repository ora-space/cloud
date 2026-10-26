import { createDefaultWorkflowCapabilities } from '@/features/workflows/runtime/capabilities'
import {
  WORKFLOW_LOOP_NODE_HEIGHT,
  WORKFLOW_LOOP_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { DEFAULT_ITERATION_MAX_ITERATIONS } from '@/features/workflows/runtime/iteration-defaults'
import type {
  WorkflowAgentConfig,
  WorkflowDefinitionEdge,
  WorkflowDefinitionNode,
  WorkflowNodeData,
  WorkflowNodeKind,
  WorkflowPosition,
} from '@/features/workflows/runtime/types'

/** Resolves a translation key to the text of the active locale. */
export type WorkflowNodeTranslator = (key: string) => string

/** Everything a new node needs beyond its kind. */
export interface WorkflowNodeSeed {
  kind: WorkflowNodeKind
  /** 1-based ordinal within the graph; the generated id and title both use it. */
  sequence: number
  position: WorkflowPosition
  /** Resolves the palette label and description in the active locale. */
  translate: WorkflowNodeTranslator
  /** Execution contract a new Agent node starts from; the catalog default when omitted. */
  agentConfig?: WorkflowAgentConfig
}

/**
 * The node kinds the editor offers, matching the palette.
 *
 * React Flow can render every kind in the execution contract, but only these six are
 * authorable from the palette; the rest (tool, junction, human, subflow) arrive by
 * import or by migration, and the inspector still supports them.
 */
export const AUTHORABLE_NODE_KINDS: readonly WorkflowNodeKind[] = [
  'start',
  'agent',
  'condition',
  'aggregator',
  'loop',
  'iteration',
  'output',
]

/** Builds the readable id of the `sequence`-th node of one kind. */
export function workflowNodeId(kind: WorkflowNodeKind, sequence: number): string {
  return `${kind}-${sequence}`
}

/**
 * Finds the lowest ordinal whose id is still free.
 *
 * Ordinals are reused after a delete so ids stay short, and an id is never
 * handed out twice — React Flow keys the canvas by it, and the graph document
 * stores it as the edge endpoint.
 *
 * @param kind - Node kind the id is generated for.
 * @param existingIds - Ids already present in the graph.
 * @returns The first positive ordinal whose id is unused.
 */
export function nextWorkflowNodeSequence(
  kind: WorkflowNodeKind,
  existingIds: Iterable<string>,
): number {
  const taken = new Set(existingIds)
  let sequence = 1
  while (taken.has(workflowNodeId(kind, sequence))) {
    sequence += 1
  }
  return sequence
}

/**
 * Builds the configuration a fresh node of one kind starts from.
 *
 * An authorable kind returns a complete seed: Condition starts with one empty IF
 * branch so its output handles are stable before rules exist, and Iteration starts
 * with the safety ceiling every run must enforce. A kind the editor cannot create
 * throws instead of producing a node that looks finished on the canvas but carries
 * no executable configuration.
 *
 * @param kind - Node kind being created.
 * @param translate - Active-locale resolver for the condition's descriptive label.
 * @param agentConfig - Execution contract to clone for a new Agent node.
 * @returns The `data` fields the kind contributes beyond title and description.
 */
function seedNodeData(
  kind: WorkflowNodeKind,
  translate: WorkflowNodeTranslator,
  agentConfig?: WorkflowAgentConfig,
): Partial<WorkflowNodeData> {
  switch (kind) {
    case 'start':
      return { input: '' }
    case 'output':
    case 'loop':
    case 'loopExit':
      // The Loop's own `loopConfig` is supplied by the atomic group factory, which creates
      // its starter children at the same time. The early-exit marker carries no data at all.
      return {}
    case 'aggregator':
      // An empty list is a legal draft but not publishable: the author picks candidates next.
      return { aggregatorConfig: { variables: [] } }
    case 'condition':
      return {
        condition: translate('workflows.node.condition.label'),
        cases: [{ id: 'case-1', logic: 'and', conditions: [] }],
      }
    case 'iteration':
      return {
        iterationConfig: {
          iteratorSelector: [],
          collectSelector: [],
          errorStrategy: 'fail',
          maxIterations: DEFAULT_ITERATION_MAX_ITERATIONS,
        },
      }
    case 'agent': {
      const defaults = createDefaultWorkflowCapabilities().defaultAgentConfig
      return { agentConfig: structuredClone(agentConfig ?? defaults) }
    }
    default:
      throw new Error(`Cannot create a workflow node of kind "${kind}" yet`)
  }
}

/**
 * Creates a palette entry as a graph node carrying business data in `data`.
 *
 * The title and description are resolved once, here, from the active locale and
 * then stored. They are authored text from that moment on: switching language
 * later must not rename a graph the user already wrote.
 *
 * @param seed - Kind, ordinal, position, translator and optional agent contract.
 * @returns A persisted-shape node, ready to add to the canvas.
 */
export function createWorkflowNode(seed: WorkflowNodeSeed): WorkflowDefinitionNode {
  const definition = workflowNodeTypeDefinition(seed.kind)
  const label = seed.translate(definition.labelKey)
  return {
    id: workflowNodeId(seed.kind, seed.sequence),
    type: 'workflow',
    // The Start node is the run's entry point: deleting it would leave a graph
    // that can be saved but never executed, so React Flow hides its delete key.
    ...(seed.kind === 'start' ? { deletable: false } : {}),
    position: { ...seed.position },
    data: {
      kind: seed.kind,
      title: `${label} ${seed.sequence}`,
      description: seed.translate(definition.descriptionKey),
      ...seedNodeData(seed.kind, seed.translate, seed.agentConfig),
    },
  }
}

/** Nodes and the internal edge created atomically for one executable Loop container. */
export interface WorkflowLoopGroup {
  nodes: WorkflowDefinitionNode[]
  edges: WorkflowDefinitionEdge[]
}

/**
 * Creates a working Loop with one child Start and one child Agent.
 *
 * Keeping the complete group in the factory guarantees every editor entry point
 * emits the same container ownership, first-round feedback, and exit condition —
 * a Loop with a body that references nothing is valid but useless, so the starter
 * body is always present and always references `loop-N.value`.
 *
 * @param seed - Kind, position, and translator for the container; the child Start
 * takes its id from the parent, the child Agent clones `agentConfig`.
 * @returns The container, its two children, and the edge joining them, in order.
 */
export function createWorkflowLoopGroup(seed: WorkflowNodeSeed): WorkflowLoopGroup {
  const loop = createWorkflowNode({
    ...seed,
    kind: 'loop',
    position: { ...seed.position },
    translate: seed.translate,
  })
  const childStartId = `${loop.id}-start`
  const childAgentId = `${loop.id}-agent`
  const childStart = createWorkflowNode({
    ...seed,
    kind: 'start',
    position: { x: 40, y: 145 },
    translate: seed.translate,
  })
  childStart.id = childStartId
  childStart.parentId = loop.id
  childStart.data = {
    ...childStart.data,
    title: seed.translate('workflows.loop.roundStart'),
    containerId: loop.id,
  }
  // The round's first probe reads the carried variable; without it the Agent runs blind
  // before any feedback has produced a value, so the template is baked into the very
  // contract the child node starts from.
  const childAgentContract = structuredClone(
    seed.agentConfig ?? createDefaultWorkflowCapabilities().defaultAgentConfig,
  )
  childAgentContract.prompt = seed
    .translate('workflows.loop.promptTemplate')
    .replace('NODE_ID', loop.id)
  const childAgent = createWorkflowNode({
    ...seed,
    agentConfig: childAgentContract,
    kind: 'agent',
    position: { x: 350, y: 145 },
    translate: seed.translate,
  })
  childAgent.id = childAgentId
  childAgent.parentId = loop.id
  childAgent.data = {
    ...childAgent.data,
    title: seed.translate('workflows.loop.loopAgent'),
    containerId: loop.id,
  }
  loop.initialWidth = WORKFLOW_LOOP_NODE_WIDTH
  loop.initialHeight = WORKFLOW_LOOP_NODE_HEIGHT
  loop.data = {
    ...loop.data,
    loopConfig: {
      maxIterations: 3,
      variables: [
        {
          name: 'value',
          valueType: 'string',
          initial: { kind: 'constant', value: '' },
          feedback: [childAgentId, 'output'],
        },
      ],
      until: {
        logic: 'and',
        conditions: [
          {
            variableSelector: [childAgentId, 'output'],
            operator: 'not_empty',
          },
        ],
      },
      outputs: [
        {
          name: 'result',
          variableSelector: [childAgentId, 'output'],
        },
      ],
    },
  }
  return {
    nodes: [loop, childStart, childAgent],
    edges: [
      {
        id: `e-${childStartId}-${childAgentId}`,
        source: childStartId,
        target: childAgentId,
        type: 'workflow',
      },
    ],
  }
}
