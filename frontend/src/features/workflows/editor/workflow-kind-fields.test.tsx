import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { createDefaultWorkflowCapabilities } from '@/features/workflows/runtime/capabilities'
import type {
  WorkflowConditionCase,
  WorkflowGlobalVariable,
  WorkflowIterationConfig,
  WorkflowLoopConfig,
  WorkflowNodeData,
} from '@/features/workflows/runtime/types'
import type { WorkflowDecoratedVariable } from '@/features/workflows/runtime/variable-catalog'
import { ConditionNodeFields } from '@/features/workflows/editor/workflow-condition-fields'
import {
  HumanNodeFields,
  JunctionNodeFields,
  LoopExitNodeFields,
  SubflowNodeFields,
  ToolNodeFields,
} from '@/features/workflows/editor/workflow-kind-fields'
import { LoopNodeFields } from '@/features/workflows/editor/workflow-loop-fields'
import { IterationNodeFields } from '@/features/workflows/editor/workflow-iteration-fields'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'
import { renderWithProviders } from '@/test/render'

/**
 * The variable pool a downstream node may select from, globals first.
 *
 * A catalog entry names its producer so the pickers can render `node / {x} name`
 * the way Dify does; those titles are what the assertions below lean on.
 */
const CATALOG: readonly WorkflowDecoratedVariable[] = [
  {
    selector: ['sys', 'workflow_id'],
    sourceNodeId: 'sys',
    variableName: 'workflow_id',
    valueType: 'string',
    scope: 'global',
  },
  {
    selector: ['start-1', 'input'],
    sourceNodeId: 'start-1',
    variableName: 'input',
    valueType: 'string',
    scope: 'node',
    sourceNodeTitle: '开始 1',
    sourceNodeKind: 'start',
  },
  {
    selector: ['agent-1', 'output'],
    sourceNodeId: 'agent-1',
    variableName: 'output',
    valueType: 'string',
    scope: 'node',
    sourceNodeTitle: 'Agent 1',
    sourceNodeKind: 'agent',
  },
  {
    selector: ['iteration-1', 'output'],
    sourceNodeId: 'iteration-1',
    variableName: 'output',
    valueType: 'array[string]',
    scope: 'node',
    sourceNodeTitle: '迭代 1',
    sourceNodeKind: 'iteration',
  },
  {
    selector: ['agent-2', 'output'],
    sourceNodeId: 'agent-2',
    variableName: 'output',
    valueType: 'string',
    scope: 'node',
    sourceNodeTitle: '区域内 Agent',
    sourceNodeKind: 'agent',
  },
]

/** A whole-graph around one Iteration frame with a single region member. */
const GRAPH_NODES: WorkflowCanvasNode[] = [
  {
    id: 'iteration-1',
    type: 'workflow',
    position: { x: 0, y: 0 },
    data: { kind: 'iteration', title: '迭代 1', description: '' },
  },
  {
    id: 'agent-2',
    parentId: 'iteration-1',
    type: 'workflow',
    position: { x: 0, y: 160 },
    data: {
      kind: 'agent',
      title: '区域内 Agent',
      description: '',
      agentConfig: createDefaultWorkflowCapabilities().defaultAgentConfig,
    },
  },
]

/** The system globals, declared in the graph and normalized before they seed a catalog. */
const GLOBAL_VARIABLES: readonly WorkflowGlobalVariable[] = [
  { name: 'sys.workflow_id', valueType: 'string' },
  { name: 'sys.timestamp', valueType: 'number' },
]

/** One authored condition case with a single comparison, shared by several tests. */
const AUTHORED_CASE: WorkflowConditionCase = {
  id: 'case-1',
  logic: 'and',
  conditions: [{ variableSelector: ['agent-1', 'output'], operator: 'equals', value: 'ok' }],
}

/** Builds a node panel payload for exercised kinds. */
function nodeData(patch: Partial<WorkflowNodeData>): WorkflowNodeData {
  return { kind: 'condition', title: '节点', description: '', ...patch }
}

/** A mock `onChange` that accumulates every published edit under `.data`. */
function captureOnChange(): { data: WorkflowNodeData; onChange: (next: WorkflowNodeData) => void } {
  // Each panel publishes the whole next node, so a complete seed keeps the
  // accumulator a real WorkflowNodeData without an unsafe cast.
  const captured: WorkflowNodeData = { kind: 'condition', title: '', description: '' }
  const onChange = vi.fn<(data: WorkflowNodeData) => void>((next: WorkflowNodeData) => {
    Object.assign(captured, next)
  })
  return { data: captured, onChange }
}

