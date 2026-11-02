import '@/lib/workflows/test/dom'
import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { WorkflowAgentConfig } from '@/lib/workflows/runtime'
import { unappliedAgentSettings } from './agent-unapplied'
import { RunActV2Execution } from './run-act-v2-execution'

const config = (over: Partial<WorkflowAgentConfig> = {}): WorkflowAgentConfig => ({
  schemaVersion: 3,
  executor: { agentCli: 'ora/opencode', modelId: 'claude-sonnet-5-5' },
  roleId: '',
  skills: [],
  mcps: [],
  prompt: '起草',
  interactive: false,
  retry: { enabled: false, maxRetries: 0, initialDelaySeconds: 0 },
  ...over,
})

describe('unappliedAgentSettings', () => {
  it('names nothing for a node that configures nothing v2 skips', () => {
    expect(unappliedAgentSettings(config())).toEqual([])
  })

  it('names every configured setting v2 skips, in a fixed order', () => {
    const configured = config({
      roleId: 'reviewer',
      skills: [{ skillId: 'lint', enabled: true }],
      mcps: [{ mcpId: 'github', enabled: true }],
      interactive: true,
    })
    expect(unappliedAgentSettings(configured)).toEqual(['role', 'skill', 'mcp', 'interactive'])
  })

  it('ignores turned-off bindings and a blank role', () => {
    const configured = config({ roleId: '  ', skills: [{ skillId: 'lint', enabled: false }], mcps: [{ mcpId: 'github', enabled: false }] })
    expect(unappliedAgentSettings(configured)).toEqual([])
  })

  it('counts the default retry policy, since v2 never retries', () => {
    expect(unappliedAgentSettings(config({ retry: undefined }))).toEqual(['retry'])
    expect(unappliedAgentSettings(config({ retry: { enabled: true, maxRetries: 0, initialDelaySeconds: 5 } }))).toEqual([])
  })

  it('keeps what the attempt recorded', () => {
    expect(unappliedAgentSettings(config(), ['mcp', 'role'])).toEqual(['role', 'mcp'])
  })
})

describe('RunActV2Execution', () => {
  it('shows the executor and model the attempt ran on and what v2 did not apply', () => {
    render(
      <RunActV2Execution
        config={config({ roleId: 'reviewer', retry: undefined })}
        attempt={{
          executor: { id: 'opencode:fixture', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '1.4.0' },
          model: { id: 'fixture', name: 'Fixture', requested: 'claude-sonnet-5-5', fallback: true },
          unapplied: ['role'],
        }}
      />,
    )
    const used = screen.getByLabelText('实际执行')
    expect(used).toHaveTextContent('OpenCode · Fixture')
    expect(used).toHaveTextContent('插件版本 1.4.0')
    expect(used).toHaveTextContent('没有找到配置的模型「claude-sonnet-5-5」，使用了 Agent 的默认模型')
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：角色、自动重试')
  })

  it('says a node without a model ran on the default one', () => {
    render(
      <RunActV2Execution
        config={config()}
        attempt={{
          executor: { id: 'opencode:fixture', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '' },
          model: { id: 'fixture', name: 'Fixture', requested: '', fallback: true },
        }}
      />,
    )
    expect(screen.getByLabelText('实际执行')).toHaveTextContent('节点没有指定模型，使用了 Agent 的默认模型')
    expect(screen.getByLabelText('实际执行')).not.toHaveTextContent('插件版本')
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
  })

  it('shows nothing about execution before the Agent has run', () => {
    render(<RunActV2Execution config={config({ mcps: [{ mcpId: 'github', enabled: true }] })} attempt={{}} />)
    expect(screen.queryByLabelText('实际执行')).not.toBeInTheDocument()
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：MCP')
  })
})
