import {
  WORKFLOW_CONDITION_NODE_WIDTH,
  WORKFLOW_ITERATION_MEMBER_LEFT,
  WORKFLOW_ITERATION_MEMBER_TOP,
  WORKFLOW_ITERATION_NODE_HEIGHT,
  WORKFLOW_ITERATION_NODE_WIDTH,
  WORKFLOW_NODE_INITIAL_HEIGHT,
  WORKFLOW_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import type {
  WorkflowAgentConfig,
  WorkflowIterationConfig,
  WorkflowNodeKind,
} from '@/features/workflows/runtime/types'

/**
 * Iteration-region graph transforms, free of any canvas-library import.
 *
 * The region rules are pure: membership comes from `parentId` (the persisted document),
 * never from geometry, and every transform returns a graph the persistence layer can
 * round-trip. This module declares only the node fields it reads, so an editor passes its
 * real canvas nodes in and a test passes plain objects.
 */

/**
 * The node fields the iteration-region transforms read.
 *
 * The transforms never write the fields they declare; callers hand in canvas
 * nodes (or plain test objects) structurally and cast the transformed lists
 * back out across the boundary, so this stays the loose subset it is rather
 * than growing the canvas library's full node shape.
 */
export interface WorkflowIterationNode {
  id: string
  position: { x: number; y: number }
  data: {
    kind: WorkflowNodeKind
    agentConfig?: WorkflowAgentConfig
    containerId?: string
    iterationConfig?: WorkflowIterationConfig
    /** Presentation-only: the frame folds to the collapsed badge over its region. */
    collapsed?: boolean
  }
  parentId?: string
  initialWidth?: number
  initialHeight?: number
  width?: number
  height?: number
  measured?: { width?: number; height?: number }
  extent?: unknown
  expandParent?: unknown
}

/** The edge fields the iteration-region transforms read. */
export interface WorkflowIterationEdge {
  id: string
  source: string
  target: string
  type?: string | undefined
  /** Passed through untouched; the canvas edge may carry a rich label. */
  label?: unknown
  sourceHandle?: string | null | undefined
  targetHandle?: string | null | undefined
  hidden?: boolean | undefined
}

/** A graph the iteration transforms operate over; edits return the same concrete shape. */
export interface WorkflowIterationGraph {
  nodes: WorkflowIterationNode[]
  edges: WorkflowIterationEdge[]
}

/** Where a new member is spliced into the region, relative to the author's selection. */
export type WorkflowIterationInsertion =
  | { type: 'entry'; iterationId: string }
  | { type: 'edge'; iterationId: string; edgeId: string }
  | {
      type: 'output'
      iterationId: string
      sourceId: string
      sourceHandle?: string | null
    }

/** One member deletion, plus every region whose collect target the deletion orphaned. */
export interface WorkflowIterationDeletionRepair<TGraph extends WorkflowIterationGraph> {
  graph: TGraph
  clearedCollectSelectorIterationIds: string[]
}

/** Everything that must vanish when the author deletes an iteration container. */
export interface WorkflowIterationDeletionCascade {
  nodeIds: Set<string>
  edgeIds: Set<string>
  memberCount: number
}

/**
 * One canvas node-change the frame-resize transform inspects.
 *
 * A structural subset of the canvas library's `NodeChange`, so the editor can pass its
 * real change stream straight in and a test can pass plain objects; only `dimensions`
 * changes with `resizing` set are mirrored onto the authored frame size.
 */
export interface WorkflowIterationFrameChange {
  type: string
  id?: string
  resizing?: boolean
  dimensions?: { width: number; height: number }
  position?: { x: number; y: number }
  setAttributes?: unknown
}

const ITERATION_MEMBER_COLUMN_GAP = 100
const ITERATION_MEMBER_ROW_GAP = 52
const ITERATION_FRAME_RIGHT_PADDING = 48
const ITERATION_FRAME_BOTTOM_PADDING = 40

/**
 * Hides edges owned by folded regions without changing the persisted graph.
 *
 * Generic over the graph type so the editor passes its canvas edges in and gets
 * the same concrete shape back: the transform only reads node parents and marks
 * edges hidden, never changing what the draft owns.
 */
export function projectIterationEdges<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  collapsedIterationIds: ReadonlySet<string>,
): TGraph['edges'] {
  if (collapsedIterationIds.size === 0) {
    return graph.edges
  }
  const parentByNodeId = new Map(graph.nodes.map((node) => [node.id, node.parentId] as const))
  return graph.edges.map((edge) => {
    const sourceParent = parentByNodeId.get(edge.source)
    const targetParent = parentByNodeId.get(edge.target)
    return (sourceParent !== undefined && collapsedIterationIds.has(sourceParent)) ||
      (targetParent !== undefined && collapsedIterationIds.has(targetParent))
      ? { ...edge, hidden: true }
      : edge
  })
}

