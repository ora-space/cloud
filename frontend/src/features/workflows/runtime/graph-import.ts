import { isEdge, isNode, type Node } from '@xyflow/react'
import {
  WorkflowDefinitionValidationError,
  normalizeWorkflowDocument,
  type WorkflowDefinitionInput,
  type WorkflowDefinitionInputEdge,
  type WorkflowDefinitionInputNode,
} from '@/features/workflows/runtime/definition'
import {
  isWorkflowGlobalVariable,
  isWorkflowGraphAnnotation,
  isWorkflowLaunchField,
  type WorkflowGraphAnnotation,
} from '@/features/workflows/runtime/graph-codec'
import { asJsonRecord } from '@/features/workflows/runtime/json-record'
import {
  isWorkflowNodeKind,
  type WorkflowAgentConfig,
  type WorkflowDefinition,
  type WorkflowGlobalVariable,
  type WorkflowLaunchField,
  type WorkflowNodeData,
  type WorkflowViewport,
} from '@/features/workflows/runtime/types'

/**
 * Raised when an imported document is not a workflow the editor can open.
 *
 * Distinct from `WorkflowDefinitionValidationError`: that one reports what is wrong with a
 * document the author built here, this one reports why a picked file cannot become one.
 */
export class WorkflowImportError extends Error {
  readonly issues: readonly string[]

  constructor(issues: readonly string[]) {
    super(`Invalid workflow document: ${issues.join('; ')}`)
    this.name = 'WorkflowImportError'
    this.issues = issues
  }
}

/** An imported document, split into the parts the editor stores separately. */
export interface ImportedWorkflowDocument {
  definition: WorkflowDefinition
  /** Editor notes, which live beside the definition rather than inside it. */
  annotations: WorkflowGraphAnnotation[]
  /**
   * The `@` form's launch-field declaration, which the definition does not carry: the executable
   * document is shared with the desktop runtime, and which fields the cloud form asks for is a
   * cloud-`@` concern. Leaving it out of the definition is what keeps an export round-tripping.
   */
  launchFields: WorkflowLaunchField[]
}

/** One element that either narrowed cleanly or is the reason the import is refused. */
type Imported<T> = { ok: true; value: T } | { ok: false; issue: string }

/** The viewport an exported document is assumed to have when it carries none. */
const DEFAULT_IMPORT_VIEWPORT: WorkflowViewport = { x: 0, y: 0, zoom: 1 }

/**
 * Validates an untrusted document and turns it into an editor definition.
 *
 * The persisted-graph codec is deliberately forgiving — it drops what it cannot read so a graph
 * written by a newer editor still opens. An imported file gets the opposite treatment: silently
 * discarding half of one would leave the author editing a graph that is not the one they picked,
 * so anything unrecognized is reported instead.
 *
 * The authoring contract applies, not the executable one: a graph with a cycle among nodes the
 * author has not wired up yet imports fine and is refused at publish, exactly as a hand-built one
 * is. Refusing it here would block importing a file that is merely unfinished.
 */
export function parseImportedWorkflowDocument(value: unknown): ImportedWorkflowDocument {
  const document = asJsonRecord(value)
  if (document === undefined) {
    throw new WorkflowImportError(['document must be an object'])
  }
  const nodes = readImportedNodes(document['nodes'])
  const edges = readImportedEdges(document['edges'])
  const viewport = readImportedViewport(document['viewport'])
  const annotations = readImportedAnnotations(document['annotations'])
  const globalVariables = readImportedGlobalVariables(document['globalVariables'])
  const launchFields = readImportedLaunchFields(document['launchFields'])
  const issues = [
    ...readDocumentIssues(document),
    ...nodes.issues,
    ...edges.issues,
    ...viewport.issues,
    ...annotations.issues,
    ...globalVariables.issues,
    ...launchFields.issues,
  ]
  if (issues.length > 0) {
    throw new WorkflowImportError(issues)
  }
  // The element shapes are proven above, so the identity, geometry, and acyclicity rules the
  // editor already enforces are the only ones left to apply.
  try {
    return {
      definition: normalizeWorkflowDocument(
        asDefinitionInput(document, {
          nodes: nodes.values,
          edges: edges.values,
          viewport: viewport.value,
          globalVariables: globalVariables.values,
        }),
      ),
      annotations: annotations.values,
      launchFields: launchFields.values,
    }
  } catch (error) {
    throw error instanceof WorkflowDefinitionValidationError
      ? new WorkflowImportError(error.issues)
      : error
  }
}

/** Requires the two fields a workflow cannot be created without. */
function readDocumentIssues(document: Record<string, unknown>): string[] {
  const issues: string[] = []
  if (!isNonEmptyString(document['id'])) {
    issues.push('document must carry an id')
  }
  if (!isNonEmptyString(document['name'])) {
    issues.push('document must carry a name')
  }
  return issues
}

/** The geometry and workflow variables an import recovered from the file. */
interface ImportedParts {
  nodes: WorkflowDefinitionInputNode[]
  edges: WorkflowDefinitionInputEdge[]
  viewport: WorkflowViewport
  globalVariables: WorkflowGlobalVariable[]
}

