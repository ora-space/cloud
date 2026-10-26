import { describe, expect, it } from 'vitest'
import {
  isValidWorkflowConnection,
  workflowEdgeId,
} from '@/features/workflows/runtime/connection-validation'

/** An existing edge, in the shape the validation only reads endpoints from. */
function edge(source: string, target: string) {
  return { source, target }
}

describe('isValidWorkflowConnection', () => {
  it('accepts a gesture between two different nodes', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, [])).toBe(true)
  })

  it('rejects a self-loop, which would make a node depend on itself', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'a' }, [])).toBe(false)
  })

  it('rejects a duplicate of an edge already in the graph', () => {
    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, [edge('a', 'b')])).toBe(false)
  })

  it('accepts the reverse direction, which is a different execution order', () => {
    expect(isValidWorkflowConnection({ source: 'b', target: 'a' }, [edge('a', 'b')])).toBe(true)
  })

  it('rejects a gesture released on empty canvas', () => {
    expect(isValidWorkflowConnection({ source: null, target: 'b' }, [])).toBe(false)
    expect(isValidWorkflowConnection({ source: 'a', target: null }, [])).toBe(false)
    expect(isValidWorkflowConnection({ source: null, target: null }, [])).toBe(false)
  })

  it('ignores edges that share only one endpoint with the candidate', () => {
    const edges = [edge('a', 'c'), edge('c', 'b')]

    expect(isValidWorkflowConnection({ source: 'a', target: 'b' }, edges)).toBe(true)
  })

  it('allows separate condition branches to join the same target, but rejects the same port twice', () => {
    const edges = [{ source: 'condition', target: 'join', sourceHandle: 'case-1' }]
    expect(
      isValidWorkflowConnection(
        { source: 'condition', target: 'join', sourceHandle: 'else' },
        edges,
      ),
    ).toBe(true)
    expect(
      isValidWorkflowConnection(
        { source: 'condition', target: 'join', sourceHandle: 'case-1' },
        edges,
      ),
    ).toBe(false)
  })
})

describe('workflowEdgeId', () => {
  it('normalizes an empty legacy port to the omitted port identity', () => {
    expect(workflowEdgeId('a', 'b', { sourceHandle: '', targetHandle: '' })).toBe(
      workflowEdgeId('a', 'b'),
    )
    expect(
      isValidWorkflowConnection({ source: 'a', target: 'b' }, [
        { source: 'a', target: 'b', sourceHandle: '', targetHandle: '' },
      ]),
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