/** Inserts one iteration member and rewires the selected insertion point atomically. */
export function insertIterationMember<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  insertion: WorkflowIterationInsertion,
  inputNode: WorkflowIterationNode,
): TGraph {
  const iteration = graph.nodes.find(
    (node) => node.id === insertion.iterationId && node.data.kind === 'iteration',
  )
  if (iteration === undefined) {
    return graph
  }
  const node = prepareIterationMember(
    inputNode,
    insertion.iterationId,
    insertionPosition(graph, insertion, inputNode),
  )
  const occupiedIds = [
    ...graph.nodes.map((candidate) => candidate.id),
    ...graph.edges.map((edge) => edge.id),
    node.id,
  ]
  let edges: WorkflowIterationEdge[]
  switch (insertion.type) {
    case 'entry':
      edges = [
        ...graph.edges,
        {
          id: uniqueGraphId('edge', occupiedIds),
          source: insertion.iterationId,
          sourceHandle: 'iteration-entry',
          target: node.id,
          type: 'workflow',
        },
      ]
      break
    case 'output':
      edges = [
        ...graph.edges,
        {
          id: uniqueGraphId('edge', occupiedIds),
          source: insertion.sourceId,
          ...(insertion.sourceHandle == null ? {} : { sourceHandle: insertion.sourceHandle }),
          target: node.id,
          type: 'workflow',
        },
      ]
      break
    case 'edge': {
      const edge = graph.edges.find((candidate) => candidate.id === insertion.edgeId)
      if (edge === undefined) {
        return graph
      }
      const { targetHandle, ...sourceEdge } = edge
      edges = graph.edges.flatMap((candidate) =>
        candidate.id === edge.id
          ? [
              { ...sourceEdge, target: node.id },
              {
                id: uniqueGraphId('edge', occupiedIds),
                source: node.id,
                target: edge.target,
                ...(edge.type === undefined ? {} : { type: edge.type }),
                ...(targetHandle == null ? {} : { targetHandle }),
              },
            ]
          : [candidate],
      )
      break
    }
  }
  return resizeIterationFrames(
    { ...graph, nodes: [...graph.nodes, node], edges },
    [insertion.iterationId],
    'expand',
  )
}

/** Expands the selected frames to contain every member without shrinking authored space. */
export function expandIterationFrames<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  iterationIds?: readonly string[],
): TGraph {
  return resizeIterationFrames(graph, iterationIds, 'expand')
}

/** Recomputes the smallest valid frame around each selected region. */
export function compactIterationFrames<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  iterationIds?: readonly string[],
): TGraph {
  return resizeIterationFrames(graph, iterationIds, 'compact')
}

/** Clears iteration collect selectors whose owning member was deleted, then compacts frames. */
export function repairIterationGraphAfterNodeDeletion<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  removedNodeIds: ReadonlySet<string>,
): WorkflowIterationDeletionRepair<TGraph> {
  const clearedCollectSelectorIterationIds: string[] = []
  const nodes = graph.nodes.map((node) => {
    if (node.data.kind !== 'iteration') {
      return node
    }
    const config = node.data.iterationConfig
    const collectedNodeId = config?.collectSelector[0]
    if (
      config === undefined ||
      collectedNodeId === undefined ||
      !removedNodeIds.has(collectedNodeId)
    ) {
      return node
    }
    clearedCollectSelectorIterationIds.push(node.id)
    return {
      ...node,
      data: {
        ...node.data,
        iterationConfig: { ...config, collectSelector: [] },
      },
    }
  })
  return {
    graph: compactIterationFrames({ ...graph, nodes }),
    clearedCollectSelectorIterationIds,
  }
}

