import { describe, expect, it } from 'vitest'
import { agentConfigurationBody, agentConfigurationDraft } from './draft'
import { agentContext } from './test-fixtures'

describe('personal Agent configuration drafts', () => {
  it('filters removed/type-changed values, resolves defaults and preserves an intermediate numeric edit', () => {
    const context = agentContext({ settings: { style: 'mine', budget: true, removed: 'old' } })
    const draft = agentConfigurationDraft(context)
    expect(draft.values).toEqual({ style: 'mine', budget: 5, verbose: false })
    expect(draft.removedFields).toEqual(['removed'])
    draft.values['budget'] = '12.5'
    expect(agentConfigurationDraft(agentContext({ revision: 2 }), draft).values['budget']).toBe(
      '12.5',
    )
    expect(draft.revision).toBe(1)
  })

  it('uses strict scalar types and does not turn a blank optional number into zero', () => {
    const draft = agentConfigurationDraft(agentContext())
    draft.values['budget'] = ''
    expect(agentConfigurationBody(draft)?.settings).toEqual({ style: 'concise', verbose: false })
    draft.values['budget'] = 'not-a-number'
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['budget'] = Infinity
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['budget'] = Number.MAX_SAFE_INTEGER + 1
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['budget'] = false
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['budget'] = 1.5
    expect(agentConfigurationBody(draft)?.settings['budget']).toBe(1.5)
  })

  it('keeps false as a supplied boolean and refuses absent required values and model references', () => {
    const draft = agentConfigurationDraft(agentContext())
    delete draft.values['style']
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['style'] = ''
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.values['style'] = 'valid'
    draft.connectionId = ''
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.connectionId = 'mc1'
    draft.modelId = ''
    expect(agentConfigurationBody(draft)).toBeUndefined()
    draft.modelId = 'vendor/model'
    expect(agentConfigurationBody(draft)?.settings['verbose']).toBe(false)
  })

  it('does not copy the OpenCode global model into plugin options and starts cleared values afresh', () => {
    const draft = agentConfigurationDraft(
      agentContext({
        cleared: true,
        modelDefault: { connectionId: 'global', modelId: 'global/model', version: 7 },
      }),
    )
    expect(draft.values['style']).toBeUndefined()
    draft.values['style'] = 'new'
    expect(agentConfigurationBody(draft)).toEqual({
      revision: 1,
      selectionVersion: 3,
      declarationFingerprint: 'a'.repeat(64),
      settings: { style: 'new', budget: 5, verbose: false },
    })
  })
})
