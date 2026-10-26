import { describe, expect, it } from 'vitest'
import {
  agentConfigurationResponse,
  agentNumberLiteral,
  serializeAgentConfiguration,
} from './numbers'

describe('exact personal setting numbers', () => {
  it('retains large integers and precise decimals in saved values and declaration defaults', () => {
    const result = agentConfigurationResponse(
      '{"revision":1,"settings":{"counter":9007199254740993,"ratio":0.123456789012345678901,"style":"literal"},"declaration":{"settings":[{"id":"budget","default":9007199254740995}]}}',
    )
    expect(result).toEqual(
      expect.objectContaining({
        revision: 1,
        numericValues: {
          counter: '9007199254740993',
          ratio: '0.123456789012345678901',
        },
        numericDefaults: { budget: '9007199254740995' },
      }),
    )
    expect(() => agentConfigurationResponse(undefined)).toThrow(
      'invalid_agent_configuration_response',
    )
  })

  it('serializes number literals exactly and JSON encodes every key and non-number value', () => {
    expect(
      serializeAgentConfiguration(
        {
          revision: 1,
          selectionVersion: 2,
          declarationFingerprint: 'f',
          settings: {
            counter: 9007199254740992,
            ratio: 0.123,
            style: 'literal $(text)',
            verbose: false,
          },
        },
        { counter: '9007199254740993', ratio: '0.123456789012345678901' },
      ),
    ).toBe(
      '{"revision":1,"selectionVersion":2,"declarationFingerprint":"f","settings":{"counter":9007199254740993,"ratio":0.123456789012345678901,"style":"literal $(text)","verbose":false}}',
    )
    expect(() =>
      serializeAgentConfiguration(
        { revision: 1, selectionVersion: 2, declarationFingerprint: 'f', settings: { budget: 2 } },
        { budget: '2,"other":3' },
      ),
    ).toThrow('invalid_agent_number')
  })

  it('accepts JSON exponent numbers and rejects blank, leading zeroes and non-finite input', () => {
    for (const value of ['-1', '0.125', '2e3', '9007199254740993'])
      expect(agentNumberLiteral(value)).toBe(true)
    for (const value of ['', ' 2', '01', 'NaN', '1e400', '0x10'])
      expect(agentNumberLiteral(value)).toBe(false)
  })
})