/**
 * Resolves members and incident edges that must join a container deletion.
 *
 * Both container kinds take their whole body along — Loop members are owned through either
 * key, so a member written with only `parentId` (an older import) is not left floating — but
 * only iteration members count toward `memberCount`, the number the deletion confirmation
 * quotes; a Loop always has a body, and its deletion is one undoable step instead.
 */
export function resolveIterationDeletionCascade(
  graph: WorkflowIterationGraph,
  requestedNodeIds: ReadonlySet<string>,
): WorkflowIterationDeletionCascade {
  const containerKinds = new Map(
    graph.nodes
      .filter(
        (node) =>
          requestedNodeIds.has(node.id) &&
          (node.data.kind === 'iteration' || node.data.kind === 'loop'),
      )
      .map((node) => [node.id, node.data.kind] as const),
  )
  const nodeIds = new Set(requestedNodeIds)
  let memberCount = 0
  for (const node of graph.nodes) {
    const ownerKind = node.parentId === undefined ? undefined : containerKinds.get(node.parentId)
    if (ownerKind !== undefined) {
      nodeIds.add(node.id)
      memberCount += ownerKind === 'iteration' ? 1 : 0
    }
  }
  let added = true
  while (added) {
    added = false
    for (const node of graph.nodes) {
      const owner = node.data.containerId
      if (owner !== undefined && nodeIds.has(owner) && !nodeIds.has(node.id)) {
        nodeIds.add(node.id)
        added = true
      }
    }
  }
  return {
    nodeIds,
    edgeIds: new Set(
      graph.edges
        .filter((edge) => nodeIds.has(edge.source) || nodeIds.has(edge.target))
        .map((edge) => edge.id),
    ),
    memberCount,
  }
}

/** Returns the persisted expanded size, defaulting old snapshots to the supported minimum. */
export function iterationExpandedSize(node: WorkflowIterationNode): {
  width: number
  height: number
} {
  return {
    width: finiteSize(node.initialWidth, WORKFLOW_ITERATION_NODE_WIDTH),
    height: finiteSize(node.initialHeight, WORKFLOW_ITERATION_NODE_HEIGHT),
  }
}

/** Normalizes a new region member without persisting presentation-only frame dotting. */
function prepareIterationMember(
  node: WorkflowIterationNode,
  iterationId: string,
  position: { x: number; y: number },
): WorkflowIterationNode {
  const serializable = { ...node }
  delete serializable.extent
  delete serializable.expandParent
  const data =
    node.data.kind === 'agent' && node.data.agentConfig !== undefined
      ? {
          ...node.data,
          agentConfig: { ...node.data.agentConfig, interactive: false },
        }
      : node.data
  return {
    ...serializable,
    parentId: iterationId,
    position,
    data,
  }
}