describe('ConditionNodeFields', () => {
  it('renders an authored case, its localized operator and its comparison value', () => {
    const onChange = vi.fn<(data: WorkflowNodeData) => void>()
    renderWithProviders(
      <ConditionNodeFields
        data={nodeData({ cases: [AUTHORED_CASE] })}
        onChange={onChange}
        catalog={CATALOG}
      />,
    )

    expect(screen.getByText('IF')).toBeInTheDocument()
    expect(screen.getByText('Agent 1')).toBeInTheDocument()
    expect(screen.getByText('等于')).toBeInTheDocument()
    expect(screen.getByRole('textbox', { name: '值 1' })).toHaveValue('ok')
    expect(screen.getByText('添加条件')).toBeInTheDocument()
  })

  it('adds a comparison row to an empty IF card', () => {
    const { data, onChange } = captureOnChange()
    const view = renderWithProviders(
      <ConditionNodeFields data={nodeData({})} onChange={onChange} catalog={CATALOG} />,
    )

    fireEvent.click(screen.getByRole('button', { name: '添加条件' }))

    expect(data.cases).toHaveLength(1)
    expect(data.cases?.[0]?.conditions).toHaveLength(1)
    view.rerender(<ConditionNodeFields data={data} onChange={onChange} catalog={CATALOG} />)
    expect(screen.getByRole('textbox', { name: '值 1' })).toBeInTheDocument()
  })

  it('appends an ELIF branch and keeps a fresh sequence id', () => {
    const { data, onChange } = captureOnChange()
    renderWithProviders(
      <ConditionNodeFields
        data={nodeData({ cases: [AUTHORED_CASE] })}
        onChange={onChange}
        catalog={CATALOG}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: 'ELIF' }))

    expect(data.cases).toHaveLength(2)
    expect(data.cases?.[1]).toEqual({ id: 'case-2', logic: 'and', conditions: [] })
  })

  it('removes an ELIF branch together with its rows', () => {
    const { data, onChange } = captureOnChange()
    renderWithProviders(
      <ConditionNodeFields
        data={nodeData({ cases: [AUTHORED_CASE, { id: 'case-2', logic: 'or', conditions: [] }] })}
        onChange={onChange}
        catalog={CATALOG}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '移除分支' }))

    expect(data.cases).toHaveLength(1)
    expect(data.cases?.[0]?.id).toBe('case-1')
  })

  it('removes a single comparison row', () => {
    const { data, onChange } = captureOnChange()
    renderWithProviders(
      <ConditionNodeFields
        data={nodeData({
          cases: [
            {
              id: 'case-1',
              logic: 'and',
              conditions: [
                { variableSelector: ['agent-1', 'output'], operator: 'equals', value: 'ok' },
                { variableSelector: ['start-1', 'input'], operator: 'empty' },
              ],
            },
          ],
        })}
        onChange={onChange}
        catalog={CATALOG}
      />,
    )

    // Every comparison row shares the same remove label, so the second button is
    // the row this test removes.
    const removeButtons = screen.getAllByRole('button', { name: '移除条件' })
    const secondRemove = removeButtons[1]
    if (secondRemove !== undefined) {
      fireEvent.click(secondRemove)
    }

    expect(data.cases?.[0]?.conditions).toHaveLength(1)
    expect(data.cases?.[0]?.conditions?.[0]?.operator).toBe('equals')
  })

  it('coerces a comparison value as it is typed', () => {
    const { data, onChange } = captureOnChange()
    renderWithProviders(
      <ConditionNodeFields
        data={nodeData({ cases: [AUTHORED_CASE] })}
        onChange={onChange}
        catalog={CATALOG}
      />,
    )

    fireEvent.change(screen.getByRole('textbox', { name: '值 1' }), {
      target: { value: '42' },
    })

    expect(data.cases?.[0]?.conditions?.[0]?.value).toBe(42)
  })
})