/** Assembles the definition input, defaulting the metadata an export may leave out. */
function asDefinitionInput(
  document: Record<string, unknown>,
  parts: ImportedParts,
): WorkflowDefinitionInput {
  const { nodes, edges, viewport, globalVariables } = parts
  return {
    id: readString(document['id']),
    name: readString(document['name']),
    description: readString(document['description']),
    updatedAt: isNonEmptyString(document['updatedAt'])
      ? document['updatedAt']
      : new Date().toISOString(),
    viewport,
    nodes,
    edges,
    ...(globalVariables.length === 0 ? {} : { globalVariables }),
  }
}

/** Narrows every imported workflow variable, reporting each one it had to refuse. */
function readImportedGlobalVariables(value: unknown): {
  values: WorkflowGlobalVariable[]
  issues: string[]
} {
  if (value === undefined) {
    return { values: [], issues: [] }
  }
  if (!Array.isArray(value)) {
    return { values: [], issues: ['globalVariables must be an array'] }
  }
  const values: WorkflowGlobalVariable[] = []
  const issues: string[] = []
  for (const [index, variable] of value.entries()) {
    if (isWorkflowGlobalVariable(variable)) {
      values.push(variable)
    } else {
      issues.push(`global variable ${index} is not a valid workflow variable`)
    }
  }
  return { values, issues }
}

/** Narrows every imported launch-field declaration entry, reporting each one it had to refuse. */
function readImportedLaunchFields(value: unknown): {
  values: WorkflowLaunchField[]
  issues: string[]
} {
  if (value === undefined) {
    return { values: [], issues: [] }
  }
  if (!Array.isArray(value)) {
    return { values: [], issues: ['launchFields must be an array'] }
  }
  const values: WorkflowLaunchField[] = []
  const issues: string[] = []
  for (const [index, field] of value.entries()) {
    if (isWorkflowLaunchField(field)) {
      values.push(field)
    } else {
      issues.push(`launch field ${index} does not name a platform launch field`)
    }
  }
  return { values, issues }
}

/** Reads a string field, treating anything else as absent. */
function readString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

/** True for a string that names something. */
function isNonEmptyString(value: unknown): value is string {
  return typeof value === 'string' && value.trim() !== ''
}

/** Narrows every imported node, reporting each one it had to refuse. */
function readImportedNodes(value: unknown): {
  values: WorkflowDefinitionInputNode[]
  issues: string[]
} {
  if (!Array.isArray(value)) {
    return { values: [], issues: ['nodes must be an array'] }
  }
  return partition(value, readImportedNode)
}

/** Narrows every imported edge, reporting each one it had to refuse. */
function readImportedEdges(value: unknown): {
  values: WorkflowDefinitionInputEdge[]
  issues: string[]
} {
  if (!Array.isArray(value)) {
    return { values: [], issues: ['edges must be an array'] }
  }
  return partition(value, readImportedEdge)
}

/** Narrows every imported note, reporting each one it had to refuse. */
function readImportedAnnotations(value: unknown): {
  values: WorkflowGraphAnnotation[]
  issues: string[]
} {
  if (value === undefined) {
    return { values: [], issues: [] }
  }
  if (!Array.isArray(value)) {
    return { values: [], issues: ['annotations must be an array'] }
  }
  const values: WorkflowGraphAnnotation[] = []
  const issues: string[] = []
  for (const [index, annotation] of value.entries()) {
    if (isWorkflowGraphAnnotation(annotation)) {
      values.push(annotation)
    } else {
      issues.push(`annotation ${index} is not a valid editor note`)
    }
  }
  return { values, issues }
}

/** Runs one reader over a list, keeping the accepted values and the reported issues apart. */
function partition<T, U>(
  values: readonly T[],
  read: (value: T, index: number) => Imported<U>,
): { values: U[]; issues: string[] } {
  const accepted: U[] = []
  const issues: string[] = []
  for (const [index, value] of values.entries()) {
    const result = read(value, index)
    if (result.ok) {
      accepted.push(result.value)
    } else {
      issues.push(result.issue)
    }
  }
  return { values: accepted, issues }
}

/** Narrows one imported node to the authoring shape, or reports why it is not one. */
function readImportedNode(value: unknown, index: number): Imported<WorkflowDefinitionInputNode> {
  const at = `node ${index}`
  if (!isNode(value) || value.type !== 'workflow') {
    return { ok: false, issue: `${at} must be a workflow node` }
  }
  if (value.id.trim() === '') {
    return { ok: false, issue: `${at} has an empty id` }
  }
  if (!Number.isFinite(value.position.x) || !Number.isFinite(value.position.y)) {
    return { ok: false, issue: `${at} has a non-finite position` }
  }
  if (!isImportedNodeData(value.data)) {
    return { ok: false, issue: `${at} is missing a supported kind, a title, or an agent config` }
  }
  if (value.data.kind === 'start' && value.deletable !== false) {
    return { ok: false, issue: `${at} must be a non-deletable start node` }
  }
  return { ok: true, value: toDefinitionInputNode(value, value.data) }
}

