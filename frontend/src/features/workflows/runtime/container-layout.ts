/** Minimal workflow node shape needed to restore visual container ownership. */
interface WorkflowContainerNode {
  id: string
  parentId?: string
  data: {
    containerId?: string
  }
}

/**
 * Canonicalizes Loop parentage from domain-owned `containerId` and orders parents
 * before descendants, as required by React Flow's nested-node layout.
 */
export function workflowContainerNodes<T extends WorkflowContainerNode>(nodes: readonly T[]): T[] {
  const knownIds = new Set(nodes.map((node) => node.id))
  const normalized = nodes.map((node) => {
    const containerId = node.data.containerId
    return containerId !== undefined && knownIds.has(containerId)
      ? { ...node, parentId: containerId }
      : node
  })
  const firstIndexById = new Map<string, number>()
  for (const [index, node] of normalized.entries()) {
    if (!firstIndexById.has(node.id)) {
      firstIndexById.set(node.id, index)
    }
  }

  const ordered: T[] = []
  const visited = new Set<number>()
  const visiting = new Set<number>()

  /** Visits the owning container first while tolerating malformed imported cycles. */
  function visit(index: number): void {
    if (visited.has(index) || visiting.has(index)) {
      return
    }
    visiting.add(index)
    const node = normalized[index]
    if (node === undefined) {
      return
    }
    const parentIndex = node.parentId === undefined ? undefined : firstIndexById.get(node.parentId)
    if (parentIndex !== undefined) {
      visit(parentIndex)
    }
    visiting.delete(index)
    visited.add(index)
    ordered.push(node)
  }

  for (let index = 0; index < normalized.length; index += 1) {
    visit(index)
  }
  return ordered
}

/** The node fields that decide which container owns a node. */
interface WorkflowContainerOwnedNode {
  parentId?: string | undefined
  data: {
    containerId?: string | undefined
  }
}

/**
 * Resolves the container that owns a node: a non-empty `containerId` wins, otherwise `parentId`.
 *
 * This is the same rule the backend's `workflowContainerID` applies, so the editor and the
 * publish gate agree on membership even when an imported graph carries only one of the two keys.
 * An empty string on either key means "not set", never a container called `""`.
 *
 * @param node - The node whose owner is being asked for.
 * @returns The owning container id, or `undefined` for a top-level node.
 */
export function effectiveWorkflowContainer(node: WorkflowContainerOwnedNode): string | undefined {
  const containerId = node.data.containerId
  if (containerId !== undefined && containerId !== '') {
    return containerId
  }
  const parentId = node.parentId
  return parentId !== undefined && parentId !== '' ? parentId : undefined
}
