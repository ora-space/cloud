import { isJsonRecord } from '@/features/workflows/runtime/json-record'
import { isWorkflowVariableValueType } from '@/features/workflows/runtime/variable-value'
import type {
  WorkflowGlobalVariable,
  WorkflowIterationConfig,
  WorkflowNodeData,
  WorkflowNodeKind,
  WorkflowVariableValueType,
} from '@/features/workflows/runtime/types'

/**
 * The variable pool a downstream node may select from.
 *
 * The catalog is derived from the graph rather than stored, because what a node can reference
 * depends on what actually reaches it — so the answer changes whenever the graph does. Only
 * ancestors contribute, and an Iteration region applies its own scope rules on top, which is why a
 * variable that plainly exists in the graph can still be invisible to a given consumer.
 */

/** Built-in globals available in every workflow; the runtime fills their values at run creation. */
export const DEFAULT_WORKFLOW_GLOBAL_VARIABLES: WorkflowGlobalVariable[] = [
  { name: 'sys.workflow_id', valueType: 'string' },
  { name: 'sys.timestamp', valueType: 'number' },
]

/** One variable a downstream node can select, addressed by its full selector path. */
export interface WorkflowVariableCatalogEntry {
  /** Dify-style selector `[nodeId, root, ...nestedPath]`, or the parts of a dotted global name. */
  selector: string[]
  sourceNodeId: string
  /** Presentation-only source metadata, filled in by editors that know node titles. */
  sourceNodeTitle?: string
  /** Distinguishes workflow-wide declarations from values a node produces. */
  scope?: 'global' | 'node'
  variableName: string
  valueType: WorkflowVariableValueType
}

/**
 * The node fields the catalog reads.
 *
 * A structural subset of React Flow's `Node`, so this module stays free of a React Flow import:
 * the editor passes its real nodes in, and a test can pass plain objects.
 */
export interface WorkflowCatalogNode {
  id: string
  parentId?: string
  data: WorkflowNodeData
}

/** The edge fields the catalog reads. */
export interface WorkflowCatalogEdge {
  source: string
  target: string
}

/** Restores required system globals while preserving user-defined declarations. */
export function normalizeWorkflowGlobalVariables(
  variables: readonly WorkflowGlobalVariable[] | undefined,
): WorkflowGlobalVariable[] {
  const byName = new Map((variables ?? []).map((variable) => [variable.name, variable]))
  for (const required of DEFAULT_WORKFLOW_GLOBAL_VARIABLES) {
    byName.set(required.name, required)
  }
  return [...byName.values()]
}

/**
 * Derives every variable the consumer at `consumerNodeId` may select.
 *
 * @param nodes - Graph nodes, including any the consumer cannot see.
 * @param edges - Graph edges, which decide what counts as upstream.
 * @param consumerNodeId - Node being configured; every node is visible when omitted.
 * @param globalVariables - Workflow-wide declarations; the built-in system globals when omitted.
 * @returns The selectable variables, globals first, in graph order within each source.
 */
export function deriveWorkflowVariableCatalog(
  nodes: readonly WorkflowCatalogNode[],
  edges: readonly WorkflowCatalogEdge[],
  consumerNodeId?: string,
  globalVariables: readonly WorkflowGlobalVariable[] = DEFAULT_WORKFLOW_GLOBAL_VARIABLES,
): WorkflowVariableCatalogEntry[] {
  const scope = buildCatalogScope(nodes, edges, consumerNodeId, globalVariables)
  const entries = globalVariables.flatMap(globalVariableCatalogEntry)
  for (const node of nodes) {
    entries.push(...nodeCatalogEntries(node, scope))
  }
  return entries
}

/** A catalog entry carrying the producing node's identity, which only an editor can supply. */
export interface WorkflowDecoratedVariable extends WorkflowVariableCatalogEntry {
  scope: 'global' | 'node'
  sourceNodeTitle?: string
  sourceNodeKind?: WorkflowNodeKind
}

/**
 * Adds the display metadata a menu needs to the derivation's output.
 *
 * The derivation deliberately knows nothing about node titles or which entries are globals — it
 * answers what a node *may* reference, which is a graph question. Naming the source is a
 * presentation question, and it is answered here so the Condition and Iteration panels cannot
 * answer it differently.
 *
 * @param entries - Catalog entries to decorate.
 * @param nodes - Graph nodes the entries came from, for their titles and kinds.
 * @param globalVariables - Declarations that mark an entry as workflow-wide.
 * @returns The same entries, each naming its source.
 */
