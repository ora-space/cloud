import { useCallback, useMemo, useRef, useState } from 'react'
import {
  applyEdgeChanges,
  applyNodeChanges,
  type Connection,
  type EdgeChange,
  type NodeChange,
  type XYPosition,
} from '@xyflow/react'
import type { Workflow as CloudWorkflow } from '@/api/generated.schemas'
import { resolveConditionCases } from '@/features/workflows/runtime/condition-cases'
import {
  parseWorkflowGraphValue,
  serializeWorkflowGraphValue,
  type WorkflowGraphAnnotation,
} from '@/features/workflows/runtime/graph-codec'
import {
  isValidWorkflowConnection,
  workflowEdgeId,
} from '@/features/workflows/runtime/connection-validation'
import { WORKFLOW_FLOW_EDGE_TYPE, organizeWorkflowNodes } from '@/features/workflows/runtime/layout'
import {
  applyIterationFrameResize,
  insertIterationMember,
  repairIterationGraphAfterNodeDeletion,
} from '@/features/workflows/runtime/iteration-graph'
import {
  insertLoopMember,
  loopExitInsertion,
  type WorkflowContainerInsertion,
} from '@/features/workflows/runtime/loop-graph'
import {
  createWorkflowLoopGroup,
  createWorkflowNode,
  nextWorkflowNodeSequence,
  type WorkflowNodeTranslator,
} from '@/features/workflows/runtime/node-factory'
import type {
  WorkflowDefinitionEdge,
  WorkflowDefinitionNode,
  WorkflowGlobalVariable,
  WorkflowLaunchField,
  WorkflowNodeData,
  WorkflowNodeKind,
  WorkflowViewport,
} from '@/features/workflows/runtime/types'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'
import type { WorkflowHistorySnapshot } from '@/features/workflows/editor/workflow-history'

/**
 * Everything the canvas can change.
 *
 * The parts the editor cannot edit yet are carried through unchanged rather than
 * dropped, so opening and saving a graph written by a later release preserves
 * its notes and global variables instead of quietly deleting them.
 */
interface DraftState {
  nodes: WorkflowCanvasNode[]
  edges: WorkflowCanvasEdge[]
  viewport: WorkflowViewport
  annotations: readonly WorkflowGraphAnnotation[]
  globalVariables: readonly WorkflowGlobalVariable[]
  launchFields: readonly WorkflowLaunchField[]
  description?: string
}

/** The live graph plus everything the canvas calls to change it. */
export interface WorkflowDraft {
  nodes: WorkflowCanvasNode[]
  edges: WorkflowCanvasEdge[]
  /** Workflow-wide declarations, reseeded together with the graph. */
  globalVariables: readonly WorkflowGlobalVariable[]
  /** Which platform launch fields the `@` form asks for, reseeded with the graph. */
  launchFields: readonly WorkflowLaunchField[]
  /** Viewport the draft was seeded with, used until the canvas reports a new one. */
  viewport: WorkflowViewport
  /** The single selected node, or `null` when the selection is empty or multiple. */
  selectedNode: WorkflowCanvasNode | null
  applyNodeChanges: (changes: NodeChange<WorkflowCanvasNode>[]) => void
  applyEdgeChanges: (changes: EdgeChange<WorkflowCanvasEdge>[]) => void
  /** Appends a new node of `kind`, with the next free ordinal for that kind. */
  addNode: (kind: WorkflowNodeKind, position: XYPosition) => void
  /** Adds a loopExit into the given loop's body. */
  addLoopExit: (loopId: string) => void
  /** Adds one container member through an iteration or loop-body insertion seam. */
  addIterationMember: (insertion: WorkflowContainerInsertion, kind: WorkflowNodeKind) => void
  /** Folds or unfolds one iteration frame; members stay in the graph. */
  toggleIterationCollapsed: (iterationId: string) => void
  /** Replaces the graph nodes after an iteration drag correction. */
  applyCorrection: (nodes: WorkflowCanvasNode[]) => void
  /** Adds an edge, unless the gesture would create a self-loop or a duplicate. */
  connect: (connection: Connection) => void
  /** Replaces one node's data with an edited copy. */
  updateNodeData: (nodeId: string, data: WorkflowNodeData) => void
  /** Replaces the workflow-wide declarations, e.g. after the variables dialog saves. */
  replaceGlobalVariables: (variables: WorkflowGlobalVariable[]) => void
  /** Replaces the launch-field declaration, e.g. after the launch-fields dialog saves. */
  replaceLaunchFields: (fields: WorkflowLaunchField[]) => void
  /** Re-arranges the graph into execution order. */
  organize: () => void
  /** Replaces the whole draft from a fresh workflow record, e.g. after reloading the document. */
  reset: (workflow: CloudWorkflow) => void
  /**
   * Snapshots the current authored content for a history step. The viewport is
   * left out, and React Flow's measurement and interface state is stripped, so
   * a selection that makes no content difference cannot become a history step.
   */
  captureContent: () => WorkflowHistorySnapshot
  /**
   * Replaces the authored content from a history snapshot. Runs synchronously,
   * so undo can read back the pre-restore state immediately after it.
   */
  restoreContent: (snapshot: WorkflowHistorySnapshot) => void
  /**
   * Serializes the draft into the graph document the API stores.
   *
   * @param viewport - Viewport to record, taken from the canvas rather than state.
   * @returns The document to send as the `graph` body field.
   */
  snapshot: (viewport: WorkflowViewport) => Record<string, unknown>
}

