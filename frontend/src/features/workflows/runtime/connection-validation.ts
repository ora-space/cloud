import { effectiveWorkflowContainer } from '@/features/workflows/runtime/container-layout'
import type { WorkflowLayoutEdge } from '@/features/workflows/runtime/layout'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

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

/** The node fields the connection gate reads to place an endpoint in its scope. */
export interface WorkflowConnectionNode {
  id: string
  parentId?: string | undefined
  data: {
    kind: WorkflowNodeKind
    containerId?: string | undefined
  }
}

/** The graph a candidate connection would join. */
export interface WorkflowConnectionGraph {
  nodes: readonly WorkflowConnectionNode[]
  edges: readonly (WorkflowLayoutEdge & WorkflowConnectionHandles)[]
}

/** The iteration frame's internal start port, which feeds only that frame's members. */
const ITERATION_ENTRY_HANDLE = 'iteration-entry'

/**
 * Decides whether a connection gesture may become an edge.
 *
 * Every rejected case produces a graph the publish gate refuses or the runtime cannot
 * execute, so refusing at the gesture keeps the author from drawing it at all:
 *
 * - a self-loop, which would make a node depend on itself;
 * - a duplicate, which would run the same node twice from one predecessor;
 * - an incomplete gesture, released on empty canvas;
 * - an edge out of a loopExit, which is terminal inside its round;
 * - an edge between scopes (a loop body, an iteration region, the top level), which the
 *   backend rejects because a round can never see the other scope's state;
 * - an iteration's internal start feeding anything but that iteration's own members.
 *
 * Scope follows the backend rule (`containerId`, else `parentId`), and only an owner that is
 * really a loop or iteration in this graph counts, so a dangling key reads as top level.
 *
 * @param candidate - Endpoints React Flow is proposing.
 * @param graph - The nodes and edges already in the graph.
 * @returns Whether the edge may be added.
 */
export function isValidWorkflowConnection(
  candidate: WorkflowConnectionCandidate,
  graph: WorkflowConnectionGraph,
): boolean {
  const { source, target } = candidate
  if (source === null || target === null || source === target) {
    return false
  }
  if (!respectsWorkflowScopes(source, target, candidate.sourceHandle ?? null, graph.nodes)) {
    return false
  }
  return !graph.edges.some(
    (edge) =>
      edge.source === source &&
      edge.target === target &&
      (edge.sourceHandle || null) === (candidate.sourceHandle || null) &&
      (edge.targetHandle || null) === (candidate.targetHandle || null),
  )
}

/** Whether an edge stays inside one scope, allowing an iteration's entry into its own region. */
function respectsWorkflowScopes(
  source: string,
  target: string,
  sourceHandle: string | null,
  nodes: readonly WorkflowConnectionNode[],
): boolean {
  const nodeById = new Map(nodes.map((node) => [node.id, node] as const))
  const sourceNode = nodeById.get(source)
  if (sourceNode?.data.kind === 'loopExit') {
    return false
  }
  const scopeOf = (nodeId: string): string | undefined => {
    const node = nodeById.get(nodeId)
    const owner = node === undefined ? undefined : effectiveWorkflowContainer(node)
    const ownerKind = owner === undefined ? undefined : nodeById.get(owner)?.data.kind
    return ownerKind === 'loop' || ownerKind === 'iteration' ? owner : undefined
  }
  const targetScope = scopeOf(target)
  if (sourceHandle === ITERATION_ENTRY_HANDLE) {
    return sourceNode?.data.kind === 'iteration' && targetScope === source
  }
  return scopeOf(source) === targetScope
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
