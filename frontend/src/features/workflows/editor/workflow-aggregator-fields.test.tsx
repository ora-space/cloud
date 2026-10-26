import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import { AggregatorNodeFields } from '@/features/workflows/editor/workflow-aggregator-fields'
import { renderWithProviders } from '@/test/render'

/** The only upstream root the picker offers: the workflow's Start input. */
const CATALOG: readonly WorkflowDecoratedVariable[] = [
  {
    selector: ['start-1', 'input'],
    sourceNodeId: 'start-1',
    variableName: 'input',
    valueType: 'string',
    scope: 'node',
    sourceNodeTitle: '开始 1',
  },
]

/** The node the panel edits; candidates are the only thing it reads. */
function aggregatorData(variables: string[][]): WorkflowNodeData {
  return {
    kind: 'aggregator',
    title: '变量聚合',
    description: '',
    aggregatorConfig: { variables },
  }
}

/**
 * Renders the panel and tracks the latest node it published.
 *
 * `published.data` is what a real editor would persist, so each assertion reads the shape the save
 * path would write instead of a captured mock call.
 */
function renderPanel(variables: string[][]) {
  const published = { data: aggregatorData(variables) }
  const onChange = (next: WorkflowNodeData): void => {
    published.data = next
  }
  const view = renderWithProviders(
    <AggregatorNodeFields data={published.data} onChange={onChange} catalog={CATALOG} />,
  )
  return { published, onChange, view }
}

describe('AggregatorNodeFields', () => {
  it('adds a candidate and picks its root from the catalog', async () => {
    const user = userEvent.setup()
    const { published, onChange, view } = renderPanel([])

    fireEvent.click(screen.getByRole('button', { name: '添加候选' }))
    expect(published.data.aggregatorConfig?.variables).toEqual([[]])

    view.rerender(
      <AggregatorNodeFields data={published.data} onChange={onChange} catalog={CATALOG} />,
    )
    await user.click(screen.getByLabelText('候选 1'))
    await user.click(await screen.findByRole('option', { name: /input/ }))
    expect(published.data.aggregatorConfig?.variables).toEqual([['start-1', 'input']])
  })

  it('moves a candidate up to change the published priority', () => {
    const { published } = renderPanel([
      ['a', 'output'],
      ['b', 'output'],
    ])

    fireEvent.click(screen.getByRole('button', { name: '上移候选 2' }))

    expect(published.data.aggregatorConfig?.variables).toEqual([
      ['b', 'output'],
      ['a', 'output'],
    ])
  })

  it('keeps the declared order when a candidate is removed', () => {
    const { published } = renderPanel([
      ['a', 'output'],
      ['b', 'output'],
    ])

    fireEvent.click(screen.getByRole('button', { name: '删除候选 1' }))

    expect(published.data.aggregatorConfig?.variables).toEqual([['b', 'output']])
  })
})
