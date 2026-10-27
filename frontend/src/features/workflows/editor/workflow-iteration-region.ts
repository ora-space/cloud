import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'
import {
  WORKFLOW_ITERATION_COLLAPSED_HEIGHT,
  WORKFLOW_ITERATION_COLLAPSED_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import {
  iterationExpandedSize,
  projectIterationEdges,
} from '@/features/workflows/runtime/iteration-graph'
import { loopFrameSize } from '@/features/workflows/runtime/loop-graph'
import type { WorkflowPosition } from '@/features/workflows/runtime/types'

/**
 * Iteration-region canvas presentation, one pure function the editor feeds.
 *
 * Everything here is a view of the persisted graph: membership comes from
 * `parentId` and never from geometry, so folding a region, constraining its
 * members or projecting its edges changes only how the canvas draws the same
 * document. The draft stays the source of truth and keeps whatever this module
 * derives out of it.
 */

/** The canvas will draw after region presentation is applied. */
export interface WorkflowIterationRegionPresentation {
  nodes: WorkflowCanvasNode[]
  edges: WorkflowCanvasEdge[]
}

/** Selected cards sit above their peers; frames and loops stay below their members. */
const SELECTED_Z_INDEX = 10
/** Ordinary cards; frames render under them so members are never covered. */
const REGULAR_NODE_Z_INDEX = 1
/** Frames and Loop containers render behind the work they own. */
const FRAME_Z_INDEX = 0

/**
 * Applies the canvas-only fields an iteration region needs to draw correctly.
 *
 * Members are constrained to their frame with `extent: "parent"` (without
 * `expandParent`, which would fight the region's own resize pass); Loop children
 * keep `expandParent` because Loop frames grow reactively rather than by author
 * gesture. A folded frame hides its members and projects their edges away, and
 * every frame carries the member count its summary badge reads. Frames come
 * first in the node order, as React Flow's nested layout requires.
 *
 * @param nodes - Persisted graph nodes.
 * @param edges - Persisted graph edges.
 * @returns The same graph plus the presentation fields, parents before children.
 */
export function buildIterationRegionGraph(
  nodes: readonly WorkflowCanvasNode[],
  edges: readonly WorkflowCanvasEdge[],
): WorkflowIterationRegionPresentation {
  const iterationIds = new Set(
    nodes.filter((node) => node.data.kind === 'iteration').map((node) => node.id),
  )
  const collapsedIds = new Set(
    nodes
      .filter((node) => node.data.kind === 'iteration' && node.data.collapsed === true)
      .map((node) => node.id),
  )
  const memberCounts = new Map<string, number>()
  for (const node of nodes) {
    const parentId = node.parentId
    if (parentId !== undefined) {
      memberCounts.set(parentId, (memberCounts.get(parentId) ?? 0) + 1)
    }
  }
  const projectedEdges = edges.map((edge) =>
    !edge.sourceHandle &&
    nodes.some((node) => node.id === edge.source && node.data.kind === 'condition')
      ? { ...edge, sourceHandle: 'else' }
      : edge,
  )
  const nodeById = new Map(nodes.map((node) => [node.id, node]))
  const presented = nodes.map((node): WorkflowCanvasNode => {
    const isFrame = iterationIds.has(node.id)
    const parentIsIteration = node.parentId !== undefined && iterationIds.has(node.parentId)
    const loopParent = node.data.containerId
    const isLoopChild = node.parentId !== undefined && loopParent !== undefined
    const loopParentExists = isLoopChild && loopParent !== undefined && nodeById.has(loopParent)
    const liveMemberHidden =
      node.parentId !== undefined && collapsedIds.has(node.parentId)
        ? { hidden: true as const }
        : {}
    return {
      ...node,
      ...constraintAttributes(node, parentIsIteration, isLoopChild, loopParentExists),
      ...liveMemberHidden,
      ...(isFrame
        ? { data: { ...node.data, regionMemberCount: memberCounts.get(node.id) ?? 0 } }
        : {}),
      zIndex: presentationZIndex(node, isFrame),
    }
  })
  return {
    nodes: orderForCanvas(presented, nodeById),
    // The projection is generic over the caller's edge type, so the presented
    // edges come back with the same concrete canvas shape they went in with.
    edges: projectIterationEdges({ nodes: presented, edges: projectedEdges }, collapsedIds),
  }
}

/**
 * Constraint attributes a node needs so React Flow draws it inside its owner.
 *
 * Iteration members are clamped to their frame with `extent: "parent"` but no
 * `expandParent`, because the region's box is authored by the author's resize
 * gesture and must not be pushed open reactively. Loop children keep
 * `expandParent` because a Loop container grows to fit its members instead.
 *
 * @param loopParentExists - The author declared a loop, and it is still in the graph.
 */
function constraintAttributes(
  node: WorkflowCanvasNode,
  parentIsIteration: boolean,
  isLoopChild: boolean,
  loopParentExists: boolean,
): Pick<WorkflowCanvasNode, 'extent' | 'expandParent'> {
  if (node.parentId === undefined) {
    return {}
  }
  if (parentIsIteration) {
    return { extent: 'parent' as const }
  }
  if (isLoopChild && loopParentExists) {
    return { extent: 'parent' as const, expandParent: true as const }
  }
  return {}
}

/**
 * Draw order: selected cards sit above their peers, frames and loops below the
 * work they own so members are never covered.
 */
function presentationZIndex(node: WorkflowCanvasNode, isFrame: boolean): number {
  if (node.selected) {
    return SELECTED_Z_INDEX
  }
  if (isFrame || node.data.kind === 'loop') {
    return FRAME_Z_INDEX
  }
  return REGULAR_NODE_Z_INDEX
}

/**
 * Returns whether a flow-space point falls inside an expanded or folded frame.
 *
 * The palette drop path uses this to refuse a drop that would appear to grant
 * membership the region does not contain — adding into a region is an explicit
 * action at its entry seam, never a drag.
 *
 * @param nodes - Graph nodes, for the frames to test.
 * @param point - Flow-space point, from `screenToFlowPosition`.
 * @returns Whether the point lies inside any iteration or Loop frame.
 */
export function workflowPointInsideIterationFrame(
  nodes: readonly WorkflowCanvasNode[],
  point: WorkflowPosition,
): boolean {
  return nodes.some((node) => {
    if (node.data.kind === 'loop') {
      const size = loopFrameSize(node, nodes)
      return (
        point.x >= node.position.x &&
        point.x <= node.position.x + size.width &&
        point.y >= node.position.y &&
        point.y <= node.position.y + size.height
      )
    }
    if (node.data.kind !== 'iteration') {
      return false
    }
    const expanded = iterationExpandedSize(node)
    const width = node.data.collapsed === true ? WORKFLOW_ITERATION_COLLAPSED_WIDTH : expanded.width
    const height =
      node.data.collapsed === true ? WORKFLOW_ITERATION_COLLAPSED_HEIGHT : expanded.height
    return (
      point.x >= node.position.x &&
      point.x <= node.position.x + width &&
      point.y >= node.position.y &&
      point.y <= node.position.y + height
    )
  })
}

/**
 * Orders nodes parents-before-children, as React Flow's nested layout requires.
 *
 * Depth is the number of ancestors up to a frame or the graph root, counted with
 * a visited set so a malformed imported cycle cannot recurse forever; stable
 * sorting keeps the authored order among siblings.
 *
 * @param nodes - Nodes to order.
 * @param nodeById - Nodes by id, for ancestor lookups.
 * @returns The same nodes with every parent before its children.
 */
function orderForCanvas(
  nodes: readonly WorkflowCanvasNode[],
  nodeById: ReadonlyMap<string, WorkflowCanvasNode>,
): WorkflowCanvasNode[] {
  const depth = new Map<string, number>()
  function depthOf(id: string): number {
    const cached = depth.get(id)
    if (cached !== undefined) {
      return cached
    }
    const node = nodeById.get(id)
    const parentId = node?.parentId
    if (node === undefined || parentId === undefined) {
      depth.set(id, 0)
      return 0
    }
    // A parent that is itself a member adds one level; a cycle degrades to depth 0.
    const parentDepth = depthOf(parentId)
    depth.set(id, parentDepth + 1)
    return parentDepth + 1
  }
  for (const node of nodes) {
    depthOf(node.id)
  }
  return nodes.toSorted((left, right) => (depth.get(left.id) ?? 0) - (depth.get(right.id) ?? 0))
}