/**
 * Holds the graph being edited.
 *
 * Seeded once from the loaded workflow and owned entirely by React state: the
 * server copy is a starting point, not a source of truth, so a refetch triggered
 * by a save cannot overwrite edits made while that save was in flight. Remounting
 * (a `key` on the workflow id) is what switches drafts, which keeps the reset
 * out of an effect that would have to compare ids.
 *
 * Every transition below delegates to a module-scope reducer, so the hook's
 * body stays a thin binding of callbacks to `setState` and the mutation rules
 * stay plain functions a test could drive without React.
 *
 * @param workflow - The workflow whose graph seeds the draft.
 * @param translate - Resolves the label and description a new node starts with.
 * @returns The draft state and its change handlers.
 */
// oxlint-disable-next-line max-lines-per-function -- the draft's whole mutation surface; splitting it would thread its state mirror through a second hook.
export function useWorkflowDraft(
  workflow: CloudWorkflow,
  translate: WorkflowNodeTranslator,
): WorkflowDraft {
  const [state, setState] = useState<DraftState>(() => seedDraft(workflow))
  // History captures after the mutator, so the mirror must update synchronously.
  const stateRef = useRef<DraftState>(state)
  /** Commits a new draft state, keeping the sync mirror and the render in step. */
  const replaceState = useCallback((next: DraftState): void => {
    stateRef.current = next
    setState(next)
  }, [])

  const applyNodeEdits = (changes: NodeChange<WorkflowCanvasNode>[]): void => {
    replaceState(reduceNodeChanges(stateRef.current, changes))
  }
  const applyEdgeEdits = (changes: EdgeChange<WorkflowCanvasEdge>[]): void => {
    replaceState({
      ...stateRef.current,
      edges: applyEdgeChanges(changes, stateRef.current.edges),
    })
  }
  const addNode = (kind: WorkflowNodeKind, position: XYPosition): void => {
    replaceState(appendNode(stateRef.current, kind, position, translate))
  }
  const addLoopExit = (loopId: string): void => {
    replaceState(appendLoopExitNode(stateRef.current, loopId, translate))
  }
  const connect = (connection: Connection): void => {
    replaceState(appendConnection(stateRef.current, connection))
  }
  const addIterationMember = (
    insertion: WorkflowContainerInsertion,
    kind: WorkflowNodeKind,
  ): void => {
    replaceState(insertMember(stateRef.current, insertion, kind, translate))
  }
  const toggleIterationCollapsed = (iterationId: string): void => {
    replaceState(toggleCollapsed(stateRef.current, iterationId))
  }
  const applyCorrection = (nodes: WorkflowCanvasNode[]): void => {
    // The correction source is the canvas's presented copy, which carries
    // presentation-only fields (extent, hidden, zIndex, member count); stripping
    // them keeps the draft as the clean persisted document the canvas rebuilds.
    replaceState({ ...stateRef.current, nodes: nodes.map(toDraftNode) })
  }
  const updateNodeData = (nodeId: string, data: WorkflowNodeData): void => {
    replaceState(setNodeData(stateRef.current, nodeId, data))
  }
  const replaceGlobalVariables = (variables: WorkflowGlobalVariable[]): void => {
    replaceState(setWorkflowGlobalVariables(stateRef.current, variables))
  }
  const replaceLaunchFields = (fields: WorkflowLaunchField[]): void => {
    replaceState(setWorkflowLaunchFields(stateRef.current, fields))
  }
  const organize = (): void => {
    replaceState({
      ...stateRef.current,
      nodes: organizeWorkflowNodes(stateRef.current.nodes, stateRef.current.edges),
    })
  }

  const reset = (next: CloudWorkflow): void => {
    replaceState(seedDraft(next))
  }

  const captureContent = useCallback(
    (): WorkflowHistorySnapshot => captureWorkflowHistorySnapshot(stateRef.current),
    [],
  )

  const restoreContent = useCallback(
    (snapshot: WorkflowHistorySnapshot) => {
      replaceState(restoreSnapshotContent(snapshot, stateRef.current.viewport))
    },
    [replaceState],
  )

  const snapshot = useCallback(
    (viewport: WorkflowViewport) => serializeDraft(state, viewport),
    [state],
  )

  const selectedNode = useMemo(
    () => state.nodes.find((node) => node.selected === true) ?? null,
    [state.nodes],
  )

  return {
    nodes: state.nodes,
    edges: state.edges,
    globalVariables: state.globalVariables,
    launchFields: state.launchFields,
    viewport: state.viewport,
    selectedNode,
    applyNodeChanges: applyNodeEdits,
    applyEdgeChanges: applyEdgeEdits,
    addNode,
    addLoopExit,
    addIterationMember,
    toggleIterationCollapsed,
    applyCorrection,
    connect,
    updateNodeData,
    replaceGlobalVariables,
    replaceLaunchFields,
    organize,
    reset,
    captureContent,
    restoreContent,
    snapshot,
  }
}