/** Places a new member close to the selected graph seam while keeping it inside the frame. */
function insertionPosition(
  graph: WorkflowIterationGraph,
  insertion: WorkflowIterationInsertion,
  inputNode: WorkflowIterationNode,
): { x: number; y: number } {
  const members = graph.nodes.filter((node) => node.parentId === insertion.iterationId)
  // The seam-derived point only approximates free space: midpoints can land on an
  // existing member (an entry seam's virtual source sits exactly on the first member's
  // row), so every placement is finally nudged below whatever it would overlap.
  const clearOfMembers = (position: { x: number; y: number }): { x: number; y: number } =>
    avoidMemberOverlap(members, position, nodeWidth(inputNode), nodeHeight(inputNode))
  if (insertion.type === 'entry') {
    const entryTargetIds = new Set(
      graph.edges
        .filter(
          (edge) =>
            edge.source === insertion.iterationId && edge.sourceHandle === 'iteration-entry',
        )
        .map((edge) => edge.target),
    )
    const entryMembers = members.filter((member) => entryTargetIds.has(member.id))
    const firstCandidateY =
      entryMembers.length === 0
        ? WORKFLOW_ITERATION_MEMBER_TOP
        : Math.max(...entryMembers.map((member) => member.position.y + nodeHeight(member))) +
          ITERATION_MEMBER_ROW_GAP
    return clearOfMembers({
      x: WORKFLOW_ITERATION_MEMBER_LEFT,
      y: nextFreeEntryRowY(members, inputNode, firstCandidateY),
    })
  }
  if (insertion.type === 'edge') {
    const edge = graph.edges.find((candidate) => candidate.id === insertion.edgeId)
    if (edge !== undefined) {
      const source = graph.nodes.find((node) => node.id === edge.source)
      const target = graph.nodes.find((node) => node.id === edge.target)
      if (source !== undefined && target !== undefined) {
        const sourcePosition =
          source.id === insertion.iterationId
            ? { x: 0, y: WORKFLOW_ITERATION_MEMBER_TOP }
            : source.position
        return clearOfMembers({
          x: Math.max(
            WORKFLOW_ITERATION_MEMBER_LEFT,
            Math.round((sourcePosition.x + target.position.x) / 2),
          ),
          y: Math.max(
            WORKFLOW_ITERATION_MEMBER_TOP,
            Math.round((sourcePosition.y + target.position.y) / 2),
          ),
        })
      }
    }
  }
  if (insertion.type === 'output') {
    const source = graph.nodes.find((node) => node.id === insertion.sourceId)
    if (source !== undefined) {
      const siblingTargets = graph.edges
        .filter(
          (edge) =>
            edge.source === insertion.sourceId && edge.sourceHandle === insertion.sourceHandle,
        )
        .map((edge) => graph.nodes.find((node) => node.id === edge.target))
        .filter((target): target is WorkflowIterationNode => target !== undefined)
      return clearOfMembers({
        x: source.position.x + nodeWidth(source) + ITERATION_MEMBER_COLUMN_GAP,
        y: stackedMemberTop(siblingTargets, source.position.y),
      })
    }
  }
  return clearOfMembers({
    x: WORKFLOW_ITERATION_MEMBER_LEFT,
    y: stackedMemberTop(members, WORKFLOW_ITERATION_MEMBER_TOP),
  })
}

/**
 * Moves an insertion point below any member box it would overlap.
 *
 * Seam-derived coordinates only approximate free space — an entry seam's virtual
 * source position, for example, sits on the first member's row, so its midpoint
 * can land exactly on an existing card. Pushing the placement below every
 * overlapped member (and re-checking, since the move itself can hit another
 * member) keeps the new card visible and clickable; the loop terminates because
 * each pass strictly increases the y coordinate.
 */
function avoidMemberOverlap(
  members: WorkflowIterationNode[],
  position: { x: number; y: number },
  width: number,
  height: number,
): { x: number; y: number } {
  const { x } = position
  let y = position.y
  let moved = true
  while (moved) {
    moved = false
    for (const member of members) {
      const overlaps =
        x < member.position.x + nodeWidth(member) &&
        x + width > member.position.x &&
        y < member.position.y + nodeHeight(member) &&
        y + height > member.position.y
      if (overlaps) {
        y = member.position.y + nodeHeight(member) + ITERATION_MEMBER_ROW_GAP
        moved = true
      }
    }
  }
  return { x, y }
}

/**
 * Stacks one new member below the real bottom of the anchor members.
 *
 * Card heights vary by kind and content far beyond the fixed placeholder, and a
 * freshly inserted card only gets measured after it renders, so counting
 * anchors with the placeholder height lets a tall rendered card overlap the
 * next placement. Using each anchor's measured-or-estimated bottom keeps
 * consecutive inserts clear of each other; with placeholder-sized anchors the
 * result matches the classic fixed-row stacking.
 */
