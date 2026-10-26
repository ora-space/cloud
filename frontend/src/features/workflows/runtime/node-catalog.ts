import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** Editor scopes in which a node kind can be authored. */
export type WorkflowNodeScope = 'workflow' | 'iteration' | 'loop'

/** Inspector sections a node kind exposes. */
export type WorkflowConfigField =
  | 'agent'
  | 'initialPrompt'
  | 'tool'
  | 'condition'
  | 'aggregator'
  | 'approvalPrompt'
  | 'waitStrategy'
  | 'failureStrategy'
  | 'maxAttempts'
  | 'exitCondition'
  | 'maxIterations'
  | 'loopInitialValue'
  | 'loopBehavior'
  | 'iteration'

/**
 * Authoring metadata for one node kind: what it is called and what may be configured on it.
 *
 * The label and description are translation keys rather than resolved text, because a node's
 * stored title is resolved once at creation while the palette is re-rendered on every locale
 * change. Keeping them apart stops a language switch from renaming existing graphs.
 */
export interface WorkflowNodeTypeDefinition {
  kind: WorkflowNodeKind
  /** Translation key of the palette label. */
  labelKey: string
  /** Translation key of the palette description. */
  descriptionKey: string
  configFields: WorkflowConfigField[]
  supportedScopes: WorkflowNodeScope[]
}

/** Node kinds the editor offers in its palette, in menu order. */
export const WORKFLOW_NODE_PALETTE: readonly WorkflowNodeKind[] = [
  'start',
  'agent',
  'condition',
  'aggregator',
  'loop',
  'iteration',
  'output',
  // Offered only by a loop body's insert menu: the marker is meaningless outside a container.
  'loopExit',
]

/** Builds one definition, deriving the translation keys from the kind. */
function defineNodeType(
  kind: WorkflowNodeKind,
  configFields: WorkflowConfigField[],
  supportedScopes: WorkflowNodeScope[],
): WorkflowNodeTypeDefinition {
  return {
    kind,
    labelKey: `workflows.node.${kind}.label`,
    descriptionKey: `workflows.node.${kind}.description`,
    configFields,
    supportedScopes,
  }
}

/**
 * Authoring metadata for every node kind the execution contract understands.
 *
 * The record is keyed by the kind union, so adding a kind to `WORKFLOW_NODE_KINDS` without
 * describing it here is a type error rather than a node that renders blank.
 */
const NODE_TYPE_DEFINITIONS: Record<WorkflowNodeKind, WorkflowNodeTypeDefinition> = {
  start: defineNodeType('start', ['initialPrompt'], ['workflow']),
  agent: defineNodeType('agent', ['agent'], ['workflow', 'iteration']),
  condition: defineNodeType('condition', ['condition'], ['workflow', 'iteration']),
  aggregator: defineNodeType('aggregator', ['aggregator'], ['workflow', 'iteration']),
  tool: defineNodeType('tool', ['tool'], ['workflow']),
  junction: defineNodeType('junction', ['waitStrategy', 'failureStrategy'], ['workflow']),
  human: defineNodeType('human', ['approvalPrompt'], ['workflow']),
  loop: defineNodeType('loop', ['maxIterations', 'loopBehavior', 'loopInitialValue'], ['workflow']),
  loopExit: defineNodeType('loopExit', [], ['loop']),
  iteration: defineNodeType('iteration', ['iteration'], ['workflow']),
  subflow: defineNodeType('subflow', [], ['workflow']),
  output: defineNodeType('output', [], ['workflow']),
}

/** Returns the authoring metadata for one node kind. */
export function workflowNodeTypeDefinition(kind: WorkflowNodeKind): WorkflowNodeTypeDefinition {
  return NODE_TYPE_DEFINITIONS[kind]
}

/** Returns whether a node kind may be authored in the requested editor scope. */
export function supportsWorkflowNodeScope(
  kind: WorkflowNodeKind,
  scope: WorkflowNodeScope,
): boolean {
  return NODE_TYPE_DEFINITIONS[kind].supportedScopes.includes(scope)
}

/** Lists the palette entries available in one editor scope, in menu order. */
export function workflowPaletteNodeTypes(scope: WorkflowNodeScope): WorkflowNodeTypeDefinition[] {
  return WORKFLOW_NODE_PALETTE.filter((kind) => supportsWorkflowNodeScope(kind, scope)).map(
    workflowNodeTypeDefinition,
  )
}
