import {
  WORKFLOW_ITERATION_COLLAPSED_HEIGHT,
  WORKFLOW_ITERATION_COLLAPSED_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import {
  expandIterationFrames,
  iterationExpandedSize,
  nodeHeight,
  nodeWidth,
  type WorkflowIterationGraph,
  type WorkflowIterationNode,
} from '@/features/workflows/runtime/iteration-graph'
import { loopFrameSize } from '@/features/workflows/runtime/loop-graph'

/**
 * Enforces container membership after a drag, never inferring it from geometry.
 *
 * Existing members keep their `parentId` and may expand their owner; an outer node whose
 * center was dropped over a region returns to its drag start position so the canvas cannot
 * imply membership the graph document does not contain. Rejections are reported so the
 * editor can tell the author what did not move.
 */
export interface WorkflowIterationDragResult<TWorkflow extends WorkflowIterationGraph> {
  workflow: TWorkflow
  rejectedNodeIds: string[]
}

/**
 * Enforces authored iteration membership after a drag.
 *
 * @param workflow - The graph as it is after the drag gesture moved nodes.
 * @param beforeDrag - The graph as it was before the drag, for restoring rejected drops.
 * @param draggedNodeIds - Ids the gesture actually moved; untouched nodes pass through.
 * @returns The corrected workflow and the ids returned to their drag start.
 */
export function applyIterationDragRules<TWorkflow extends WorkflowIterationGraph>(
  workflow: TWorkflow,
  beforeDrag: WorkflowIterationGraph,
  draggedNodeIds: readonly string[],
): WorkflowIterationDragResult<TWorkflow> {
  const draggedIds = new Set(draggedNodeIds)
  const originalById = new Map(beforeDrag.nodes.map((node) => [node.id, node] as const))
  // Each frame lists the span it currently owns: the expanded size when open, the
  // collapsed bar size while folded. A collapsed region is a small bar on the canvas,
  // so a drop on it must not grant membership the author never drew.
  const frames = workflow.nodes
    .filter((node) => node.data.kind === 'iteration' || node.data.kind === 'loop')
    .map((node) => {
      if (node.data.kind === 'loop') {
        // A Loop frame never folds; its box is the authored (or member-grown) size.
        const { width, height } = loopFrameSize(node, workflow.nodes)
        return { node, width, height, visibleWidth: width, visibleHeight: height }
      }
      const expanded = iterationExpandedSize(node)
      const open = node.data.collapsed !== true
      return {
        node,
        width: expanded.width,
        height: expanded.height,
        visibleWidth: open ? expanded.width : WORKFLOW_ITERATION_COLLAPSED_WIDTH,
        visibleHeight: open ? expanded.height : WORKFLOW_ITERATION_COLLAPSED_HEIGHT,
      }
    })
  const rejectedNodeIds: string[] = []
  let changed = false
  const nodes = workflow.nodes.map((node): WorkflowIterationNode => {
    if (!draggedIds.has(node.id) || node.parentId !== undefined) {
      return node
    }
    const center = {
      x: node.position.x + nodeWidth(node) / 2,
      y: node.position.y + nodeHeight(node) / 2,
    }
    const overlapsRegion = frames.some(
      (frame) =>
        frame.node.id !== node.id &&
        center.x >= frame.node.position.x &&
        center.x <= frame.node.position.x + frame.visibleWidth &&
        center.y >= frame.node.position.y &&
        center.y <= frame.node.position.y + frame.visibleHeight,
    )
    if (!overlapsRegion) {
      return node
    }
    const original = originalById.get(node.id)
    if (original === undefined) {
      return node
    }
    rejectedNodeIds.push(node.id)
    changed = true
    return { ...node, position: { ...original.position } }
  })
  const withRejectedDropsRestored = changed ? ({ ...workflow, nodes } as TWorkflow) : workflow
  // Only iteration members that moved widen their owner (a Loop grows through `expandParent`);
  // a drag already ran `expandIterationFrames`,
  // and replaying the same gesture against an already-wide frame must not ratchet it again.
  const affectedIterationIds = workflow.nodes.flatMap((node): string[] => {
    const parentId = node.parentId
    if (
      parentId === undefined ||
      !draggedIds.has(node.id) ||
      !frames.some((frame) => frame.node.id === parentId)
    ) {
      return []
    }
    return [parentId]
  })
  return {
    workflow: expandIterationFrames(withRejectedDropsRestored, affectedIterationIds),
    rejectedNodeIds,
  }
}
