import { screen } from '@testing-library/react'
import { ReactFlow } from '@xyflow/react'
import { beforeEach, describe, expect, it } from 'vitest'
import { WorkflowNodeCard } from '@/features/workflows/editor/workflow-node-card'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'
import { renderWithProviders } from '@/test/render'
import { stubViewportGeometry } from '@/test/viewport'

const NODE_TYPES = { workflow: WorkflowNodeCard }

/** The real React Flow canvas registers each rendered port under this authored node. */
function canvas(nodes: WorkflowCanvasNode[]) {
  return <ReactFlow nodes={nodes} edges={[]} nodeTypes={NODE_TYPES} />
}

/** A node whose case ids intentionally differ from their displayed ordinal labels. */
function condition(ids: string[]): WorkflowCanvasNode {
  return {
    id: 'condition-1',
    type: 'workflow',
    position: { x: 0, y: 0 },
    data: {
      kind: 'condition',
      title: '条件 1',
      description: '',
      cases: ids.map((id) => ({ id, conditions: [] })),
    },
  }
}

beforeEach(() => stubViewportGeometry())

describe('Condition canvas ports', () => {
  it('renders the actual case ids and implicit ELSE, refreshing ports after a case edit', async () => {
    const view = renderWithProviders(canvas([condition(['approved', 'retry'])]))
    const first = await screen.findByLabelText('从条件 1的 IF 分支连接')
    expect(first).toHaveAttribute('data-handleid', 'approved')
    expect(screen.getByLabelText('从条件 1的 ELIF 1 分支连接')).toHaveAttribute(
      'data-handleid',
      'retry',
    )
    expect(screen.getByLabelText('从条件 1的 ELSE 分支连接')).toHaveAttribute(
      'data-handleid',
      'else',
    )
    view.rerender(canvas([condition(['approved', 'fallback', 'stop'])]))
    expect(await screen.findByLabelText('从条件 1的 ELIF 2 分支连接')).toHaveAttribute(
      'data-handleid',
      'stop',
    )
    expect(document.querySelector('[data-handleid="retry"]')).toBeNull()
  })

  it('keeps ELSE available when a stored Condition has no cases', async () => {
    renderWithProviders(canvas([condition([])]))
    expect(await screen.findByLabelText('从条件 1的 ELSE 分支连接')).toHaveAttribute(
      'data-handleid',
      'else',
    )
    expect(screen.queryByText('IF')).toBeNull()
  })
})
