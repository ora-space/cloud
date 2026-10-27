import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import {
  WorkflowCanvas,
  type WorkflowCanvasProps,
} from '@/features/workflows/editor/workflow-canvas'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'
import { renderWithProviders } from '@/test/render'
import { stubViewportGeometry } from '@/test/viewport'

/** One canvas node; body members carry both ownership keys, as the factory writes them. */
function node(
  id: string,
  kind: WorkflowCanvasNode['data']['kind'],
  position: { x: number; y: number },
  loopId?: string,
): WorkflowCanvasNode {
  return {
    id,
    type: 'workflow',
    position,
    ...(loopId === undefined ? {} : { parentId: loopId }),
    data: {
      kind,
      title: id,
      description: '',
      ...(loopId === undefined ? {} : { containerId: loopId }),
    },
  }
}

/** A Loop holding a body start and agent, next to one top-level agent. */
function loopGraph(): { nodes: WorkflowCanvasNode[]; edges: WorkflowCanvasEdge[] } {
  const loop = node('loop-1', 'loop', { x: 0, y: 0 })
  loop.initialWidth = 620
  loop.initialHeight = 300
  loop.data.loopConfig = {
    maxIterations: 5,
    variables: [],
    until: { logic: 'and', conditions: [] },
    outputs: [],
  }
  return {
    nodes: [
      loop,
      node('body-start', 'start', { x: 40, y: 145 }, 'loop-1'),
      node('body-agent', 'agent', { x: 350, y: 145 }, 'loop-1'),
      node('outer', 'agent', { x: 700, y: 0 }),
    ],
    edges: [{ id: 'e-body', source: 'body-start', target: 'body-agent', type: 'workflow' }],
  }
}

/** Renders the editable canvas with spies for the handlers these tests observe. */
function renderCanvas(graph = loopGraph()) {
  const handlers = {
    onNodesChange: vi.fn<WorkflowCanvasProps['onNodesChange']>(),
    onEdgesChange: vi.fn<WorkflowCanvasProps['onEdgesChange']>(),
    onConnect: vi.fn<WorkflowCanvasProps['onConnect']>(),
    onAddNode: vi.fn<WorkflowCanvasProps['onAddNode']>(),
    onOrganize: vi.fn<WorkflowCanvasProps['onOrganize']>(),
    onIterationInsert: vi.fn<NonNullable<WorkflowCanvasProps['onIterationInsert']>>(),
  }
  renderWithProviders(<WorkflowCanvas nodes={graph.nodes} edges={graph.edges} {...handlers} />)
  return handlers
}

/** The frame React Flow drew for one Loop node. */
function loopFrame(id: string): HTMLElement {
  const element = document.querySelector(
    `[data-workflow-loop-frame][data-workflow-node-id="${id}"]`,
  )
  if (!(element instanceof HTMLElement)) {
    throw new Error(`the canvas did not draw a loop frame for "${id}"`)
  }
  return element
}

beforeEach(() => {
  stubViewportGeometry()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('Loop container on the canvas', () => {
  it('draws the Loop as a frame at its authored size, with its round ceiling', () => {
    renderCanvas()

    const frame = loopFrame('loop-1')
    expect(frame).toHaveStyle({ width: '620px', height: '300px' })
    expect(within(frame).getByText('最多 5 轮')).toBeInTheDocument()
  })

  it('draws from the authored size and its members, not a size React Flow wrote back', () => {
    const graph = loopGraph()
    graph.nodes = graph.nodes.map((candidate) =>
      candidate.id === 'loop-1'
        ? { ...candidate, width: 900, height: 500, measured: { width: 900, height: 500 } }
        : candidate,
    )
    renderCanvas(graph)

    // A frame that read back its old size could only ever grow; this one returns to 620×300.
    expect(loopFrame('loop-1')).toHaveStyle({ width: '620px', height: '300px' })
  })

  it('offers the append menu on a body member port, but not on a top-level node', async () => {
    renderCanvas()

    expect(await screen.findByLabelText('在 body-agent 之后添加节点')).toBeInTheDocument()
    expect(screen.getByLabelText('在 body-start 之后添加节点')).toBeInTheDocument()
    expect(screen.queryByLabelText('在 outer 之后添加节点')).not.toBeInTheDocument()
  })

  it('lists the kinds a round may run, including the early exit and never an Output', async () => {
    const handlers = renderCanvas()

    fireEvent.click(await screen.findByLabelText('从body-agent连接'))
    const menu = await screen.findByRole('menu')
    const items = within(menu)
      .getAllByRole('menuitem')
      .map((item) => item.textContent)
    expect(items).toEqual(['Agent', '条件分支', '变量聚合', '早退'])

    fireEvent.click(within(menu).getByRole('menuitem', { name: '早退' }))
    expect(handlers.onIterationInsert).toHaveBeenCalledWith(
      { type: 'loop-output', loopId: 'loop-1', sourceId: 'body-agent', sourceHandle: null },
      'loopExit',
    )
  })

  it('offers no append menu on a read-only canvas', async () => {
    const graph = loopGraph()
    renderWithProviders(
      <WorkflowCanvas
        nodes={graph.nodes}
        edges={graph.edges}
        onNodesChange={vi.fn<WorkflowCanvasProps['onNodesChange']>()}
        onEdgesChange={vi.fn<WorkflowCanvasProps['onEdgesChange']>()}
        onConnect={vi.fn<WorkflowCanvasProps['onConnect']>()}
        onAddNode={vi.fn<WorkflowCanvasProps['onAddNode']>()}
        onOrganize={vi.fn<WorkflowCanvasProps['onOrganize']>()}
        readOnly
      />,
    )

    expect(await screen.findByLabelText('从body-agent连接')).toBeInTheDocument()
    expect(screen.queryByLabelText('在 body-agent 之后添加节点')).not.toBeInTheDocument()
  })

  it('deletes the body and its edges together with the Loop, without a confirmation', async () => {
    // The spies do not apply changes, so the selection the delete control needs is given up front.
    const graph = loopGraph()
    graph.nodes = graph.nodes.map((candidate) =>
      candidate.id === 'loop-1' ? { ...candidate, selected: true } : candidate,
    )
    const handlers = renderCanvas(graph)

    fireEvent.click(within(loopFrame('loop-1')).getByLabelText(/删除/))

    await waitFor(() => expect(handlers.onNodesChange).toHaveBeenCalled())
    const removed = handlers.onNodesChange.mock.calls
      .flatMap(([changes]) => changes)
      .flatMap((change) => (change.type === 'remove' ? [change.id] : []))
    expect(removed.toSorted()).toEqual(['body-agent', 'body-start', 'loop-1'])
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})
