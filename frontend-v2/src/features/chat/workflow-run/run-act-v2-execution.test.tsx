import '@/lib/workflows/test/dom'
import { render, screen } from '@testing-library/react'
import type { ReactElement } from 'react'
import { I18nextProvider } from 'react-i18next'
import { describe, expect, it } from 'vitest'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { RunAnalyzedUnappliedContext } from './run-analysis-context'
import { RunActV2Execution } from './run-act-v2-execution'
import { workflowRunTranslations } from './translations'

// The run page's own strings, as RunPane provides them.
const runI18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })
const view = (ui: ReactElement) => render(<I18nextProvider i18n={runI18n}>{ui}</I18nextProvider>)

describe('RunActV2Execution', () => {
  it('shows the persisted model for a matched executor without a fallback notice', () => {
    view(<RunActV2Execution attempt={{
      executor: { id: 'selected', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '' },
      model: { id: 'actual-model', name: 'Actual Model', requested: 'actual-model', fallback: false },
    }} />)
    expect(screen.getByLabelText('实际执行')).toHaveTextContent('OpenCode · Actual Model')
    expect(screen.getByLabelText('实际执行')).not.toHaveTextContent('默认模型')
    expect(screen.getByLabelText('实际执行')).not.toHaveTextContent('claude-sonnet-5-5')
  })

  it('shows the executor and model the attempt ran on and what v2 did not apply', () => {
    view(
      <RunActV2Execution
        attempt={{
          executor: { id: 'opencode:fixture', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '1.4.0' },
          model: { id: 'fixture', name: 'Fixture', requested: 'claude-sonnet-5-5', fallback: true },
          unapplied: ['role', 'retry'],
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
    view(
      <RunActV2Execution
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

  it('names exactly what the attempt recorded, in the server order', () => {
    view(<RunActV2Execution attempt={{ unapplied: ['mcp', 'interactive', 'role'] }} />)
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：MCP、交互模式、角色')
  })

  it('names nothing the attempt did not record', () => {
    view(<RunActV2Execution attempt={{ executor: { id: 'e', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '' }, model: { id: 'm', name: 'M', requested: 'm', fallback: false }, unapplied: [] }} />)
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
  })

  it('falls back to the run analysis for a failed attempt that recorded no list', () => {
    const analyzed = new Map([['writer', ['role', 'retry']], ['other', ['mcp']]])
    view(
      <RunAnalyzedUnappliedContext.Provider value={analyzed}>
        <RunActV2Execution nodeId="writer" attempt={{}} />
      </RunAnalyzedUnappliedContext.Provider>,
    )
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：角色、自动重试')
  })

  it('prefers the list the attempt recorded over the run analysis, even an empty one', () => {
    const analyzed = new Map([['writer', ['role', 'retry']]])
    const { unmount } = view(
      <RunAnalyzedUnappliedContext.Provider value={analyzed}>
        <RunActV2Execution nodeId="writer" attempt={{ unapplied: ['mcp'] }} />
      </RunAnalyzedUnappliedContext.Provider>,
    )
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：MCP')
    unmount()
    view(
      <RunAnalyzedUnappliedContext.Provider value={analyzed}>
        <RunActV2Execution nodeId="writer" attempt={{ unapplied: [] }} />
      </RunAnalyzedUnappliedContext.Provider>,
    )
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
  })

  it('names a setting it has no label for as the server spells it', () => {
    view(
      <RunAnalyzedUnappliedContext.Provider value={new Map([['writer', ['sandbox']]])}>
        <RunActV2Execution nodeId="writer" attempt={{}} />
      </RunAnalyzedUnappliedContext.Provider>,
    )
    expect(screen.getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent('v2 未应用：sandbox')
  })

  it('shows nothing about execution or unapplied settings without a record or an analysis', () => {
    view(<RunActV2Execution nodeId="writer" attempt={{}} />)
    expect(screen.queryByLabelText('实际执行')).not.toBeInTheDocument()
    expect(screen.queryByRole('note')).not.toBeInTheDocument()
  })
})