function stackedMemberTop(anchors: WorkflowIterationNode[], baseTop: number): number {
  const lowestBottom = anchors.reduce(
    (bottom, anchor) => Math.max(bottom, anchor.position.y + nodeHeight(anchor)),
    baseTop - ITERATION_MEMBER_ROW_GAP,
  )
  return Math.max(baseTop, lowestBottom + ITERATION_MEMBER_ROW_GAP)
}

/** Finds the next first-column row whose rectangle does not overlap an authored member. */
function nextFreeEntryRowY(
  members: readonly WorkflowIterationNode[],
  inputNode: WorkflowIterationNode,
  firstCandidateY: number,
): number {
  const candidateRight = WORKFLOW_ITERATION_MEMBER_LEFT + nodeWidth(inputNode)
  const candidateHeight = nodeHeight(inputNode)
  const obstacles = members
    .filter(
      (member) =>
        member.position.x < candidateRight &&
        member.position.x + nodeWidth(member) > WORKFLOW_ITERATION_MEMBER_LEFT,
    )
    .toSorted((left, right) => left.position.y - right.position.y)
  let candidateY = firstCandidateY
  for (const obstacle of obstacles) {
    const obstacleBottom = obstacle.position.y + nodeHeight(obstacle)
    const overlapsWithGap =
      candidateY < obstacleBottom + ITERATION_MEMBER_ROW_GAP &&
      candidateY + candidateHeight + ITERATION_MEMBER_ROW_GAP > obstacle.position.y
    if (overlapsWithGap) {
      candidateY = obstacleBottom + ITERATION_MEMBER_ROW_GAP
    }
  }
  return candidateY
}

/**
 * Applies either monotonic expansion or compact fitting to selected iteration frames.
 *
 * Nested regions read member sizes from the same working node list. One map pass can
 * enlarge an inner frame while the outer still sees the pre-expand size, so the next
 * measurement tick expands the ancestor and the canvas appears to thrash. The pass loop
 * repeats until a pass is a no-op, bounded by the nesting depth.
 */
function resizeIterationFrames<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  iterationIds: readonly string[] | undefined,
  mode: 'expand' | 'compact',
): TGraph {
  const selectedIds = iterationIds === undefined ? null : new Set(iterationIds)
  let nodes = graph.nodes
  let changed = false
  for (let pass = 0; pass < nodes.length; pass += 1) {
    let passChanged = false
    const nextNodes = nodes.map((node) => {
      if (node.data.kind !== 'iteration' || (selectedIds !== null && !selectedIds.has(node.id))) {
        return node
      }
      const members = nodes.filter((candidate) => candidate.parentId === node.id)
      const requiredWidth = Math.max(
        WORKFLOW_ITERATION_NODE_WIDTH,
        ...members.map(
          (member) => member.position.x + nodeWidth(member) + ITERATION_FRAME_RIGHT_PADDING,
        ),
      )
      const requiredHeight = Math.max(
        WORKFLOW_ITERATION_NODE_HEIGHT,
        ...members.map(
          (member) => member.position.y + nodeHeight(member) + ITERATION_FRAME_BOTTOM_PADDING,
        ),
      )
      const current = iterationExpandedSize(node)
      const width = snapSize(
        mode === 'expand' ? Math.max(current.width, requiredWidth) : requiredWidth,
      )
      const height = snapSize(
        mode === 'expand' ? Math.max(current.height, requiredHeight) : requiredHeight,
      )
      if (node.initialWidth === width && node.initialHeight === height) {
        return node
      }
      passChanged = true
      const resized: WorkflowIterationNode = {
        ...node,
        initialWidth: width,
        initialHeight: height,
      }
      if (node.width !== undefined || node.height !== undefined) {
        resized.width = width
        resized.height = height
      }
      return resized
    })
    if (!passChanged) {
      break
    }
    nodes = nextNodes
    changed = true
  }
  return changed ? { ...graph, nodes } : graph
}

