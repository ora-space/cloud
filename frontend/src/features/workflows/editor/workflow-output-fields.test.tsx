import { useState } from 'react'
import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { OutputNodeFields } from '@/features/workflows/editor/workflow-output-fields'
import { useWorkflowDraft } from '@/features/workflows/editor/use-workflow-draft'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import { workflowFixture } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'

const CATALOG: readonly WorkflowDecoratedVariable[] = [
  {
    selector: ['start', 'input'],
    sourceNodeId: 'start',
    variableName: 'input',
    valueType: 'string',
    scope: 'node',
    sourceNodeTitle: '开始',
  },
]
const EMPTY: WorkflowNodeData = { kind: 'output', title: '输出', description: '' }

/** Exercises the real draft save/reload boundary after every controlled panel edit. */
function Editor({ onSave }: { onSave: (graph: Record<string, unknown>) => void }) {
  const draft = useWorkflowDraft(
    workflowFixture({
      graph: {
        nodes: [
          {
            id: 'out',
            type: 'workflow',
            position: { x: 0, y: 0 },
            data: EMPTY,
          },
        ],
        edges: [],
      },
    }),
    (key) => key,
  )
  const data = draft.nodes[0]?.data ?? EMPTY
  return (
    <>
      <OutputNodeFields
        data={data}
        onChange={(next) => draft.updateNodeData('out', next)}
        catalog={CATALOG}
      />
      <button
        onClick={() => {
          const graph = draft.snapshot({ x: 0, y: 0, zoom: 1 })
          draft.reset(workflowFixture({ graph }))
          onSave(graph)
        }}
      >
        保存并重载
      </button>
    </>
  )
}

describe('Output bindings', () => {
  it('persists a named catalog selection through save/reload and removes the exact row', async () => {
    const user = userEvent.setup()
    const saved: Record<string, unknown>[] = []
    renderWithProviders(<Editor onSave={(graph) => saved.push(graph)} />)
    await user.click(screen.getByRole('button', { name: '添加输出' }))
    fireEvent.change(screen.getByLabelText('输出名称 1'), { target: { value: 'summary' } })
    await user.click(screen.getByLabelText('输出来源 1'))
    await user.click(await screen.findByRole('option', { name: /input/ }))
    await user.click(screen.getByRole('button', { name: '保存并重载' }))
    expect(saved[0]).toMatchObject({
      nodes: [{ data: { outputs: [{ name: 'summary', variableSelector: ['start', 'input'] }] } }],
    })
    expect(screen.getByLabelText('输出名称 1')).toHaveValue('summary')
    expect(screen.getByLabelText('输出来源 1')).toHaveTextContent('input')
    await user.click(screen.getByRole('button', { name: '添加输出' }))
    fireEvent.change(screen.getByLabelText('输出名称 2'), { target: { value: 'second' } })
    await user.click(screen.getByRole('button', { name: '删除输出 1' }))
    expect(screen.getByLabelText('输出名称 1')).toHaveValue('second')
    await user.click(screen.getByRole('button', { name: '删除输出 1' }))
    await user.click(screen.getByRole('button', { name: '保存并重载' }))
    expect(saved[1]).toMatchObject({ nodes: [{ data: { outputs: [] } }] })
  })

  it('keeps imported nested selectors editable without silently replacing them', () => {
    function Imported() {
      const [data, setData] = useState<WorkflowNodeData>({
        ...EMPTY,
        outputs: [{ name: 'count', variableSelector: ['writer', 'structured_output', 'count'] }],
      })
      return <OutputNodeFields data={data} onChange={setData} catalog={[]} />
    }
    renderWithProviders(<Imported />)
    expect(screen.getByLabelText('输出来源 1')).toHaveTextContent('writer.structured_output.count')
    fireEvent.change(screen.getByLabelText('输出名称 1'), { target: { value: 'total' } })
    expect(screen.getByLabelText('输出来源 1')).toHaveTextContent('writer.structured_output.count')
  })
})