export function decorateWorkflowVariableCatalog(
  entries: readonly WorkflowVariableCatalogEntry[],
  nodes: readonly WorkflowCatalogNode[],
  globalVariables: readonly WorkflowGlobalVariable[],
): WorkflowDecoratedVariable[] {
  const globalSelectors = new Set(globalVariables.map((variable) => variable.name))
  const nodeById = new Map(nodes.map((node) => [node.id, node.data]))
  return entries.map((entry) => {
    const source = nodeById.get(entry.sourceNodeId)
    return {
      ...entry,
      scope: globalSelectors.has(entry.selector.join('.')) ? 'global' : 'node',
      ...(source === undefined
        ? {}
        : { sourceNodeTitle: source.title, sourceNodeKind: source.kind }),
    }
  })
}

/** Everything the per-node rules are judged against, derived once per derivation. */
interface WorkflowCatalogScope {
  nodes: readonly WorkflowCatalogNode[]
  globalVariables: readonly WorkflowGlobalVariable[]
  consumerNodeId: string | undefined
  /** Ancestors of the consumer, plus every node when there is no consumer. */
  visibleProducerIds: ReadonlySet<string>
  /** Owning Iteration of a node, or `null` when it is not a region member. */
  owningContainerOf: (nodeId: string) => string | null
}

/** Derives the scope one catalog derivation runs against. */
function buildCatalogScope(
  nodes: readonly WorkflowCatalogNode[],
  edges: readonly WorkflowCatalogEdge[],
  consumerNodeId: string | undefined,
  globalVariables: readonly WorkflowGlobalVariable[],
): WorkflowCatalogScope {
  const iterationIds = new Set(
    nodes
      .filter((node) => node.data.kind === 'iteration' || node.data.kind === 'loop')
      .map((node) => node.id),
  )
  const parentByNodeId = new Map(
    nodes.map((node) => [node.id, node.data.containerId || node.parentId] as const),
  )
  const owner = parentByNodeId.get(consumerNodeId ?? '')
  const visible =
    consumerNodeId === undefined
      ? new Set(nodes.map((node) => node.id))
      : collectVisibleProducerIds(nodes, edges, consumerNodeId)
  if (owner !== undefined && iterationIds.has(owner)) {
    for (const id of collectVisibleProducerIds(nodes, edges, owner)) visible.add(id)
  }
  return {
    nodes,
    globalVariables,
    consumerNodeId,
    visibleProducerIds: visible,
    owningContainerOf: (nodeId) => {
      const parent = parentByNodeId.get(nodeId)
      return parent !== undefined && iterationIds.has(parent) ? parent : null
    },
  }
}

/**
 * Whether one node's products reach the consumer.
 *
 * Three rules, in order: a node never contributes to itself; only ancestors contribute; and a
 * region member's products stay region-visible, so a node outside the region never sees them and
 * neither does a sibling in a different region. An author who wants a region's work visible
 * outside it has to collect it, which is what the collect target is for.
 */
function contributesTo(node: WorkflowCatalogNode, scope: WorkflowCatalogScope): boolean {
  if (!scope.visibleProducerIds.has(node.id) || node.id === scope.consumerNodeId) {
    return false
  }
  const producerOwner = scope.owningContainerOf(node.id)
  if (producerOwner === null || scope.consumerNodeId === undefined) {
    return true
  }
  return producerOwner === scope.owningContainerOf(scope.consumerNodeId)
}

/** The catalog entries one node contributes, if any. */
function nodeCatalogEntries(
  node: WorkflowCatalogNode,
  scope: WorkflowCatalogScope,
): WorkflowVariableCatalogEntry[] {
  if (node.data.kind === 'start') {
    return contributesTo(node, scope) ? startCatalogEntries(node) : []
  }
  if (node.data.kind === 'iteration') {
    return iterationCatalogEntries(node, scope)
  }
  if (node.data.kind === 'loop') {
    const member =
      scope.consumerNodeId !== undefined &&
      scope.owningContainerOf(scope.consumerNodeId) === node.id
    if (member)
      return (node.data.loopConfig?.variables ?? []).map((v) =>
        nodeVariable(node, v.name, v.valueType),
      )
    return contributesTo(node, scope)
      ? (node.data.loopConfig?.outputs ?? []).map((v) => nodeVariable(node, v.name, 'any'))
      : []
  }
  if (!contributesTo(node, scope)) {
    return []
  }
  // A Condition only routes control flow. Exposing its internal branch decision would let a
  // downstream prompt depend on scheduler state as if it were business data.
  const stable =
    node.data.kind === 'condition' || node.data.kind === 'loopExit'
      ? []
      : [nodeVariable(node, 'output', 'string')]
  return [...stable, ...structuredOutputCatalogEntries(node)]
}

