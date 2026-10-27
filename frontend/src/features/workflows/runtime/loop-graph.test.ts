import { describe, expect, it } from 'vitest'
import type {
  WorkflowIterationGraph,
  WorkflowIterationNode,
} from '@/features/workflows/runtime/iteration-graph'
import {
  insertLoopMember,
  loopAuthoredSize,
  loopContentBox,
  loopExitInsertion,
  loopFrameSize,
  loopOutputInsertion,
} from '@/features/workflows/runtime/loop-graph'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** A node at a position; body members get both ownership keys like the factory writes. */
function node(
  id: string,
  kind: WorkflowNodeKind,
  position: { x: number; y: number },
  loopId?: string,
): WorkflowIterationNode {
  return {
    id,
    position,
    ...(loopId === undefined ? {} : { parentId: loopId }),
    data: { kind, ...(loopId === undefined ? {} : { containerId: loopId }) },
  }
}

/** The palette's default Loop group: a 620×300 Loop, its body start and one agent. */
function loopGraph(extra: WorkflowIterationNode[] = []): WorkflowIterationGraph {
  return {
    nodes: [
      { ...node('loop-1', 'loop', { x: 400, y: 300 }), initialWidth: 620, initialHeight: 300 },
      node('loop-1-start', 'start', { x: 40, y: 145 }, 'loop-1'),
      node('loop-1-agent', 'agent', { x: 350, y: 145 }, 'loop-1'),
      node('outer', 'agent', { x: 0, y: 0 }),
      ...extra,
    ],
    edges: [{ id: 'e-start-agent', source: 'loop-1-start', target: 'loop-1-agent' }],
  }
}

describe('loopOutputInsertion', () => {
  it('offers an append seam on a body member, keeping the port', () => {
    expect(loopOutputInsertion(loopGraph(), 'loop-1-agent', 'case-1')).toEqual({
      type: 'loop-output',
      loopId: 'loop-1',
      sourceId: 'loop-1-agent',
      sourceHandle: 'case-1',
    })
  })

  it('resolves the owner through parentId when containerId is absent', () => {
    const graph = loopGraph([
      { id: 'legacy', position: { x: 0, y: 0 }, parentId: 'loop-1', data: { kind: 'agent' } },
    ])
    expect(loopOutputInsertion(graph, 'legacy', null)?.loopId).toBe('loop-1')
  })

  it.each([
    ['a top-level node', 'outer'],
    ['a node that is not in the graph', 'missing'],
  ])('offers no seam on %s', (_name, sourceId) => {
    expect(loopOutputInsertion(loopGraph(), sourceId, null)).toBeNull()
  })

  it.each(['loopExit', 'output'] as const)(
    'offers no seam after a %s, which has no port',
    (kind) => {
      const graph = loopGraph([node('terminal', kind, { x: 700, y: 145 }, 'loop-1')])
      expect(loopOutputInsertion(graph, 'terminal', null)).toBeNull()
    },
  )

  it('offers no seam inside an iteration region', () => {
    const graph = loopGraph([
      node('iter', 'iteration', { x: 0, y: 600 }),
      { id: 'member', position: { x: 0, y: 0 }, parentId: 'iter', data: { kind: 'agent' } },
    ])
    expect(loopOutputInsertion(graph, 'member', null)).toBeNull()
  })
})

describe('insertLoopMember', () => {
  const exit = node('loopExit-1', 'loopExit', { x: 0, y: 0 })

  it('places the member one column right of its source, owned by the loop on both keys', () => {
    const insertion = loopOutputInsertion(loopGraph(), 'loop-1-agent', null)
    if (insertion === null) {
      throw new Error('expected a seam')
    }
    const next = insertLoopMember(loopGraph(), insertion, exit)
    const added = next.nodes.find((candidate) => candidate.id === 'loopExit-1')

    expect(added).toEqual({
      id: 'loopExit-1',
      position: { x: 680, y: 145 },
      parentId: 'loop-1',
      data: { kind: 'loopExit', containerId: 'loop-1' },
    })
    expect(next.edges.at(-1)).toEqual({
      id: 'edge-1',
      source: 'loop-1-agent',
      target: 'loopExit-1',
      type: 'workflow',
    })
  })

  it('grows the loop to contain the new member and never shrinks it', () => {
    const insertion = loopOutputInsertion(loopGraph(), 'loop-1-agent', null)
    if (insertion === null) {
      throw new Error('expected a seam')
    }
    const loop = insertLoopMember(loopGraph(), insertion, exit).nodes.find(
      (candidate) => candidate.id === 'loop-1',
    )
    // 680 + 230 card + 48 padding = 958 → 960 on the grid; 145 + 98 + 40 stays under 300.
    expect(loop === undefined ? undefined : loopFrameSize(loop, [])).toEqual({
      width: 960,
      height: 300,
    })
  })

  it('stacks a second branch from the same port below the first instead of overlapping it', () => {
    const graph = loopGraph([node('first', 'agent', { x: 680, y: 145 }, 'loop-1')])
    const next = insertLoopMember(
      graph,
      { type: 'loop-output', loopId: 'loop-1', sourceId: 'loop-1-agent', sourceHandle: 'else' },
      exit,
    )
    const added = next.nodes.find((candidate) => candidate.id === 'loopExit-1')

    expect(added?.position).toEqual({ x: 680, y: 295 })
    expect(next.edges.at(-1)).toMatchObject({ source: 'loop-1-agent', sourceHandle: 'else' })
  })

  it('never persists canvas-only constraint fields on the new member', () => {
    const insertion = loopOutputInsertion(loopGraph(), 'loop-1-agent', null)
    if (insertion === null) {
      throw new Error('expected a seam')
    }
    const next = insertLoopMember(loopGraph(), insertion, {
      ...exit,
      extent: 'parent',
      expandParent: true,
    })
    const added = next.nodes.find((candidate) => candidate.id === 'loopExit-1')
    expect(added).not.toHaveProperty('extent')
    expect(added).not.toHaveProperty('expandParent')
  })

  it('returns the graph unchanged when the source is not in the named loop', () => {
    const graph = loopGraph()
    expect(
      insertLoopMember(graph, { type: 'loop-output', loopId: 'loop-1', sourceId: 'outer' }, exit),
    ).toBe(graph)
    expect(
      insertLoopMember(
        graph,
        { type: 'loop-output', loopId: 'other', sourceId: 'loop-1-agent' },
        exit,
      ),
    ).toBe(graph)
  })
})

