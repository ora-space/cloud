import { describe, expect, it } from 'vitest'
import {
  isValidWorkflowConnection,
  workflowEdgeId,
  type WorkflowConnectionGraph,
  type WorkflowConnectionNode,
} from '@/features/workflows/runtime/connection-validation'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** An existing edge, in the shape the validation only reads endpoints from. */
function edge(source: string, target: string) {
  return { source, target }
}

/** A graph of top-level agents, for the rules that only look at edges. */
function graph(edges: WorkflowConnectionGraph['edges']): WorkflowConnectionGraph {
  return { nodes: [], edges }
}

/** One node placed in a scope, with the owner written on either key. */
function node(
  id: string,
  kind: WorkflowNodeKind,
  owner: { containerId?: string; parentId?: string } = {},
): WorkflowConnectionNode {
  return {
    id,
    ...(owner.parentId === undefined ? {} : { parentId: owner.parentId }),
    data: { kind, ...(owner.containerId === undefined ? {} : { containerId: owner.containerId }) },
  }
}

/**
 * A top-level agent, a loop with a body start, agent and exit, and an iteration with one member
 * owned only through `parentId`, the way the iteration factory writes it.
 */
const SCOPED_NODES: WorkflowConnectionNode[] = [
  node('outer', 'agent'),
  node('loop', 'loop'),
  node('loop-start', 'start', { containerId: 'loop', parentId: 'loop' }),
  node('loop-agent', 'agent', { containerId: 'loop', parentId: 'loop' }),
  node('loop-exit', 'loopExit', { containerId: 'loop' }),
  node('iter', 'iteration'),
  node('iter-agent', 'agent', { parentId: 'iter' }),
  node('stray', 'agent', { containerId: 'missing' }),
]

describe('isValidWorkflowConnection', () => {
  it('accepts a gesture between two different nodes', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, graph([]))).toBe(true)
  })

  it('rejects a self-loop, which would make a node depend on itself', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'a' }, graph([]))).toBe(false)
  })

  it('rejects a duplicate of an edge already in the graph', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, graph([edge('a', 'b')]))).toBe(
      false,
    )
  })

  it('accepts the reverse direction, which is a different execution order', () => {
    expect(isValidWorkflowConnection({ source: 'b', target: 'a' }, graph([edge('a', 'b')]))).toBe(
      true,
    )
  })

  it('rejects a gesture released on empty canvas', () => {
    expect(isValidWorkflowConnection({ source: null, target: 'b' }, graph([]))).toBe(false)
    expect(isValidWorkflowConnection({ source: 'a', target: null }, graph([]))).toBe(false)
    expect(isValidWorkflowConnection({ source: null, target: null }, graph([]))).toBe(false)
  })

  it('ignores edges that share only one endpoint with the candidate', () => {
    const edges = [edge('a', 'c'), edge('c', 'b')]

    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, graph(edges))).toBe(true)
  })

  it('allows separate condition branches to join the same target, but rejects the same port twice', () => {
    const edges = [{ source: 'condition', target: 'join', sourceHandle: 'case-1' }]
    expect(
      isValidWorkflowConnection(
        { source: 'condition', target: 'join', sourceHandle: 'else' },
        graph(edges),
      ),
    ).toBe(true)
    expect(
      isValidWorkflowConnection(
        { source: 'condition', target: 'join', sourceHandle: 'case-1' },
        graph(edges),
      ),
    ).toBe(false)
  })
})

describe('isValidWorkflowConnection scopes', () => {
  const scoped: WorkflowConnectionGraph = { nodes: SCOPED_NODES, edges: [] }

  it.each([
    ['inside one loop body', 'loop-start', 'loop-agent', null],
    ['from a body member into the loop exit', 'loop-agent', 'loop-exit', null],
    ['between top-level nodes and containers', 'outer', 'loop', null],
    ['out of a container to a top-level node', 'iter', 'outer', null],
    ['from an iteration entry into its own member', 'iter', 'iter-agent', 'iteration-entry'],
    ['from a dangling owner key, which reads as top level', 'stray', 'outer', null],
  ])('accepts an edge %s', (_name, source, target, sourceHandle) => {
    expect(isValidWorkflowConnection({ source, target, sourceHandle }, scoped)).toBe(true)
  })

  it.each([
    ['out of a loopExit, which is terminal in its round', 'loop-exit', 'loop-agent', null],
    ['from the top level into a loop body', 'outer', 'loop-agent', null],
    ['from a loop body out to the top level', 'loop-agent', 'outer', null],
    ['from a loop body into an iteration region', 'loop-agent', 'iter-agent', null],
    ['from the loop container into its own body', 'loop', 'loop-agent', null],
    ['from an iteration entry out of its region', 'iter', 'outer', 'iteration-entry'],
    ['from an iteration entry into another scope', 'iter', 'loop-agent', 'iteration-entry'],
  ])('rejects an edge %s', (_name, source, target, sourceHandle) => {
    expect(isValidWorkflowConnection({ source, target, sourceHandle }, scoped)).toBe(false)
  })
})

describe('workflowEdgeId', () => {
  it('normalizes an empty legacy port to the omitted port identity', () => {
    expect(workflowEdgeId('a', 'b', { sourceHandle: '', targetHandle: '' })).toBe(
      workflowEdgeId('a', 'b'),
    )
    expect(
      isValidWorkflowConnection(
        { source: 'a', target: 'b' },
        graph([{ source: 'a', target: 'b', sourceHandle: '', targetHandle: '' }]),
      ),
    ).toBe(false)
  })
  it('derives the id from the endpoints, so a reload cannot duplicate it', () => {
    expect(workflowEdgeId('agent-1', 'output-1')).toBe('e-agent-1-output-1')
  })

  it('distinguishes the two directions between the same pair', () => {
    expect(workflowEdgeId('a', 'b')).not.toBe(workflowEdgeId('b', 'a'))
  })

  it('gives parallel branch connections distinct ids that survive serialization', () => {
    const first = workflowEdgeId('condition', 'join', { sourceHandle: 'case-1' })
    const otherwise = workflowEdgeId('condition', 'join', { sourceHandle: 'else' })
    expect(first).not.toBe(otherwise)
    expect(first).toBe(workflowEdgeId('condition', 'join', { sourceHandle: 'case-1' }))
    expect(workflowEdgeId('condition', 'join', { sourceHandle: 'a/b' })).not.toBe(
      workflowEdgeId('condition', 'join', { sourceHandle: 'a%2Fb' }),
    )
  })
})