describe('ToolNodeFields', () => {
  it('renders the selected tool and operation, then edits a parameter', () => {
    const { data, onChange } = captureOnChange()
    const view = renderWithProviders(
      <ToolNodeFields
        data={nodeData({ kind: 'tool', tool: 'Terminal', operation: 'run_command' })}
        onChange={onChange}
      />,
    )

    expect(screen.getByText('Terminal')).toBeInTheDocument()
    expect(screen.getByText('执行命令')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '添加参数' })).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '添加参数' }))
    expect(data.toolParameters).toEqual([{ key: '', value: '' }])

    view.rerender(<ToolNodeFields data={data} onChange={onChange} />)
    fireEvent.change(screen.getByRole('textbox', { name: '参数名 1' }), {
      target: { value: 'repo' },
    })
    expect(data.toolParameters?.[0]?.key).toBe('repo')

    view.rerender(<ToolNodeFields data={data} onChange={onChange} />)
    fireEvent.change(screen.getByRole('textbox', { name: '参数值 1' }), {
      target: { value: 'mor' },
    })
    expect(data.toolParameters?.[0]?.value).toBe('mor')

    view.rerender(<ToolNodeFields data={data} onChange={onChange} />)
    fireEvent.click(screen.getByRole('button', { name: '移除参数 1' }))
    view.rerender(<ToolNodeFields data={data} onChange={onChange} />)
    expect(screen.getByRole('button', { name: '添加参数' })).toBeInTheDocument()
  })
})

describe('JunctionNodeFields', () => {
  it('shows the wait-count field only for a count wait strategy', () => {
    const onChange = vi.fn<(data: WorkflowNodeData) => void>()
    const view = renderWithProviders(
      <JunctionNodeFields data={nodeData({ kind: 'junction' })} onChange={onChange} />,
    )

    expect(screen.getByText('全部分支完成')).toBeInTheDocument()
    expect(screen.getByText('任一失败即失败')).toBeInTheDocument()
    expect(screen.queryByLabelText('完成数量')).not.toBeInTheDocument()

    view.rerender(
      <JunctionNodeFields
        data={nodeData({ kind: 'junction', waitStrategy: 'count', waitCount: 2 })}
        onChange={onChange}
      />,
    )
    expect(screen.getByLabelText('完成数量')).toHaveValue(2)
  })
})

describe('HumanNodeFields', () => {
  it('records the approval prompt as it is typed', () => {
    const { data, onChange } = captureOnChange()
    renderWithProviders(<HumanNodeFields data={nodeData({ kind: 'human' })} onChange={onChange} />)

    fireEvent.change(screen.getByRole('textbox', { name: '审批说明' }), {
      target: { value: '请确认发布到生产。' },
    })

    expect(data.instruction).toBe('请确认发布到生产。')
  })
})

