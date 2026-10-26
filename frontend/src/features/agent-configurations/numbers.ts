import type { PutApiV1MeAgentConfigurationsSpaceIdPluginNamespacePluginIdentifierBody as AgentConfigurationWrite } from '@/api/generated.schemas'

const JSON_NUMBER = /^-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?$/

/** Numeric setting literals kept apart from JavaScript's approximate number representation. */
export type AgentNumberLiterals = Record<string, string>

/** Checks a bounded JSON number before it can enter an exact request serialization. */
export function agentNumberLiteral(value: string): boolean {
  return value.length <= 1024 && JSON_NUMBER.test(value) && Number.isFinite(Number(value))
}

function record(value: unknown): Record<string, unknown> {
  return isRecord(value) ? value : {}
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function literal(value: unknown): string | undefined {
  const source = record(value)['numberLiteral']
  return typeof source === 'string' && agentNumberLiteral(source) ? source : undefined
}

/** Axios response boundary: retain exact numeric settings/defaults without changing schema types. */
export function agentConfigurationResponse(data: unknown): unknown {
  if (typeof data !== 'string') throw new Error('invalid_agent_configuration_response')
  const parsed: unknown = JSON.parse(data)
  const exact: unknown = JSON.parse(
    data,
    (_key: string, value: unknown, context?: { source?: string }) => {
      if (typeof value !== 'number') return value
      if (context?.source === undefined) throw new Error('exact_json_numbers_unavailable')
      return { numberLiteral: context.source }
    },
  )
  const values: AgentNumberLiterals = {}
  for (const [key, value] of Object.entries(record(record(exact)['settings']))) {
    const source = literal(value)
    if (source !== undefined) values[key] = source
  }
  const defaults: AgentNumberLiterals = {}
  const fields = record(record(exact)['declaration'])['settings']
  if (Array.isArray(fields)) {
    for (const field of fields) {
      const id = record(field)['id']
      const source = literal(record(field)['default'])
      if (typeof id === 'string' && source !== undefined) defaults[id] = source
    }
  }
  return { ...record(parsed), numericValues: values, numericDefaults: defaults }
}

/** Serializes only declared numeric settings verbatim; keys and all other fields stay JSON encoded. */
export function serializeAgentConfiguration(
  body: AgentConfigurationWrite,
  numbers: AgentNumberLiterals,
): string {
  const { settings, ...fields } = body
  const values = Object.entries(settings).map(([key, value]) => {
    const source = numbers[key]
    if (source !== undefined && (typeof value !== 'number' || !agentNumberLiteral(source))) {
      throw new Error('invalid_agent_number')
    }
    return JSON.stringify(key) + ':' + (source ?? JSON.stringify(value))
  })
  return JSON.stringify(fields).slice(0, -1) + ',"settings":{' + values.join(',') + '}}'
}
