import { describe, expect, it } from 'vitest'
import { applyIterationDragRules } from '@/features/workflows/runtime/iteration-containment'
import type {
  WorkflowIterationGraph,
  WorkflowIterationNode,
} from '@/features/workflows/runtime/iteration-graph'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** Builds one graph node carrying the fields the drag guard reads. */
function workflowNode(
  id: string,
  kind: WorkflowNodeKind,
  position: { x: number; y: number },
  parentId?: string,
): WorkflowIterationNode {
  return {
    id,
    position,
    data: { kind },
    ...(parentId === undefined ? {} : { parentId }),
  }
}

/** Builds a graph with the given nodes and no edges, as the guard only reads membership. */
function workflowOf(nodes: WorkflowIterationNode[]): WorkflowIterationGraph {
  return { nodes, edges: [] }
}

/** Returns the frame every test graph places first; guards against an empty list. */
function iterationFrame(nodes: readonly WorkflowIterationNode[]): WorkflowIterationNode {
  const frame = nodes[0]
  if (frame === undefined) {
    throw new Error('the test graph never omits its iteration frame')
  }
  return frame
}

describe('iteration drag rules', () => {
  it('restores an outer node dropped over an expanded iteration region', () => {
    const before = workflowOf([
      workflowNode('iter', 'iteration', { x: 0, y: 0 }),
      workflowNode('fix', 'agent', { x: 700, y: 40 }),
    ])
    const after = workflowOf([
      iterationFrame(before.nodes),
      workflowNode('fix', 'agent', { x: 100, y: 180 }),
    ])

    const result = applyIterationDragRules(after, before, ['fix'])

    expect(result.rejectedNodeIds).toEqual(['fix'])
    expect(result.workflow.nodes[1]?.position).toEqual({ x: 700, y: 40 })
    expect(result.workflow.nodes[1]?.parentId).toBeUndefined()
  })

  it('returns an outer node dropped on the measured part of a Loop frame', () => {
    const loop: WorkflowIterationNode = {
      ...workflowNode('loop', 'loop', { x: 0, y: 0 }),
      initialWidth: 620,
      initialHeight: 300,
      measured: { width: 628, height: 363 },
    }
    const before = workflowOf([loop, workflowNode('fix', 'agent', { x: 900, y: 40 })])
    // The card's centre lands at y≈330: below the authored 300, inside the drawn 363.
    const after = workflowOf([loop, workflowNode('fix', 'agent', { x: 100, y: 281 })])

    const result = applyIterationDragRules(after, before, ['fix'])

    expect(result.rejectedNodeIds).toEqual(['fix'])
    expect(result.workflow.nodes[1]?.position).toEqual({ x: 900, y: 40 })
  })

  it('never grants membership when a node moves over a frame', () => {
    const before = workflowOf([
      workflowNode('iter', 'iteration', { x: 0, y: 0 }),
      workflowNode('fix', 'agent', { x: 700, y: 40 }),
    ])
    const after = workflowOf([
      iterationFrame(before.nodes),
      workflowNode('fix', 'agent', { x: 40, y: 20 }),
    ])

    const result = applyIterationDragRules(after, before, ['fix'])

    expect(result.workflow.nodes[1]?.parentId).toBeUndefined()
  })

  it('keeps a member in its owner and expands the frame after member movement', () => {
    const before = workflowOf([
      workflowNode('iter', 'iteration', { x: 0, y: 0 }),
      workflowNode('fix', 'agent', { x: 60, y: 180 }, 'iter'),
    ])
    const after = workflowOf([
      iterationFrame(before.nodes),
      workflowNode('fix', 'agent', { x: 620, y: 400 }, 'iter'),
    ])

    const result = applyIterationDragRules(after, before, ['fix'])
    const frame = result.workflow.nodes[0]

    expect(result.rejectedNodeIds).toEqual([])
    expect(result.workflow.nodes[1]?.parentId).toBe('iter')
    expect(frame?.initialWidth).toBeGreaterThan(560)
    expect(frame?.initialHeight).toBeGreaterThan(340)
  })

  it('does not treat a collapsed frame as an active region drop target', () => {
    const frame = workflowNode('iter', 'iteration', { x: 0, y: 0 })
    frame.data = { ...frame.data, collapsed: true }
    const before = workflowOf([frame, workflowNode('fix', 'agent', { x: 700, y: 40 })])
    const after = workflowOf([frame, workflowNode('fix', 'agent', { x: 100, y: 180 })])

    const result = applyIterationDragRules(after, before, ['fix'])

    expect(result.rejectedNodeIds).toEqual([])
    expect(result.workflow.nodes[1]?.position).toEqual({ x: 100, y: 180 })
  })
})