describe('LoopNodeFields', () => {
  /** The graph props the Loop panels need; the selector catalog derives from them. */
  const LOOP_PROPS = {
    nodeId: 'loop-1',
    graphNodes: [],
    edges: [],
    globalVariables: [],
  } as const
  const LOOP_CONFIG: WorkflowLoopConfig = {
    maxIterations: 3,
    variables: [
      {
        name: 'value',
        valueType: 'string',
        initial: { kind: 'constant', value: '0' },
        feedback: [],
      },
    ],
    until: { logic: 'and', conditions: [] },
    outputs: [],
  }

  /** Renders the panel with the shared config; `rerenderPanel` rebinds it to the captured node. */
  function setUpLoopPanel() {
    const { data, onChange } = captureOnChange()
    const view = renderWithProviders(
      <LoopNodeFields
        {...LOOP_PROPS}
        data={nodeData({ kind: 'loop', loopConfig: LOOP_CONFIG })}
        onChange={onChange}
      />,
    )
    const rerenderPanel = (): void => {
      view.rerender(<LoopNodeFields {...LOOP_PROPS} data={data} onChange={onChange} />)
    }
    return { data, rerenderPanel }
  }

  /** A body member of loop-1, owned through both keys as the factory writes them. */
  function bodyNode(id: string, kind: WorkflowNodeData['kind'], x: number): WorkflowCanvasNode {
    return {
      id,
      type: 'workflow',
      position: { x, y: 145 },
      parentId: 'loop-1',
      data: { ...nodeData({ kind }), containerId: 'loop-1' },
    }
  }

  /** Renders the panel over a body made of the given members, with the exit action wired. */
  function renderWithBody(members: WorkflowCanvasNode[]) {
    const onAddExit = vi.fn<() => void>()
    renderWithProviders(
      <LoopNodeFields
        {...LOOP_PROPS}
        graphNodes={[
          {
            id: 'loop-1',
            type: 'workflow',
            position: { x: 0, y: 0 },
            data: nodeData({ kind: 'loop' }),
          },
          ...members,
        ]}
        data={nodeData({ kind: 'loop', loopConfig: LOOP_CONFIG })}
        onChange={vi.fn<(data: WorkflowNodeData) => void>()}
        onAddExit={onAddExit}
      />,
    )
    return { onAddExit, button: screen.getByRole('button', { name: '添加早退节点' }) }
  }

  it('offers the early exit after a body step that does round work', () => {
    const { onAddExit, button } = renderWithBody([
      bodyNode('entry', 'start', 40),
      bodyNode('worker', 'agent', 350),
    ])

    expect(button).toBeEnabled()
    fireEvent.click(button)
    expect(onAddExit).toHaveBeenCalledOnce()
  })

  it('withholds the early exit when only the body start and conditions could precede it', () => {
    const { button } = renderWithBody([
      bodyNode('entry', 'start', 40),
      bodyNode('branch', 'condition', 350),
    ])

    // After Start the exit would end every round at once; after a Condition it would take ELSE.
    expect(button).toBeDisabled()
    expect(screen.getByText(/请点那个分支端口的 \+ 添加/)).toBeInTheDocument()
  })

  it('asks for a first step when the body has nothing an exit could follow', () => {
    const { button } = renderWithBody([bodyNode('entry', 'start', 40)])

    expect(button).toBeDisabled()
    expect(screen.getByText('先在循环体里添加一个步骤，早退会接在它后面。')).toBeInTheDocument()
  })

  it('clamps the round ceiling and writes a variable first-round value', () => {
    const { data, rerenderPanel } = setUpLoopPanel()

    expect(screen.getByLabelText('轮次耗尽时')).toHaveTextContent('失败')
    fireEvent.change(screen.getByLabelText('最大轮次'), { target: { value: '55' } })
    expect(data.loopConfig?.maxIterations).toBe(55)

    rerenderPanel()
    fireEvent.change(screen.getByLabelText('初值 1'), { target: { value: 'ready' } })
    expect(data.loopConfig?.variables[0]?.initial).toEqual({ kind: 'constant', value: 'ready' })
  })

  it('adds a carried variable and records its feedback selector', async () => {
    const user = userEvent.setup()
    const { data, rerenderPanel } = setUpLoopPanel()
    fireEvent.click(screen.getByRole('button', { name: '添加变量' }))
    expect(data.loopConfig?.variables).toHaveLength(2)

    rerenderPanel()
    fireEvent.change(screen.getByLabelText('变量名 2'), { target: { value: 'draft' } })
    rerenderPanel()
    await user.click(screen.getByLabelText('反馈 2'))
    await user.click(await screen.findByRole('option', { name: /value/ }))
    expect(data.loopConfig?.variables[1]).toMatchObject({
      name: 'draft',
      feedback: ['loop-1', 'value'],
    })

    fireEvent.click(screen.getByRole('button', { name: '删除变量 1' }))
    expect(data.loopConfig?.variables).toHaveLength(1)
    expect(data.loopConfig?.variables[0]?.name).toBe('draft')
  })

  it('adds a named output and picks its selector from the catalog', async () => {
    const user = userEvent.setup()
    const { data, rerenderPanel } = setUpLoopPanel()
    fireEvent.click(screen.getByRole('button', { name: '添加输出' }))
    expect(data.loopConfig?.outputs).toHaveLength(1)

    rerenderPanel()
    fireEvent.change(screen.getByLabelText('输出名 1'), { target: { value: 'result' } })
    rerenderPanel()
    await user.click(screen.getByLabelText('取值 1'))
    await user.click(await screen.findByRole('option', { name: /value/ }))
    expect(data.loopConfig?.outputs[0]).toEqual({
      name: 'result',
      variableSelector: ['loop-1', 'value'],
    })
  })

  it('adds a round-end rule and records its selector, operator and value', async () => {
    const user = userEvent.setup()
    const { data, rerenderPanel } = setUpLoopPanel()
    fireEvent.click(screen.getByRole('button', { name: '添加条件' }))
    expect(data.loopConfig?.until.conditions).toHaveLength(1)

    rerenderPanel()
    await user.click(screen.getByLabelText('变量 1'))
    await user.click(await screen.findByRole('option', { name: /value/ }))
    rerenderPanel()
    fireEvent.change(screen.getByLabelText('值 1'), { target: { value: 'done' } })
    expect(data.loopConfig?.until.conditions[0]).toEqual({
      variableSelector: ['loop-1', 'value'],
      operator: 'equals',
      value: 'done',
    })

    fireEvent.click(screen.getByRole('button', { name: '删除条件 1' }))
    expect(data.loopConfig?.until.conditions).toHaveLength(0)
  })

  it('keeps body and carried roots out of first-round variable initial choices', async () => {
    const user = userEvent.setup()
    const config: WorkflowLoopConfig = {
      ...LOOP_CONFIG,
      variables: [
        {
          name: 'value',
          valueType: 'string',
          initial: { kind: 'variable', selector: [] },
          feedback: [],
        },
      ],
    }
    const member: WorkflowCanvasNode = {
      id: 'body',
      type: 'workflow',
      parentId: 'loop-1',
      position: { x: 20, y: 30 },
      data: nodeData({ kind: 'agent', title: '内部产物' }),
    }
    renderWithProviders(
      <LoopNodeFields
        {...LOOP_PROPS}
        graphNodes={[member]}
        data={nodeData({ kind: 'loop', loopConfig: config })}
        onChange={vi.fn<(data: WorkflowNodeData) => void>()}
      />,
    )
    await user.click(screen.getByLabelText('初值 1'))
    expect(screen.queryByRole('option', { name: /内部产物/ })).not.toBeInTheDocument()
    expect(screen.queryByRole('option', { name: /value/ })).not.toBeInTheDocument()
    expect(await screen.findByRole('option', { name: /workflow_id/ })).toBeInTheDocument()
  })

  it('hides the config inputs for a legacy loop without executable config', () => {
    const onChange = vi.fn<(data: WorkflowNodeData) => void>()
    renderWithProviders(
      <LoopNodeFields {...LOOP_PROPS} data={nodeData({ kind: 'loop' })} onChange={onChange} />,
    )

    expect(screen.getByLabelText('最大轮次')).toBeDisabled()
    expect(screen.getByLabelText('轮次耗尽时')).toBeDisabled()
    expect(screen.queryByLabelText('变量名 1')).not.toBeInTheDocument()
    expect(screen.getByText('此旧循环节点缺少可执行配置，请删除后重新添加。')).toBeInTheDocument()
  })
})