describe('loopExitInsertion', () => {
  it('appends after the right-most body member that has a port', () => {
    const graph = loopGraph([node('far-exit', 'loopExit', { x: 900, y: 145 }, 'loop-1')])
    expect(loopExitInsertion(graph, 'loop-1')).toEqual({
      type: 'loop-output',
      loopId: 'loop-1',
      sourceId: 'loop-1-agent',
      sourceHandle: null,
    })
  })

  it('skips a Condition, whose branches the action cannot choose between', () => {
    const graph = loopGraph([node('branch', 'condition', { x: 700, y: 145 }, 'loop-1')])
    expect(loopExitInsertion(graph, 'loop-1')?.sourceId).toBe('loop-1-agent')
  })

  it('never appends after the body start, which would end every round before any work', () => {
    const graph: WorkflowIterationGraph = {
      nodes: [
        node('loop-1', 'loop', { x: 0, y: 0 }),
        node('entry', 'start', { x: 40, y: 145 }, 'loop-1'),
        node('branch', 'condition', { x: 350, y: 145 }, 'loop-1'),
      ],
      edges: [],
    }
    expect(loopExitInsertion(graph, 'loop-1')).toBeNull()
  })

  it('returns null for a loop with no body', () => {
    expect(loopExitInsertion(loopGraph(), 'outer')).toBeNull()
  })
})

describe('loopAuthoredSize', () => {
  it('ignores the measured and runtime sizes, so a frame drawn from it can shrink', () => {
    const loop = {
      ...node('loop', 'loop', { x: 0, y: 0 }),
      initialWidth: 620,
      initialHeight: 300,
      width: 900,
      measured: { width: 900, height: 500 },
    }
    expect(loopAuthoredSize(loop)).toEqual({ width: 620, height: 300 })
  })
})

describe('loopFrameSize', () => {
  it('widens the authored box to what React Flow measured, never narrows it', () => {
    const loop = { ...node('loop', 'loop', { x: 0, y: 0 }), initialWidth: 620, initialHeight: 300 }
    expect(loopFrameSize({ ...loop, measured: { width: 628, height: 363 } }, [])).toEqual({
      width: 628,
      height: 363,
    })
    expect(loopFrameSize({ ...loop, measured: { width: 500, height: 200 } }, [])).toEqual({
      width: 620,
      height: 300,
    })
  })

  it('includes the padding the frame draws beyond a member React Flow pinned its box to', () => {
    // After an expandParent in this session React Flow pins the Loop to the member's edge (700),
    // while the frame draws that edge plus its 48px padding.
    const loop = {
      ...node('loop-1', 'loop', { x: 0, y: 0 }),
      initialWidth: 700,
      initialHeight: 300,
      width: 700,
      measured: { width: 700, height: 300 },
    }
    const member = {
      ...node('wide', 'agent', { x: 470, y: 145 }, 'loop-1'),
      measured: { width: 230, height: 98 },
    }
    expect(loopFrameSize(loop, [loop, member]).width).toBe(748)
  })

  it('defaults a loop without an authored size to the palette size', () => {
    expect(loopFrameSize(node('loop', 'loop', { x: 0, y: 0 }), [])).toEqual({
      width: 620,
      height: 300,
    })
  })
})

describe('loopContentBox', () => {
  it('needs room for every measured member plus the insertion padding', () => {
    const agent = {
      ...node('a', 'agent', { x: 350, y: 145 }, 'loop-1'),
      measured: { width: 230, height: 178 },
    }
    const nodes = [...loopGraph().nodes.filter((item) => item.id !== 'loop-1-agent'), agent]
    // 350 + 230 + 48 = 628 wide; 145 + 178 + 40 = 363 tall — taller than the authored 300.
    expect(loopContentBox(nodes, 'loop-1')).toEqual({ width: 628, height: 363 })
  })

  it('ignores nodes outside the loop', () => {
    const outer = {
      ...node('far', 'agent', { x: 5000, y: 5000 }),
      measured: { width: 1, height: 1 },
    }
    expect(loopContentBox([outer], 'loop-1')).toEqual({ width: 0, height: 0 })
  })
})