/**
 * Applies a node-change batch to the draft.
 *
 * A manual frame resize arrives as a `dimensions` change with `resizing` set and
 * must be mirrored onto the authored frame size atomically; a deletion then needs
 * its collect-selector repairs applied in the same update, so both run here rather
 * than in the change source.
 */
function reduceNodeChanges(
  current: DraftState,
  changes: NodeChange<WorkflowCanvasNode>[],
): DraftState {
  const applied = applyNodeChanges(changes, current.nodes)
  const resized = applyIterationFrameResize(applied, changes)
  const removedIds = new Set(
    changes
      .filter(
        (change): change is Extract<NodeChange<WorkflowCanvasNode>, { type: 'remove' }> =>
          change.type === 'remove',
      )
      .map((change) => change.id),
  )
  if (removedIds.size === 0) {
    // The resize transform is generic over the node type, so the canvas nodes
    // come back unchanged in shape.
    return { ...current, nodes: resized }
  }
  const repair = repairIterationGraphAfterNodeDeletion(
    { nodes: resized, edges: current.edges },
    removedIds,
  )
  return {
    ...current,
    nodes: repair.graph.nodes,
    edges: repair.graph.edges,
  }
}

/** Appends a new node of `kind` at the position, on the next free ordinal. */
function appendNode(
  current: DraftState,
  kind: WorkflowNodeKind,
  position: XYPosition,
  translate: WorkflowNodeTranslator,
): DraftState {
  const sequence = nextWorkflowNodeSequence(
    kind,
    current.nodes.map((node) => node.id),
  )
  if (kind === 'loop') {
    // A Loop without a body is a saveable but unrunnable container; the factory emits the body
    // start, a starter agent and the round contract together so the palette entry is executable.
    const group = createWorkflowLoopGroup({ kind, sequence, position, translate })
    return {
      ...current,
      nodes: [...current.nodes, ...group.nodes],
      edges: [...current.edges, ...group.edges],
    }
  }
  const node = createWorkflowNode({ kind, sequence, position, translate })
  return { ...current, nodes: [...current.nodes, node] }
}

/**
 * Appends one loopExit into `loopId`'s body, after its right-most member.
 *
 * The inspector action goes through the same seam as a member port's "+", so the exit is
 * wired, placed clear of the round and grows the Loop instead of landing on a fixed point.
 */
function appendLoopExitNode(
  current: DraftState,
  loopId: string,
  translate: WorkflowNodeTranslator,
): DraftState {
  const insertion = loopExitInsertion(current, loopId)
  return insertion === null ? current : insertMember(current, insertion, 'loopExit', translate)
}