describe('LoopExitNodeFields', () => {
  it('explains that the marker has no configuration of its own', () => {
    renderWithProviders(<LoopExitNodeFields />)

    expect(screen.getByText(/循环体内的终端节点/)).toBeInTheDocument()
  })
})

describe('IterationNodeFields', () => {
  const ITERATION_CONFIG: WorkflowIterationConfig = {
    iteratorSelector: ['iteration-1', 'output'],
    collectSelector: ['agent-2', 'output'],
    errorStrategy: 'fail',
    maxIterations: 50,
  }

  it('renders the config, region member choices and empty-state warnings', () => {
    const onChange = vi.fn<(data: WorkflowNodeData) => void>()
    renderWithProviders(
      <IterationNodeFields
        nodeId="iteration-1"
        data={nodeData({
          kind: 'iteration',
          iterationConfig: ITERATION_CONFIG,
        })}
        onChange={onChange}
        catalog={CATALOG}
        graphNodes={GRAPH_NODES}
        globalVariables={GLOBAL_VARIABLES}
      />,
    )

    const hint = screen.getByText(/对迭代源数组的每个元素/).closest('p')
    expect(hint).toBeInTheDocument()
    // The collect trigger resolves the selected region member with its node title.
    expect(screen.getByText('区域内 Agent')).toBeInTheDocument()
    expect(screen.getByText('任一轮失败即失败')).toBeInTheDocument()
    expect(screen.getByLabelText('最大轮次')).toHaveValue(50)
    // Configured selectors clear the empty-state warnings.
    expect(screen.queryByText('迭代源未设置')).not.toBeInTheDocument()
    expect(screen.queryByText('收集目标未设置')).not.toBeInTheDocument()
  })

  it('warns while the iterator or the collect target is still unset', () => {
    const onChange = vi.fn<(data: WorkflowNodeData) => void>()
    renderWithProviders(
      <IterationNodeFields
        nodeId="iteration-1"
        data={nodeData({ kind: 'iteration' })}
        onChange={onChange}
        catalog={CATALOG}
        graphNodes={GRAPH_NODES}
        globalVariables={GLOBAL_VARIABLES}
      />,
    )

    expect(screen.getByText('迭代源未设置')).toBeInTheDocument()
    expect(screen.getByText('收集目标未设置')).toBeInTheDocument()
  })
})

describe('SubflowNodeFields', () => {
  it('shows the placeholder explanation', () => {
    renderWithProviders(<SubflowNodeFields />)
    expect(screen.getByText(/子流程用于封装可复用的复杂业务步骤/)).toBeInTheDocument()
  })
})
