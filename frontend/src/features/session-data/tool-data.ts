import type { SessionTool, SessionToolSummary } from './types'
import { objectValue, stringValue, textValue, timestampValue } from './values'

function errorState(status: string | null): boolean | null {
  if (status === 'failed') return true
  if (status === 'completed') return false
  return null
}

function durationValue(value: unknown): number | null {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0) return null
  return value
}

function outputText(update: Record<string, unknown>, rawOutput: unknown): string | null {
  const output = objectValue(rawOutput)
  return (
    stringValue(output?.['output']) ??
    stringValue(output?.['error']) ??
    textValue(update['content'])
  )
}

/** Reads reported tool timing verbatim; absent starts never fall back to observation time. */
export function readTool(record: Record<string, unknown>): SessionTool {
  const update = objectValue(record['update']) ?? {}
  const timing = objectValue(record['tool_timing'])
  const start = timestampValue(timing?.['startedAt'])
  const durationMs = durationValue(timing?.['durationMs'])
  const rawOutput = update['rawOutput'] ?? null
  const status = stringValue(update['status'])
  return {
    id: stringValue(update['toolCallId']),
    kind: stringValue(update['kind']),
    title: stringValue(update['title']),
    nativeName: null,
    status,
    isError: errorState(status),
    rawInput: update['rawInput'] ?? null,
    rawOutput,
    rawInputPresent: Object.hasOwn(update, 'rawInput'),
    rawOutputPresent: Object.hasOwn(update, 'rawOutput'),
    output: outputText(update, rawOutput),
    startedAt: start?.text ?? null,
    startedAtMs: start?.ms ?? null,
    durationMs,
    endedAtMs: start && durationMs !== null ? start.ms + durationMs : null,
  }
}

function summarizeKind(kind: string, tools: SessionTool[]): SessionToolSummary {
  const durations = tools.map((tool) => tool.durationMs).filter((value) => value !== null)
  const completedCount = tools.filter((tool) => tool.status === 'completed').length
  const failedCount = tools.filter((tool) => tool.status === 'failed').length
  const incompleteCount = tools.filter((tool) =>
    ['pending', 'in_progress'].includes(tool.status ?? ''),
  ).length
  const totalDurationMs = durations.length ? durations.reduce((sum, value) => sum + value, 0) : null
  return {
    kind,
    count: tools.length,
    completedCount,
    failedCount,
    incompleteCount,
    unknownStatusCount: tools.length - completedCount - failedCount - incompleteCount,
    durationKnownCount: durations.length,
    totalDurationMs,
    averageDurationMs: totalDurationMs === null ? null : totalDurationMs / durations.length,
    maxDurationMs: durations.length
      ? durations.reduce((largest, value) => Math.max(largest, value))
      : null,
  }
}

/** Aggregates observations, not guessed unique calls, using known durations as the denominator. */
export function summarizeTools(tools: SessionTool[]): SessionToolSummary[] {
  const groups = new Map<string, SessionTool[]>()
  for (const tool of tools) {
    const kind = tool.kind ?? 'unknown'
    const group = groups.get(kind) ?? []
    group.push(tool)
    groups.set(kind, group)
  }
  return [...groups].map(([kind, group]) => summarizeKind(kind, group))
}