/** Adds an edge, unless the gesture would create a self-loop or a duplicate. */
function appendConnection(current: DraftState, connection: Connection): DraftState {
  const sourceNode = current.nodes.find((node) => node.id === connection.source)
  const sourceHandle =
    connection.sourceHandle || (sourceNode?.data.kind === 'condition' ? 'else' : null)
  const candidate = { ...connection, sourceHandle }
  // An older unlabeled Condition edge means ELSE. Canonicalize that source's existing edges so
  // reconnecting its explicit ELSE port cannot create a second edge for the same branch.
  const edges = current.edges.map((edge) =>
    sourceNode?.data.kind === 'condition' && edge.source === connection.source
      ? { ...edge, sourceHandle: edge.sourceHandle || 'else' }
      : edge,
  )
  if (!isValidWorkflowConnection(candidate, { nodes: current.nodes, edges })) {
    return current
  }
  const { source, target, targetHandle } = candidate
  return {
    ...current,
    edges: [
      ...edges,
      {
        id: workflowEdgeId(source, target, candidate),
        source,
        target,
        ...(sourceHandle === null ? {} : { sourceHandle }),
        ...(targetHandle === null ? {} : { targetHandle }),
        type: WORKFLOW_FLOW_EDGE_TYPE,
      },
    ],
  }
}

/** Adds one container member through an explicit iteration or loop-body seam. */
function insertMember(
  current: DraftState,
  insertion: WorkflowContainerInsertion,
  kind: WorkflowNodeKind,
  translate: WorkflowNodeTranslator,
): DraftState {
  const containerId = insertion.type === 'loop-output' ? insertion.loopId : insertion.iterationId
  if (!current.nodes.some((node) => node.id === containerId)) {
    return current
  }
  const sequence = nextWorkflowNodeSequence(
    kind,
    current.nodes.map((node) => node.id),
  )
  // The transform places the splice point itself; a placeholder position is
  // enough because insertionPosition overwrites it from the seam geometry.
  const node = createWorkflowNode({
    kind,
    sequence,
    position: { x: 0, y: 207 },
    translate,
  })
  return insertion.type === 'loop-output'
    ? insertLoopMember(current, insertion, node)
    : insertIterationMember(current, insertion, node)
}

/** Folds or unfolds one iteration frame; members stay in the graph. */
function toggleCollapsed(current: DraftState, iterationId: string): DraftState {
  return {
    ...current,
    nodes: current.nodes.map((node) =>
      node.id === iterationId && node.data.kind === 'iteration'
        ? { ...node, data: { ...node.data, collapsed: node.data.collapsed !== true } }
        : node,
    ),
  }
}

/** Replaces one node's data with an edited copy. */
function setNodeData(current: DraftState, nodeId: string, data: WorkflowNodeData): DraftState {
  const previous = current.nodes.find((node) => node.id === nodeId)
  const removedBranches = new Set<string>()
  if (previous?.data.kind === 'condition' && data.kind === 'condition') {
    const remaining = new Set(resolveConditionCases(data).map((branch) => branch.id))
    for (const branch of resolveConditionCases(previous.data)) {
      if (!remaining.has(branch.id)) {
        removedBranches.add(branch.id)
      }
    }
  }
  return {
    ...current,
    nodes: current.nodes.map((node) => (node.id === nodeId ? { ...node, data } : node)),
    edges: current.edges.filter(
      (edge) => edge.source !== nodeId || !removedBranches.has(edge.sourceHandle ?? 'else'),
    ),
  }
}

/**
 * Replaces the workflow-wide declaration list.
 *
 * Rows are copied per variable so the graph document never aliases the dialog's
 * draft array, exactly as the other setters detach their inputs.
 */
function setWorkflowGlobalVariables(
  current: DraftState,
  variables: readonly WorkflowGlobalVariable[],
): DraftState {
  return { ...current, globalVariables: variables.map((variable) => ({ ...variable })) }
}

/** Replaces the launch-field declaration, detaching it from the dialog's draft array. */
function setWorkflowLaunchFields(
  current: DraftState,
  fields: readonly WorkflowLaunchField[],
): DraftState {
  return { ...current, launchFields: fields.map((field) => ({ ...field })) }
}

/** Serializes the draft into the graph document the API stores. */
function serializeDraft(state: DraftState, viewport: WorkflowViewport): Record<string, unknown> {
  return serializeWorkflowGraphValue({
    nodes: state.nodes.map(toDefinitionNode),
    edges: state.edges.map(toDefinitionEdge),
    viewport,
    annotations: state.annotations,
    globalVariables: state.globalVariables,
    launchFields: state.launchFields,
    ...(state.description === undefined ? {} : { description: state.description }),
  })
}

/** Reads a stored graph document into the draft's initial state. */
function seedDraft(workflow: CloudWorkflow): DraftState {
  const graph = parseWorkflowGraphValue(workflow.graph)
  return {
    nodes: graph.nodes.map((node) => ({ ...node, data: { ...node.data } })),
    edges: graph.edges.map((edge) => ({ ...edge, type: WORKFLOW_FLOW_EDGE_TYPE })),
    viewport: graph.viewport,
    annotations: graph.annotations,
    globalVariables: graph.globalVariables,
    launchFields: graph.launchFields,
    ...(graph.description === undefined ? {} : { description: graph.description }),
  }
}

