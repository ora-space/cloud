import type { AgentConfiguration, AgentConfigurationWrite } from './api'
import { agentNumberLiteral, type AgentNumberLiterals } from './numbers'

/** Scalar values supported by the static declaration, kept in an editable draft only. */
export type AgentSettingValue = string | number | boolean
/** A control value can remain blank while the user edits a number or optional setting. */
export type AgentSettingDraft = Record<string, AgentSettingValue>
/** An edit's baseline is frozen until an explicit user reconciliation or successful save. */
export type AgentConfigurationDraft = {
  revision: number
  selectionVersion: number
  fingerprint: string
  connectionId: string
  modelId: string
  values: AgentSettingDraft
  schema: AgentSetting[]
  usesGlobalDefault: boolean
  requiresRepair: boolean
  removedFields: string[]
}
/** One compiled field from the selected release. */
export type AgentSetting = AgentConfiguration['declaration']['settings'][number]

function matches(setting: AgentSetting, value: unknown): value is AgentSettingValue {
  return typeof value === setting.settingType
}

/** Builds a draft from server values, filtering removed/type-changed fields before repair. */
export function agentConfigurationDraft(
  context: AgentConfiguration,
  kept?: AgentConfigurationDraft,
): AgentConfigurationDraft {
  const source = kept?.values ?? (context.cleared ? {} : context.settings)
  return {
    revision: context.revision,
    selectionVersion: context.selectionVersion,
    fingerprint: context.declaration.fingerprint,
    connectionId: kept?.connectionId ?? context.connectionId ?? '',
    modelId: kept?.modelId ?? context.modelId ?? '',
    values: draftSettingValues(context, source, kept),
    schema: context.declaration.settings,
    usesGlobalDefault: context.modelDefault !== undefined,
    requiresRepair: context.requiresRepair,
    removedFields: Object.keys(source).filter(
      (id) => !context.declaration.settings.some((setting) => setting.id === id),
    ),
  }
}

function draftSettingValues(
  context: AgentConfiguration,
  source: AgentSettingDraft,
  kept: AgentConfigurationDraft | undefined,
): AgentSettingDraft {
  const values: AgentSettingDraft = {}
  for (const setting of context.declaration.settings) {
    const value = source[setting.id]
    const exact = kept ? undefined : context.numericValues?.[setting.id]
    const defaultLiteral = context.numericDefaults?.[setting.id]
    const previousType = kept?.schema.find((field) => field.id === setting.id)?.settingType
    if (
      matches(setting, value) ||
      (previousType === 'number' && setting.settingType === 'number' && typeof value === 'string')
    ) {
      values[setting.id] = exact ?? value
    } else if (matches(setting, setting.default))
      values[setting.id] = defaultLiteral ?? setting.default
  }
  return values
}

type SettingWrite =
  { kind: 'value'; value: AgentSettingValue } | { kind: 'missing' } | { kind: 'invalid' }

function settingWrite(setting: AgentSetting, raw: AgentSettingValue | undefined): SettingWrite {
  if (raw === undefined || (raw === '' && setting.settingType !== 'string'))
    return { kind: 'missing' }
  if (setting.settingType === 'number') return numericSettingWrite(raw)
  if (!matches(setting, raw) || (setting.required && raw === '')) return { kind: 'invalid' }
  return { kind: 'value', value: raw }
}

function numericSettingWrite(raw: AgentSettingValue): SettingWrite {
  if (typeof raw === 'boolean') return { kind: 'invalid' }
  if (typeof raw === 'string' && !agentNumberLiteral(raw)) return { kind: 'invalid' }
  const value = Number(raw)
  if (
    !Number.isFinite(value) ||
    (typeof raw === 'number' && Number.isInteger(value) && !Number.isSafeInteger(value))
  )
    return { kind: 'invalid' }
  return { kind: 'value', value }
}

/** The validated numeric controls remain exact when the HTTP boundary builds JSON. */
export function agentConfigurationNumbers(draft: AgentConfigurationDraft): AgentNumberLiterals {
  const numbers: AgentNumberLiterals = {}
  for (const setting of draft.schema) {
    const value = draft.values[setting.id]
    if (setting.settingType === 'number' && typeof value === 'string' && agentNumberLiteral(value))
      numbers[setting.id] = value
  }
  return numbers
}

/** Resolves form controls into strict scalar JSON without coercing blank numbers to zero. */
export function agentConfigurationBody(
  draft: AgentConfigurationDraft,
): AgentConfigurationWrite | undefined {
  const settings: Record<string, AgentSettingValue> = {}
  for (const setting of draft.schema) {
    const resolved = settingWrite(setting, draft.values[setting.id])
    if (resolved.kind === 'invalid') return undefined
    if (resolved.kind === 'missing') {
      if (setting.required) return undefined
      continue
    }
    settings[setting.id] = resolved.value
  }
  const body: AgentConfigurationWrite = {
    revision: draft.revision,
    selectionVersion: draft.selectionVersion,
    declarationFingerprint: draft.fingerprint,
    settings,
  }
  if (!draft.usesGlobalDefault) {
    if (!draft.connectionId || !draft.modelId) return undefined
    body.connectionId = draft.connectionId
    body.modelId = draft.modelId
  }
  return body
}