/** Copies one proven node into the definition input the editor normalizes. */
function toDefinitionInputNode(node: Node, data: WorkflowNodeData): WorkflowDefinitionInputNode {
  const input: WorkflowDefinitionInputNode = {
    id: node.id,
    position: { x: node.position.x, y: node.position.y },
    data,
  }
  if (typeof node.parentId === 'string') input.parentId = node.parentId
  if (typeof node.initialWidth === 'number' && Number.isFinite(node.initialWidth))
    input.initialWidth = node.initialWidth
  if (typeof node.initialHeight === 'number' && Number.isFinite(node.initialHeight))
    input.initialHeight = node.initialHeight
  if (node.type !== undefined) {
    input.type = node.type
  }
  if (node.deletable !== undefined) {
    input.deletable = node.deletable
  }
  return input
}

/** Narrows one imported edge to the authoring shape, or reports why it is not one. */
function readImportedEdge(value: unknown, index: number): Imported<WorkflowDefinitionInputEdge> {
  const at = `edge ${index}`
  if (!isEdge(value) || value.type !== 'workflow') {
    return { ok: false, issue: `${at} must be a workflow edge` }
  }
  if (value.id.trim() === '' || value.source === '' || value.target === '') {
    return { ok: false, issue: `${at} must name an id, a source, and a target` }
  }
  if (!isOptionalHandle(value.sourceHandle) || !isOptionalHandle(value.targetHandle)) {
    return { ok: false, issue: `${at} has a handle that is neither absent nor a string` }
  }
  const edge: WorkflowDefinitionInputEdge = {
    id: value.id,
    source: value.source,
    target: value.target,
    type: 'workflow',
  }
  if (value.label !== undefined) {
    edge.label = value.label
  }
  if (value.data !== undefined) {
    edge.data = value.data
  }
  if (typeof value.sourceHandle === 'string') {
    edge.sourceHandle = value.sourceHandle
  }
  if (typeof value.targetHandle === 'string') {
    edge.targetHandle = value.targetHandle
  }
  return { ok: true, value: edge }
}

/** Accepts a handle that was never set, which React Flow reports as null. */
function isOptionalHandle(handle: unknown): boolean {
  return handle === undefined || handle === null || typeof handle === 'string'
}

/** Narrows the imported viewport, defaulting one an export never wrote. */
function readImportedViewport(value: unknown): { value: WorkflowViewport; issues: string[] } {
  if (value === undefined) {
    return { value: DEFAULT_IMPORT_VIEWPORT, issues: [] }
  }
  return isImportedViewport(value)
    ? { value, issues: [] }
    : {
        value: DEFAULT_IMPORT_VIEWPORT,
        issues: ['viewport must contain finite coordinates and a positive zoom'],
      }
}

/** True for a viewport the canvas can restore. */
function isImportedViewport(value: unknown): value is WorkflowViewport {
  const viewport = asJsonRecord(value)
  return (
    viewport !== undefined &&
    typeof viewport['x'] === 'number' &&
    Number.isFinite(viewport['x']) &&
    typeof viewport['y'] === 'number' &&
    Number.isFinite(viewport['y']) &&
    typeof viewport['zoom'] === 'number' &&
    Number.isFinite(viewport['zoom']) &&
    viewport['zoom'] > 0
  )
}

/**
 * Narrows imported node data to the authoring payload the editor renders and the engine runs.
 *
 * A node that declares an executor but not a role, or binds the same skill twice, is refused here
 * rather than at run time, where the failure would surface as a stalled execution instead of a
 * rejected file.
 */
function isImportedNodeData(value: Record<string, unknown>): value is WorkflowNodeData {
  const kind = value['kind']
  return (
    isWorkflowNodeKind(kind) &&
    typeof value['title'] === 'string' &&
    typeof value['description'] === 'string' &&
    (kind !== 'agent' || isWorkflowAgentConfig(value['agentConfig']))
  )
}

/** Validates the serialized Agent contract before a definition can be edited or run. */
export function isWorkflowAgentConfig(value: unknown): value is WorkflowAgentConfig {
  const config = asJsonRecord(value)
  if (config === undefined) {
    return false
  }
  const executor = asJsonRecord(config['executor'])
  return (
    config['schemaVersion'] === 3 &&
    executor !== undefined &&
    isNonEmptyString(executor['agentCli']) &&
    isNonEmptyString(executor['modelId']) &&
    isNonEmptyString(config['roleId']) &&
    typeof config['prompt'] === 'string' &&
    isEnabledBindingList(config['skills'], 'skillId') &&
    isEnabledBindingList(config['mcps'], 'mcpId')
  )
}

/** True for a list of uniquely-named enablement bindings, i.e. skills or MCP attachments. */
function isEnabledBindingList(value: unknown, idKey: string): boolean {
  if (!Array.isArray(value)) {
    return false
  }
  const ids: string[] = []
  for (const entry of value) {
    const binding = asJsonRecord(entry)
    if (binding === undefined) {
      return false
    }
    const id = binding[idKey]
    if (!isNonEmptyString(id) || typeof binding['enabled'] !== 'boolean') {
      return false
    }
    ids.push(id)
  }
  return new Set(ids).size === ids.length
}