/**
 * Mirrors container resizes onto the frames' authored size.
 *
 * A manual resize gesture (a `dimensions` change with `resizing` set) sets an iteration or
 * Loop frame to the dragged size. A Loop also grows reactively: React Flow's `expandParent`
 * reports the enlarged parent as a `dimensions` change with `setAttributes`, which is mirrored
 * only as growth so a later measurement can never shrink a box the author or a member opened.
 * Plain measurements carry neither flag and never touch the authored size.
 *
 * Generic over the node type so the editor feeds its canvas nodes in and gets
 * the same shape back: the transform only copies nodes through and widens the
 * frames it owns, so nothing changes about callers' concrete types.
 */
export function applyIterationFrameResize<TNode extends WorkflowIterationNode>(
  nodes: readonly TNode[],
  changes: readonly WorkflowIterationFrameChange[],
): TNode[] {
  const resizedSizes = new Map<string, { width: number; height: number; grow: boolean }>()
  for (const change of changes) {
    // Add-changes carry no id and no size.
    if (
      change.type !== 'dimensions' ||
      change.dimensions === undefined ||
      change.id === undefined
    ) {
      continue
    }
    if (change.resizing === true) {
      resizedSizes.set(change.id, { ...change.dimensions, grow: false })
    } else if (change.setAttributes !== undefined && change.setAttributes !== false) {
      resizedSizes.set(change.id, { ...change.dimensions, grow: true })
    }
  }
  if (resizedSizes.size === 0) {
    return [...nodes]
  }
  let changed = false
  const resized = nodes.map((node) => {
    const size = resizedSizes.get(node.id)
    // Members resize through node.width; only container frames keep their editable
    // size in initialWidth/initialHeight, and only a Loop grows with its members.
    const isFrame =
      node.data.kind === 'loop' || (node.data.kind === 'iteration' && size?.grow === false)
    if (size === undefined || !isFrame) {
      return node
    }
    const width = snapSize(size.grow ? Math.max(size.width, node.initialWidth ?? 0) : size.width)
    const height = snapSize(
      size.grow ? Math.max(size.height, node.initialHeight ?? 0) : size.height,
    )
    if (node.initialWidth === width && node.initialHeight === height) {
      return node
    }
    changed = true
    return { ...node, initialWidth: width, initialHeight: height }
  })
  return changed ? resized : [...nodes]
}

/** Returns the visual width used for fitting and insertion. */
export function nodeWidth(node: WorkflowIterationNode): number {
  // Iteration frames author their box in initialWidth; measured can include
  // subpixel chrome and would keep ratcheting an ancestor frame on every pass.
  if (node.data.kind === 'iteration') {
    return finiteSize(
      node.initialWidth ?? node.width ?? node.measured?.width,
      WORKFLOW_ITERATION_NODE_WIDTH,
    )
  }
  return finiteSize(
    node.measured?.width ?? node.width ?? node.initialWidth,
    node.data.kind === 'condition' ? WORKFLOW_CONDITION_NODE_WIDTH : WORKFLOW_NODE_WIDTH,
  )
}

/** Returns the visual height used for fitting and insertion. */
export function nodeHeight(node: WorkflowIterationNode): number {
  if (node.data.kind === 'iteration') {
    return finiteSize(
      node.initialHeight ?? node.height ?? node.measured?.height,
      WORKFLOW_ITERATION_NODE_HEIGHT,
    )
  }
  return finiteSize(
    node.measured?.height ?? node.height ?? node.initialHeight,
    WORKFLOW_NODE_INITIAL_HEIGHT,
  )
}

/** Accepts only positive finite dimensions from persisted or measured geometry. */
export function finiteSize(value: number | undefined, fallback: number): number {
  return value !== undefined && Number.isFinite(value) && value > 0 ? value : fallback
}

/** Aligns frame dimensions with the editor's twenty-pixel grid. */
export function snapSize(value: number): number {
  return Math.ceil(value / 20) * 20
}

/** Produces a stable unused graph id without leaking editor state into the transform. */
export function uniqueGraphId(prefix: string, occupiedIds: Iterable<string>): string {
  const occupied = new Set(occupiedIds)
  let sequence = 1
  while (occupied.has(`${prefix}-${sequence}`)) {
    sequence += 1
  }
  return `${prefix}-${sequence}`
}