/** Start contributes its kickoff prompt plus every input variable it declares. */
function startCatalogEntries(node: WorkflowCatalogNode): WorkflowVariableCatalogEntry[] {
  // `input` is the initial prompt value. It is always declared, even before a run supplies text,
  // so downstream configuration can reference the stable selector.
  return [
    nodeVariable(node, 'input', 'string'),
    ...(node.data.inputVariables ?? [])
      .filter((variable) => variable.name.trim() !== '')
      .map((variable) => nodeVariable(node, variable.name.trim(), variable.valueType)),
  ]
}

/**
 * Iteration contributes round bindings to its members and exposed results to everyone else.
 *
 * A member sees `item` and `index`, resolved per round; an outer consumer sees the three exposed
 * variables whose types stay fixed across error strategies. The two sets are deliberately
 * disjoint, so an outer node can never depend on which round a value came from.
 */
function iterationCatalogEntries(
  node: WorkflowCatalogNode,
  scope: WorkflowCatalogScope,
): WorkflowVariableCatalogEntry[] {
  const isMember =
    scope.consumerNodeId !== undefined &&
    scope.nodes.some(
      (candidate) =>
        (candidate.data.containerId || candidate.parentId) === node.id &&
        candidate.id === scope.consumerNodeId,
    )
  const config = node.data.iterationConfig
  if (isMember) {
    const iteratorType =
      config === undefined ? 'any' : resolveDeclaredVariableType(config.iteratorSelector, scope)
    return [
      nodeVariable(node, 'item', arrayElementType(iteratorType)),
      nodeVariable(node, 'index', 'number'),
    ]
  }
  if (config === undefined || !contributesTo(node, scope)) {
    return []
  }
  const collectType = resolveCollectType(config, scope)
  return [
    nodeVariable(node, 'output', arrayValueType(collectType)),
    nodeVariable(node, 'entries', 'array[object]'),
    nodeVariable(node, 'failed_count', 'number'),
  ]
}

/** Adds the structured-output leaf paths an Agent node declares. */
function structuredOutputCatalogEntries(node: WorkflowCatalogNode): WorkflowVariableCatalogEntry[] {
  const contract = node.data.agentConfig?.outputContract
  if (contract?.type !== 'structured') {
    return []
  }
  const entries = [nodeVariable(node, 'structured_output', 'object')]
  appendStructuredProperties(entries, node, contract.schema, [])
  return entries
}

/**
 * Adds selectable leaf paths from the supported object-schema subset.
 *
 * A nested object contributes both itself and its own leaves, so an author can pass the whole
 * object on or reach one field of it. Anything the subset does not describe is skipped rather
 * than guessed at, because a selector that resolves to nothing fails the run, not the save.
 */
function appendStructuredProperties(
  entries: WorkflowVariableCatalogEntry[],
  node: WorkflowCatalogNode,
  schema: Record<string, unknown>,
  path: string[],
): void {
  const properties = schema['properties']
  if (!isJsonRecord(properties)) {
    return
  }
  for (const [name, property] of Object.entries(properties)) {
    if (!isJsonRecord(property)) {
      continue
    }
    const nextPath = [...path, name]
    const valueType = schemaValueType(property)
    entries.push({
      ...nodeVariable(node, 'structured_output', valueType),
      selector: [node.id, 'structured_output', ...nextPath],
      variableName: `structured_output.${nextPath.join('.')}`,
    })
    if (valueType === 'object') {
      appendStructuredProperties(entries, node, property, nextPath)
    }
  }
}

/** Maps a JSON Schema primitive name onto the workflow variable type set. */
function schemaValueType(schema: Record<string, unknown>): WorkflowVariableValueType {
  const declared = schema['type']
  if (declared !== 'array') {
    // `isWorkflowVariableValueType` also accepts the `array[...]` spellings, so the array case has
    // to be handled first or an element type declared in `items` would be dropped.
    return isWorkflowVariableValueType(declared) ? declared : 'any'
  }
  const items = schema['items']
  if (!isJsonRecord(items)) {
    return 'array'
  }
  const element = items['type']
  return element === 'string' ||
    element === 'number' ||
    element === 'object' ||
    element === 'boolean' ||
    element === 'file' ||
    element === 'any'
    ? `array[${element}]`
    : 'array'
}

