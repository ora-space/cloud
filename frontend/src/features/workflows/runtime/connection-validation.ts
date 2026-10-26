import type { WorkflowLayoutEdge } from '@/features/workflows/runtime/layout'

/** Port identities distinguish parallel branch edges between the same nodes. */
interface WorkflowConnectionHandles {
  sourceHandle?: string | null
  targetHandle?: string | null
}

/** The endpoint pair a candidate connection would create. */
export interface WorkflowConnectionCandidate extends WorkflowConnectionHandles {
  source: string | null
  target: string | null
}

/**
 * Decides whether a connection gesture may become an edge.
 *
 * Three cases are rejected because each produces a graph the runtime cannot
 * execute and the editor cannot repair:
 *
 * - a self-loop, which would make a node depend on itself;
 * - a duplicate, which would run the same node twice from one predecessor;
 * - an incomplete gesture, released on empty canvas.
 *
 * @param candidate - Endpoints React Flow is proposing.
 * @param edges - Edges already in the graph.
 * @returns Whether the edge may be added.
 */
export function isValidWorkflowConnection(
  candidate: WorkflowConnectionCandidate,
  edges: readonly (WorkflowLayoutEdge & WorkflowConnectionHandles)[],
): boolean {
  const { source, target } = candidate
  if (source === null || target === null || source === target) {
    return false
  }
  return !edges.some(
    (edge) =>
      edge.source === source &&
      edge.target === target &&
      (edge.sourceHandle || null) === (candidate.sourceHandle || null) &&
      (edge.targetHandle || null) === (candidate.targetHandle || null),
  )
}

/**
 * Builds the id of the edge between two nodes.
 *
 * Including the port pair keeps sibling branches distinct. Deriving the id from these identities
 * rather than from a counter keeps it stable
 * across a reload, so an edge is never duplicated by re-adding the same
 * connection.
 *
 * @param source - Source node id.
 * @param target - Target node id.
 * @returns The edge id.
 */
export function workflowEdgeId(
  source: string,
  target: string,
  handles: WorkflowConnectionHandles = {},
): string {
  const base = `e-${source}-${target}`
  const sourceHandle = handles.sourceHandle || null
  const targetHandle = handles.targetHandle || null
  if (sourceHandle === null && targetHandle === null) {
    return base
  }
  return `${base}-${encodeURIComponent(JSON.stringify([sourceHandle, targetHandle]))}`
}
