import type { FrozenSessionModel } from './types'

function isFieldObject(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

/** Narrows an untrusted JSON object without treating arrays as field dictionaries. */
export function objectValue(value: unknown): Record<string, unknown> | null {
  return isFieldObject(value) ? value : null
}

/** Keeps an explicitly supplied string, including a meaningful empty output. */
export function stringValue(value: unknown): string | null {
  return typeof value === 'string' ? value : null
}

/** Sequence identity requires safe integers; archive zero is valid, Thread zero is not. */
export function sequenceValue(value: unknown, minimum: number): number | null {
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) return null
  return value
}

/** Only explicit text blocks are flattened; arbitrary JSON stays available in raw data. */
export function textValue(value: unknown): string | null {
  if (typeof value === 'string') return value
  if (Array.isArray(value)) {
    const parts = value.map(textValue).filter((text) => text !== null)
    return parts.length ? parts.join('\n') : null
  }
  const block = objectValue(value)
  if (block?.['type'] === 'content') return textValue(block['content'])
  if (block?.['type'] === 'text') return stringValue(block['text'])
  return null
}

function calendarValid(parts: RegExpExecArray): boolean {
  const year = Number(parts[1])
  const month = Number(parts[2])
  const day = Number(parts[3])
  const hour = Number(parts[4])
  const minute = Number(parts[5])
  const second = Number(parts[6])
  const days = new Date(Date.UTC(year, month, 0)).getUTCDate()
  return (
    month >= 1 && month <= 12 && day >= 1 && day <= days && hour < 24 && minute < 60 && second < 60
  )
}

/** Parses timezone-bearing RFC3339 instants without Date.parse's invalid-date rollover. */
export function timestampValue(value: unknown): { text: string; ms: number } | null {
  if (typeof value !== 'string') return null
  const parts =
    /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d+)?(Z|[+-]\d{2}:\d{2})$/.exec(value)
  if (!parts || !calendarValid(parts)) return null
  const zone = parts[7] ?? ''
  if (zone !== 'Z' && (Number(zone.slice(1, 3)) > 23 || Number(zone.slice(4, 6)) > 59)) return null
  const ms = Date.parse(value)
  return Number.isFinite(ms) ? { text: value, ms } : null
}

/** Accepts an explicit frozen model id only; agent prose never identifies the model. */
export function frozenModelValue(value: unknown): FrozenSessionModel | null {
  const id = objectValue(value)?.['id']
  if (typeof id !== 'string' || id.length === 0 || id.length > 256) return null
  if (
    /\s/.test(id) ||
    id
      .split('')
      .some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)
  )
    return null
  return { id, source: 'frozen_run_binding' }
}
