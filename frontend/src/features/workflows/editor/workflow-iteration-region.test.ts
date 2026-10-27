import { describe, expect, it } from 'vitest'
import {
  WORKFLOW_ITERATION_COLLAPSED_HEIGHT,
  WORKFLOW_ITERATION_COLLAPSED_WIDTH,
  WORKFLOW_ITERATION_NODE_HEIGHT,
  WORKFLOW_ITERATION_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'
import {
  buildIterationRegionGraph,
  workflowPointInsideIterationFrame,
} from '@/features/workflows/editor/workflow-iteration-region'

/** One stored draft node with optional region ownership. */
function node(
  id: string,
  kind: WorkflowCanvasNode['data']['kind'],
  options: {
    parentId?: string
    position?: { x: number; y: number }
    collapsed?: boolean
    selected?: boolean
    containerId?: string
    initialWidth?: number
  } = {},
): WorkflowCanvasNode {
  return {
    id,
    type: 'workflow',
    position: options.position ?? { x: 0, y: 0 },
    ...(options.parentId === undefined ? {} : { parentId: options.parentId }),
    ...(options.selected === true ? { selected: true } : {}),
    ...(options.initialWidth === undefined ? {} : { initialWidth: options.initialWidth }),
    data: {
      kind,
      title: `${kind} ${id}`,
      description: '',
      ...(options.containerId === undefined ? {} : { containerId: options.containerId }),
      ...(options.collapsed === undefined ? {} : { collapsed: options.collapsed }),
    },
  }
}

/** An iteration frame with a persisted expanded size and its members, in draft order. */
function regionGraph(overrides: { collapsed?: boolean } = {}) {
  const frameOptions: { position: { x: number; y: number }; collapsed?: boolean } = {
    position: { x: 0, y: 0 },
  }
  // EOPT: only author the flag when the caller asked for it.
  if (overrides.collapsed !== undefined) {
    frameOptions.collapsed = overrides.collapsed
  }
  return {
    nodes: [
      node('iteration-1', 'iteration', frameOptions),
      node('agent-1', 'agent', { parentId: 'iteration-1', position: { x: 120, y: 100 } }),
      node('output-1', 'output', { position: { x: 900, y: 0 } }),
    ],
    edges: [
      { id: 'e1', source: 'iteration-1', target: 'agent-1' },
      { id: 'e2', source: 'agent-1', target: 'output-1' },
    ],
  }
}

describe('buildIterationRegionGraph', () => {
  it('constrains members to their frame and counts them for the badge', () => {
    const { nodes } = regionGraph()
    const { nodes: presented } = buildIterationRegionGraph(nodes, [])

    const frame = presented.find((candidate) => candidate.id === 'iteration-1')
    const member = presented.find((candidate) => candidate.id === 'agent-1')

    expect(frame?.data.regionMemberCount).toBe(1)
    expect(member?.extent).toBe('parent')
    expect(member?.expandParent).toBeUndefined()
  })

  it('leaves ancestors clear of the region constraints', () => {
    const { nodes } = regionGraph()
    const { nodes: presented } = buildIterationRegionGraph(nodes, [])

    const root = presented.find((candidate) => candidate.id === 'output-1')
    expect(root?.extent).toBeUndefined()
    expect(root?.zIndex).toBe(1)
  })

  it('orders frames before their members so React Flow lays children in', () => {
    const { nodes } = regionGraph()
    const { nodes: presented } = buildIterationRegionGraph(nodes, [])

    const order = presented.map((candidate) => candidate.id)
    expect(order.indexOf('iteration-1')).toBeLessThan(order.indexOf('agent-1'))
  })

  it('stacks frames below ordinary cards, selected cards above everything', () => {
    const selected = node('agent-2', 'agent', { selected: true })
    const { nodes } = regionGraph()
    const { nodes: presented } = buildIterationRegionGraph([...nodes, selected], [])

    const frame = presented.find((candidate) => candidate.id === 'iteration-1')
    const card = presented.find((candidate) => candidate.id === 'output-1')
    const chosen = presented.find((candidate) => candidate.id === 'agent-2')

    expect(frame?.zIndex).toBe(0)
    expect(card?.zIndex).toBe(1)
    expect(chosen?.zIndex).toBe(10)
  })

  it('hides members of a folded frame and their incident edges', () => {
    const { nodes, edges } = regionGraph({ collapsed: true })
    const { nodes: presented, edges: presentedEdges } = buildIterationRegionGraph(nodes, edges)

    const member = presented.find((candidate) => candidate.id === 'agent-1')
    expect(member?.hidden).toBe(true)

    // Every edge touching the folded region is hidden in the canvas projection:
    // into it, inside it, and out of it.
    for (const edge of presentedEdges) {
      expect(edge.hidden).toBe(true)
    }
  })

  it('drops irrelevant presentation when folding state changes', () => {
    const first = buildIterationRegionGraph(regionGraph().nodes, [])
    // The collapsed flag is authored directly by the fixture: rebuilding the
    // graph rather than spreading each node keeps the change without a map.
    const folded = buildIterationRegionGraph(regionGraph({ collapsed: true }).nodes, [])

    const unfoldedMember = first.nodes.find((candidate) => candidate.id === 'agent-1')
    const foldedMember = folded.nodes.find((candidate) => candidate.id === 'agent-1')

    expect(unfoldedMember?.hidden).toBeUndefined()
    expect(foldedMember?.hidden).toBe(true)
  })

  it('gives loop children expandParent so their container grows with them', () => {
    const loop = node('loop-1', 'loop', {
      position: { x: 0, y: 0 },
      initialWidth: 620,
    })
    const loopMember = node('loop-1-agent', 'agent', {
      parentId: 'loop-1',
      containerId: 'loop-1',
    })
    const { nodes: presented } = buildIterationRegionGraph(
      [...regionGraph().nodes, loop, loopMember],
      [],
    )

    const member = presented.find((candidate) => candidate.id === 'loop-1-agent')
    expect(member?.extent).toBe('parent')
    expect(member?.expandParent).toBe(true)
  })
})

describe('workflowPointInsideIterationFrame', () => {
  it('answers yes for a point inside the persisted expanded box', () => {
    const frame = node('iteration-1', 'iteration', { position: { x: 0, y: 0 } })
    const inside = workflowPointInsideIterationFrame([frame], {
      x: WORKFLOW_ITERATION_NODE_WIDTH - 1,
      y: WORKFLOW_ITERATION_NODE_HEIGHT - 1,
    })
    expect(inside).toBe(true)
  })

  it('answers no for a point beyond the frame edges', () => {
    const frame = node('iteration-1', 'iteration', { position: { x: 0, y: 0 } })
    const outside = workflowPointInsideIterationFrame([frame], {
      x: WORKFLOW_ITERATION_NODE_WIDTH + 1,
      y: WORKFLOW_ITERATION_NODE_HEIGHT + 1,
    })
    expect(outside).toBe(false)
  })

  it('uses the folded box when the frame is collapsed', () => {
    const frame = node('iteration-1', 'iteration', {
      position: { x: 0, y: 0 },
      collapsed: true,
    })
    const inside = workflowPointInsideIterationFrame([frame], {
      x: WORKFLOW_ITERATION_COLLAPSED_WIDTH - 1,
      y: WORKFLOW_ITERATION_COLLAPSED_HEIGHT - 1,
    })
    expect(inside).toBe(true)
  })

  it('counts the measured Loop box, which can exceed its authored size, as inside', () => {
    // A new Loop is authored 620×300 but draws taller to hold its starter agent.
    const loop = { ...node('loop-1', 'loop'), initialWidth: 620, initialHeight: 300 }
    const measured = { ...loop, measured: { width: 628, height: 363 } }
    const belowAuthoredEdge = { x: 100, y: 340 }

    expect(workflowPointInsideIterationFrame([loop], belowAuthoredEdge)).toBe(false)
    expect(workflowPointInsideIterationFrame([measured], belowAuthoredEdge)).toBe(true)
    expect(workflowPointInsideIterationFrame([measured], { x: 100, y: 370 })).toBe(false)
  })

  it('counts the strip the frame draws beyond a member it was pinned to as inside', () => {
    const loop = {
      ...node('loop-1', 'loop'),
      initialWidth: 700,
      initialHeight: 300,
      width: 700,
      measured: { width: 700, height: 300 },
    }
    const member = {
      ...node('wide', 'agent', {
        position: { x: 470, y: 145 },
        parentId: 'loop-1',
        containerId: 'loop-1',
      }),
      measured: { width: 230, height: 98 },
    }
    expect(workflowPointInsideIterationFrame([loop, member], { x: 720, y: 150 })).toBe(true)
  })

  it('ignores non-frame nodes even when they carry coordinates', () => {
    const card = node('agent-1', 'agent', { position: { x: 0, y: 0 } })
    expect(workflowPointInsideIterationFrame([card], { x: 10, y: 10 })).toBe(false)
  })
})

it('projects legacy condition edges onto else without mutating the draft', () => {
  const nodes = [node('c', 'condition'), node('a', 'agent')]
  const edges = [{ id: 'old', source: 'c', target: 'a' }]
  expect(buildIterationRegionGraph(nodes, edges).edges[0]?.sourceHandle).toBe('else')
  expect(edges[0]).not.toHaveProperty('sourceHandle')
})
