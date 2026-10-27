import { effectiveWorkflowContainer } from '@/features/workflows/runtime/container-layout'
import {
  finiteSize,
  nodeHeight,
  nodeWidth,
  snapSize,
  uniqueGraphId,
  type WorkflowIterationEdge,
  type WorkflowIterationGraph,
  type WorkflowIterationInsertion,
  type WorkflowIterationNode,
} from '@/features/workflows/runtime/iteration-graph'
import {
  WORKFLOW_LOOP_NODE_HEIGHT,
  WORKFLOW_LOOP_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'

/**
 * Loop-body graph transforms, free of any canvas-library import.
 *
 * A Loop's body is closed over the Loop: members carry the Loop's id on both `parentId`
 * (what React Flow nests by) and `data.containerId` (what the backend resolves first), so a
 * member is never placed by geometry and a save/load round trip cannot move it out.
 */

/** Appends a member after one output port of a Loop body member. */
export interface WorkflowLoopOutputInsertion {
  type: 'loop-output'
  loopId: string
  sourceId: string
  /** The port the new edge leaves from; a condition case or ELSE keeps its branch. */
  sourceHandle?: string | null
}

/** Any explicit seam a container member is added through. */
export type WorkflowContainerInsertion = WorkflowIterationInsertion | WorkflowLoopOutputInsertion

/** Horizontal gap between a member and the successor appended after it. */
const LOOP_MEMBER_COLUMN_GAP = 100
/** Vertical gap kept between stacked members. */
const LOOP_MEMBER_ROW_GAP = 52
/** Breathing room the Loop keeps right of and below its outermost member. */
const LOOP_FRAME_RIGHT_PADDING = 48
const LOOP_FRAME_BOTTOM_PADDING = 40

/**
 * Returns whether a node may have a successor appended inside its Loop.
 *
 * Output and loopExit have no output port, and only a member of a Loop that is still in the
 * graph has a body to append into.
 */
export function loopOutputInsertion(
  graph: WorkflowIterationGraph,
  sourceId: string,
  sourceHandle: string | null,
): WorkflowLoopOutputInsertion | null {
  const source = graph.nodes.find((node) => node.id === sourceId)
  if (source === undefined || source.data.kind === 'output' || source.data.kind === 'loopExit') {
    return null
  }
  const loopId = effectiveWorkflowContainer(source)
  if (loopId === undefined || !isLoop(graph, loopId)) {
    return null
  }
  return { type: 'loop-output', loopId, sourceId, sourceHandle }
}

/**
 * Picks the seam the inspector's "add early exit" action appends after: the right-most body
 * member that does round work and has a single output port, so the exit lands clear of the
 * existing round instead of on a fixed coordinate.
 *
 * Two kinds are never the source. A Condition: the action names no branch, and silently wiring
 * the exit to ELSE would hide which branch now ends the round. The body Start: an exit right
 * after it would end every round before any work ran. When nothing else qualifies there is no
 * seam, and exits on a branch are added from that branch's own port.
 */
export function loopExitInsertion(
  graph: WorkflowIterationGraph,
  loopId: string,
): WorkflowLoopOutputInsertion | null {
  const sources = loopMembers(graph, loopId)
    .filter(
      (node) =>
        node.data.kind !== 'output' &&
        node.data.kind !== 'loopExit' &&
        node.data.kind !== 'condition' &&
        node.data.kind !== 'start',
    )
    .toSorted((left, right) => right.position.x - left.position.x)
  const source = sources[0]
  return source === undefined ? null : loopOutputInsertion(graph, source.id, null)
}

/**
 * Appends one member after a body member's port and grows the Loop to contain it.
 *
 * The new node sits one column right of its source, pushed below any sibling it would overlap
 * (including branches already attached to the same port), carries both ownership keys, and is
 * wired from the source port. Existing edges and feedback bindings are untouched. An insertion
 * whose source is not in that Loop, or cannot have successors, returns the graph unchanged.
 *
 * @param graph - The draft graph.
 * @param insertion - The body member and port to append after.
 * @param inputNode - The freshly created node; its position is replaced.
 * @returns The graph with the member, its edge and the grown Loop.
 */
export function insertLoopMember<TGraph extends WorkflowIterationGraph>(
  graph: TGraph,
  insertion: WorkflowLoopOutputInsertion,
  inputNode: WorkflowIterationNode,
): TGraph {
  const valid = loopOutputInsertion(graph, insertion.sourceId, insertion.sourceHandle ?? null)
  const source = graph.nodes.find((node) => node.id === insertion.sourceId)
  if (valid?.loopId !== insertion.loopId || source === undefined) {
    return graph
  }
  const position = clearOfMembers(
    loopMembers(graph, insertion.loopId),
    { x: source.position.x + nodeWidth(source) + LOOP_MEMBER_COLUMN_GAP, y: source.position.y },
    inputNode,
  )
  const member: WorkflowIterationNode = {
    ...withoutPresentation(inputNode),
    parentId: insertion.loopId,
    position,
    data: { ...inputNode.data, containerId: insertion.loopId },
  }
  const occupiedIds = [...graph.nodes, ...graph.edges, member].map((item) => item.id)
  const edge: WorkflowIterationEdge = {
    id: uniqueGraphId('edge', occupiedIds),
    source: source.id,
    ...(insertion.sourceHandle == null ? {} : { sourceHandle: insertion.sourceHandle }),
    target: member.id,
    type: 'workflow',
  }
  const nodes = graph.nodes.map((node) =>
    node.id === insertion.loopId ? grownLoop(node, member, graph.nodes) : node,
  )
  return { ...graph, nodes: [...nodes, member], edges: [...graph.edges, edge] }
}

/**
 * The size the author gave the Loop (or the palette default), ignoring anything React Flow
 * measured or `expandParent` wrote at runtime.
 *
 * The frame draws itself from this plus what its members need. It must not read its own
 * measured size back: that is the size it drew last time, so it could only ever grow and a body
 * that got smaller would leave the frame stuck at its largest box until the page reloaded.
 */
export function loopAuthoredSize(node: WorkflowIterationNode): { width: number; height: number } {
  return {
    width: finiteSize(node.initialWidth, WORKFLOW_LOOP_NODE_WIDTH),
    height: finiteSize(node.initialHeight, WORKFLOW_LOOP_NODE_HEIGHT),
  }
}

/**
 * The box a Loop occupies on the canvas: its authored size, widened to what React Flow measured.
 *
 * The frame draws itself at least as large as its members need ({@link loopContentBox}), which
 * can exceed both the authored size (a new Loop's starter agent is taller than the palette
 * group's box) and what React Flow measured (after an `expandParent` or a corner resize in this
 * session, React Flow pins its wrapper to the member edge, without the padding the frame adds).
 * Every geometric decision — refusing a palette drop on the frame, returning a dragged outer
 * node, insertion growth — must use the box the author actually sees, so it takes the largest of
 * the authored, measured and needed boxes.
 *
 * @param node - The Loop.
 * @param nodes - The graph's nodes, for the members' needed box.
 */
export function loopFrameSize(
  node: WorkflowIterationNode,
  nodes: readonly LoopContentNode[],
): { width: number; height: number } {
  const needed = loopContentBox(nodes, node.id)
  return {
    width: Math.max(
      finiteSize(node.initialWidth ?? node.width, WORKFLOW_LOOP_NODE_WIDTH),
      node.measured?.width ?? 0,
      needed.width,
    ),
    height: Math.max(
      finiteSize(node.initialHeight ?? node.height, WORKFLOW_LOOP_NODE_HEIGHT),
      node.measured?.height ?? 0,
      needed.height,
    ),
  }
}

/**
 * The node fields {@link loopContentBox} reads. React Flow's store nodes carry untyped data, so
 * this is deliberately looser than {@link WorkflowIterationNode}.
 */
export interface LoopContentNode {
  position: { x: number; y: number }
  parentId?: string | undefined
  measured?: { width?: number | undefined; height?: number | undefined } | undefined
  data: { containerId?: unknown }
}

/**
 * The box a Loop's measured members need, with the same padding insertion keeps.
 *
 * A body card is measured only after it renders, and React Flow does not run `expandParent` on
 * that first measurement, so the frame sizes itself from this instead of trusting the authored
 * box alone. Membership follows the backend rule (non-empty `containerId`, else `parentId`);
 * members not measured yet contribute their position only.
 */
export function loopContentBox(
  nodes: readonly LoopContentNode[],
  loopId: string,
): { width: number; height: number } {
  let width = 0
  let height = 0
  for (const node of nodes) {
    const containerId = node.data['containerId']
    const owner =
      typeof containerId === 'string' && containerId !== '' ? containerId : node.parentId
    if (owner !== loopId) {
      continue
    }
    width = Math.max(
      width,
      node.position.x + (node.measured?.width ?? 0) + LOOP_FRAME_RIGHT_PADDING,
    )
    height = Math.max(
      height,
      node.position.y + (node.measured?.height ?? 0) + LOOP_FRAME_BOTTOM_PADDING,
    )
  }
  return { width: Math.ceil(width), height: Math.ceil(height) }
}

/** Whether `id` names a Loop in the graph. */
function isLoop(graph: WorkflowIterationGraph, id: string): boolean {
  return graph.nodes.some((node) => node.id === id && node.data.kind === 'loop')
}

/** Every node the Loop owns, by the same rule the backend resolves membership with. */
function loopMembers(graph: WorkflowIterationGraph, loopId: string): WorkflowIterationNode[] {
  return graph.nodes.filter((node) => effectiveWorkflowContainer(node) === loopId)
}

/**
 * Moves a placement below every member box it would overlap, gaps included.
 *
 * Each pass strictly increases y, so the loop terminates after at most one move per member.
 */
function clearOfMembers(
  members: readonly WorkflowIterationNode[],
  start: { x: number; y: number },
  inputNode: WorkflowIterationNode,
): { x: number; y: number } {
  const width = nodeWidth(inputNode)
  const height = nodeHeight(inputNode)
  let y = start.y
  let moved = true
  while (moved) {
    moved = false
    for (const member of members) {
      const overlaps =
        start.x < member.position.x + nodeWidth(member) + LOOP_FRAME_RIGHT_PADDING &&
        start.x + width + LOOP_FRAME_RIGHT_PADDING > member.position.x &&
        y < member.position.y + nodeHeight(member) + LOOP_MEMBER_ROW_GAP &&
        y + height + LOOP_MEMBER_ROW_GAP > member.position.y
      if (overlaps) {
        y = member.position.y + nodeHeight(member) + LOOP_MEMBER_ROW_GAP
        moved = true
      }
    }
  }
  return { x: start.x, y }
}

/** Widens the Loop's authored box until the new member fits; it never shrinks. */
function grownLoop(
  loop: WorkflowIterationNode,
  member: WorkflowIterationNode,
  nodes: readonly WorkflowIterationNode[],
): WorkflowIterationNode {
  const current = loopFrameSize(loop, nodes)
  const width = snapSize(
    Math.max(current.width, member.position.x + nodeWidth(member) + LOOP_FRAME_RIGHT_PADDING),
  )
  const height = snapSize(
    Math.max(current.height, member.position.y + nodeHeight(member) + LOOP_FRAME_BOTTOM_PADDING),
  )
  const grown: WorkflowIterationNode = { ...loop, initialWidth: width, initialHeight: height }
  if (loop.width !== undefined || loop.height !== undefined) {
    grown.width = width
    grown.height = height
  }
  return grown
}

/** Drops canvas-only constraint fields so the draft never persists them. */
function withoutPresentation(node: WorkflowIterationNode): WorkflowIterationNode {
  const serializable = { ...node }
  delete serializable.extent
  delete serializable.expandParent
  return serializable
}
