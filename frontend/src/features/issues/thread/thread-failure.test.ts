import { describe, expect, it } from 'vitest'

import { sessionFailureMessage } from './thread-failure'

describe('sessionFailureMessage', () => {
  it('names each Agent model configuration failure instead of a generic one', () => {
    expect(sessionFailureMessage('agent_model_protocol_mismatch')).toBe(
      'Agent 插件不支持所选模型连接的协议（agent_model_protocol_mismatch）',
    )
    expect(sessionFailureMessage('agent_adapter_unavailable')).toBe(
      '执行节点不支持该 Agent 的模型配置（agent_adapter_unavailable）',
    )
  })

  it('replaces an unknown code or provider text with the generic failure', () => {
    expect(sessionFailureMessage('401 from https://provider.example')).toBe(
      'Agent 会话失败（agent_failed）',
    )
    expect(sessionFailureMessage(undefined)).toBe('Agent 会话失败（agent_failed）')
  })
})
