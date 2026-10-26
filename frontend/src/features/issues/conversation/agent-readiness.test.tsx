import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AgentReadiness } from './agent-readiness'

describe('AgentReadiness', () => {
  it.each([
    ['agent_declaration_required', '这个插件版本尚未提供个人配置，请联系空间管理员升级插件。'],
    ['agent_configuration_required', '请在插件的“我的配置”中选择个人模型连接和模型。'],
    ['agent_configuration_repair_required', '插件配置要求已变化，请在“我的配置”中确认修复。'],
  ])('directs %s to plugin configuration instead of model connections', (errorCode, message) => {
    render(
      <MemoryRouter>
        <AgentReadiness
          readiness={{ agentId: 'claude', requiresPersonalModel: true, errorCode }}
          settingsPath="/w/space/settings/model-connections"
          pluginsPath="/w/space/plugins"
        />
      </MemoryRouter>,
    )
    expect(screen.getByRole('alert')).toHaveTextContent(message)
    expect(screen.getByRole('link', { name: '前往插件设置' })).toHaveAttribute(
      'href',
      '/w/space/plugins',
    )
    expect(screen.queryByRole('link', { name: '配置个人模型' })).not.toBeInTheDocument()
  })

  it.each([
    ['model_connection_unavailable', '模型连接不可用，请更新个人设置'],
    ['model_credential_required', '模型凭据缺失，请更新个人设置'],
    ['model_not_found', '所选模型已不可用，请重新选择'],
    ['model_default_required', '请先配置个人默认模型'],
  ])('keeps %s linked to the model connections page', (errorCode, message) => {
    render(
      <MemoryRouter>
        <AgentReadiness
          readiness={{
            agentId: 'claude',
            requiresPersonalModel: true,
            errorCode,
          }}
          settingsPath="/w/space/settings/model-connections"
          pluginsPath="/w/space/plugins"
        />
      </MemoryRouter>,
    )
    expect(screen.getByRole('alert')).toHaveTextContent(message)
    expect(screen.getByRole('link', { name: '配置个人模型' })).toHaveAttribute(
      'href',
      '/w/space/settings/model-connections',
    )
    expect(screen.queryByRole('link', { name: '前往插件设置' })).not.toBeInTheDocument()
  })
})