/**
 * Copies a canvas node into its stored shape.
 *
 * Every field is named explicitly: React Flow writes measurement and interaction
 * state (`measured`, `selected`, `dragging`, `width`) onto the nodes it manages,
 * and a spread would persist that runtime state into the graph document.
 */
function toDefinitionNode(node: WorkflowCanvasNode): WorkflowDefinitionNode {
  return {
    id: node.id,
    type: 'workflow',
    position: { x: node.position.x, y: node.position.y },
    data: stripRegionMemberCount(node.data),
    ...(node.parentId === undefined ? {} : { parentId: node.parentId }),
    ...(node.deletable === undefined ? {} : { deletable: node.deletable }),
    ...(node.initialWidth === undefined ? {} : { initialWidth: node.initialWidth }),
    ...(node.initialHeight === undefined ? {} : { initialHeight: node.initialHeight }),
  }
}

/** Rebuilds a canvas node into the clean document shape the draft owns. */
function toDraftNode(node: WorkflowCanvasNode): WorkflowCanvasNode {
  const next: WorkflowCanvasNode = { ...node }
  delete next.extent
  delete next.expandParent
  delete next.hidden
  delete next.zIndex
  delete next.selected
  delete next.dragging
  if (node.data.regionMemberCount !== undefined) {
    next.data = stripRegionMemberCount(node.data)
  }
  return next
}

/** Drops the presentation-derived member count from authored node data. */
function stripRegionMemberCount(data: WorkflowNodeData): WorkflowNodeData {
  if (data.regionMemberCount === undefined) {
    return data
  }
  const next = { ...data }
  delete next.regionMemberCount
  return next
}

/**
 * Snapshot of the draft's authored content for a history step.
 *
 * The viewport is session presentation and stays out. Nodes lose every field
 * React Flow writes for measurement and interaction (`measured`, `width`,
 * `selected`, `dragging`, region chrome) so a selection or a render-time
 * measurement cannot split two otherwise identical snapshots; the authored frame
 * size (`initialWidth`/`initialHeight`) is kept because resizing writes through it.
 */
function captureWorkflowHistorySnapshot(state: DraftState): WorkflowHistorySnapshot {
  return {
    nodes: state.nodes.map(captureHistoryNode),
    edges: state.edges.map((edge) => ({ ...edge, selected: false })),
    annotations: state.annotations.map((annotation) => ({ ...annotation })),
    globalVariables: state.globalVariables.map((variable) => ({ ...variable })),
    launchFields: state.launchFields.map((field) => ({ ...field })),
    ...(state.description === undefined ? {} : { description: state.description }),
  }
}

/** Copies a canvas node into the minimal shape a history snapshot needs. */
function captureHistoryNode(node: WorkflowCanvasNode): WorkflowCanvasNode {
  const next: WorkflowCanvasNode = toDraftNode(node)
  delete next.measured
  delete next.width
  delete next.height
  return next
}

/**
 * Replaces the draft content from a history snapshot, keeping the current
 * viewport. Nodes come back without the selection flags their snapshot carried,
 * so restoring never leaves a node highlighted against what the user sees.
 */
function restoreSnapshotContent(
  snapshot: WorkflowHistorySnapshot,
  viewport: WorkflowViewport,
): DraftState {
  return {
    nodes: snapshot.nodes.map((node) => ({ ...node, data: { ...node.data } })),
    edges: snapshot.edges.map((edge) => ({ ...edge })),
    viewport,
    annotations: snapshot.annotations,
    globalVariables: snapshot.globalVariables,
    launchFields: snapshot.launchFields,
    ...(snapshot.description === undefined ? {} : { description: snapshot.description }),
  }
}

/** Copies a canvas edge into its stored shape, dropping React Flow's own fields. */
function toDefinitionEdge(edge: WorkflowCanvasEdge): WorkflowDefinitionEdge {
  return {
    id: edge.id,
    source: edge.source,
    target: edge.target,
    type: WORKFLOW_FLOW_EDGE_TYPE,
    ...(edge.sourceHandle === null || edge.sourceHandle === undefined
      ? {}
      : { sourceHandle: edge.sourceHandle }),
    ...(edge.targetHandle === null || edge.targetHandle === undefined
      ? {}
      : { targetHandle: edge.targetHandle }),
  }
}
