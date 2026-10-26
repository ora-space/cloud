import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import type { RunNodeEntry } from '@/features/workflows/run/run-node-state'
import { RunInspector } from '@/features/workflows/run/run-inspector'
import { renderWithProviders } from '@/test/render'

/** A Tool node with an authored contract, so the config section has rows. */
const TOOL_DATA: WorkflowNodeData = {
  kind: 'tool',
  title: '执行命令',
  description: '跑测试套件',
  tool: 'run_command',
  operation: 'run',
  toolParameters: [{ key: 'cwd', value: '/repo' }],
}

/** The trace row a simulated run would persist for a finished node. */
const FINISHED_ENTRY: RunNodeEntry = {
  status: 'succeeded',
  startedAt: '2026-09-20T02:00:00.000Z',
  finishedAt: '2026-09-20T02:00:05.000Z',
  error: '命令退出码 2',
  output: { exit: 2, stderr: 'boom' },
}

describe('RunInspector', () => {
  it('shows one section for identity, config, execution and output', () => {
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={FINISHED_ENTRY} />)

    expect(screen.getByRole('heading', { name: '执行命令' })).toBeInTheDocument()
    const config = screen.getByRole('region', { name: '配置' })
    expect(within(config).getByText('run_command')).toBeInTheDocument()
    expect(within(config).getByText('cwd = /repo')).toBeInTheDocument()
    expect(screen.getByRole('region', { name: '执行信息' })).toBeInTheDocument()
    const output = screen.getByRole('region', { name: '输出' })
    expect(within(output).getByText(/"exit": 2/)).toBeInTheDocument()
  })

  it('states the attempt and the next retry instant for a retrying node', () => {
    const entry: RunNodeEntry = {
      status: 'retrying',
      attempt: 2,
      startedAt: '2026-09-20T02:00:00.000Z',
      retryAt: '2026-09-20T02:00:30.000Z',
      error: 'agent_failed',
    }
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={entry} />)

    const execution = screen.getByRole('region', { name: '执行信息' })
    expect(within(execution).getByText('第几次尝试')).toBeInTheDocument()
    expect(within(execution).getByText('2')).toBeInTheDocument()
    expect(within(execution).getByText('下次重试')).toBeInTheDocument()
  })

  it('reveals a trace error in a destructive block', () => {
    const entry: RunNodeEntry = { ...FINISHED_ENTRY, error: '命令退出码 2' }
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={entry} />)

    const error = screen.getByRole('region', { name: '错误' })
    expect(within(error).getByText('命令退出码 2')).toBeInTheDocument()
  })

  it('shows the quiet unconfigured line for a node with nothing recorded', () => {
    const start: WorkflowNodeData = { kind: 'start', title: '开始', description: '' }
    renderWithProviders(<RunInspector data={start} entry={undefined} />)

    expect(screen.getByText('该节点没有可展示的配置。')).toBeInTheDocument()
    expect(screen.getByText('该节点没有可展示的输出。')).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: '执行信息' })).not.toBeInTheDocument()
    expect(screen.queryByRole('region', { name: '错误' })).not.toBeInTheDocument()
  })

  it('shows no execution times for a node the run never reached', () => {
    // An inactive node still has a trace row, but its blank timestamps are not
    // evidence of a start or finish, so the execution section stays hidden.
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={{ status: 'inactive' }} />)

    expect(screen.queryByRole('region', { name: '执行信息' })).not.toBeInTheDocument()
  })

  it('notes the display name the trace labelled the node with', () => {
    const entry: RunNodeEntry = { status: 'succeeded', displayName: 'Agent 伪名' }
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={entry} />)

    expect(screen.getByText('运行记录为「Agent 伪名」')).toBeInTheDocument()
  })

  it('offers no edit affordance whatsoever', () => {
    renderWithProviders(<RunInspector data={TOOL_DATA} entry={FINISHED_ENTRY} />)

    expect(screen.queryByRole('button')).not.toBeInTheDocument()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    expect(screen.queryByRole('combobox')).not.toBeInTheDocument()
  })
})