/** Strips the element type from an array variable type; untyped arrays yield `any`. */
function arrayElementType(valueType: string): WorkflowVariableValueType {
  const element = /^array\[(.+)\]$/.exec(valueType)?.[1]
  return element !== undefined && element !== 'any' && isWorkflowVariableValueType(element)
    ? element
    : 'any'
}

/** Resolves a selector against its complete owner path so same-named declarations cannot collide. */
function resolveDeclaredVariableType(
  selector: readonly string[],
  scope: WorkflowCatalogScope,
): string {
  const global = scope.globalVariables.find((variable) => variable.name === selector.join('.'))
  if (global !== undefined) {
    return global.valueType
  }
  // Only a root variable has a declared type; a nested path's type lives in the schema, not here.
  if (selector.length !== 2) {
    return 'any'
  }
  const [nodeId, variableName] = selector
  if (nodeId === undefined || variableName === undefined) {
    return 'any'
  }
  const owner = scope.nodes.find((node) => node.id === nodeId)
  if (owner?.data.kind === 'start') {
    return (
      owner.data.inputVariables?.find((variable) => variable.name === variableName)?.valueType ??
      'any'
    )
  }
  if (owner?.data.kind === 'iteration') {
    if (variableName === 'output') {
      return 'array[any]'
    }
    if (variableName === 'entries') {
      return 'array[object]'
    }
  }
  return 'any'
}

/** Resolves the declared type of the collect target's root variable, defaulting to `any`. */
function resolveCollectType(config: WorkflowIterationConfig, scope: WorkflowCatalogScope): string {
  const [collectNodeId, collectVariable] = config.collectSelector
  if (collectNodeId === undefined || collectVariable === undefined) {
    return 'any'
  }
  // The target node has to exist before its shape can be trusted: a selector pointing at a
  // deleted node is broken config, so its element type cannot be known either.
  const target = scope.nodes.find((node) => node.id === collectNodeId)
  if (target === undefined) {
    return 'any'
  }
  if (collectVariable === 'structured_output') {
    return 'object'
  }
  if (collectVariable === 'output') {
    return 'string'
  }
  if (target.data.kind === 'start') {
    return (
      target.data.inputVariables?.find((variable) => variable.name === collectVariable)
        ?.valueType ?? 'any'
    )
  }
  return 'any'
}

/** Wraps an element type in the `array[...]` spelling, guarded to the declared union. */
function arrayValueType(elementType: string): WorkflowVariableValueType {
  const spelling = `array[${elementType}]`
  return isWorkflowVariableValueType(spelling) ? spelling : 'any'
}

/**
 * Collects every upstream ancestor, remaining finite for temporarily cyclic edit graphs.
 *
 * A cycle is authorable — the canvas lets a wire be drawn before validation rejects it — so the
 * walk carries its own visited set rather than trusting the graph to be acyclic.
 */
function collectVisibleProducerIds(
  nodes: readonly WorkflowCatalogNode[],
  edges: readonly WorkflowCatalogEdge[],
  consumerNodeId: string,
): Set<string> {
  const knownIds = new Set(nodes.map((node) => node.id))
  const incomingByTarget = new Map<string, string[]>()
  for (const edge of edges) {
    incomingByTarget.set(edge.target, [...(incomingByTarget.get(edge.target) ?? []), edge.source])
  }
  const producers = new Set<string>()
  const visited = new Set<string>()
  const pending = [...(incomingByTarget.get(consumerNodeId) ?? [])]
  while (pending.length > 0) {
    const nodeId = pending.pop()
    if (nodeId === undefined || visited.has(nodeId)) {
      continue
    }
    visited.add(nodeId)
    if (!knownIds.has(nodeId)) {
      continue
    }
    producers.add(nodeId)
    pending.push(...(incomingByTarget.get(nodeId) ?? []))
  }
  return producers
}

/** Converts one qualified global declaration into a selectable catalog entry. */
function globalVariableCatalogEntry(
  variable: WorkflowGlobalVariable,
): WorkflowVariableCatalogEntry[] {
  const parts = variable.name.split('.').filter((part) => part !== '')
  const [head, ...rest] = parts
  if (head === undefined || rest.length === 0) {
    return []
  }
  return [
    {
      selector: parts,
      sourceNodeId: head,
      variableName: rest.join('.'),
      valueType: variable.valueType,
    },
  ]
}

/** Builds a catalog entry owned by a workflow node. */
function nodeVariable(
  node: WorkflowCatalogNode,
  variableName: string,
  valueType: WorkflowVariableValueType,
): WorkflowVariableCatalogEntry {
  return {
    selector: [node.id, variableName],
    sourceNodeId: node.id,
    variableName,
    valueType,
  }
}
