import type { SourceBatch } from './records'
import type {
  SessionEvent,
  SessionStatistics,
  SessionTimelineEvent,
  SessionTimelineModel,
} from './types'

function timelineEvent(event: SessionEvent): SessionTimelineEvent | null {
  let kind: SessionTimelineEvent['kind']
  switch (event.category) {
    case 'user':
      kind = 'user'
      break
    case 'assistant':
      kind = 'llm'
      break
    case 'tool':
      kind = 'tool'
      break
    default:
      return null
  }
  const startMs = event.tool ? event.tool.startedAtMs : event.timestampMs
  return { ...event, kind, startMs, durationMs: event.tool?.durationMs ?? null }
}

/** Unknown-time rows remain selectable; only known starts and ends define the strip extent. */
export function buildTimeline(events: SessionEvent[]): SessionTimelineModel {
  const rows = events.map(timelineEvent).filter((event) => event !== null)
  let minTimestampMs: number | null = null
  let maxTimestampMs: number | null = null
  // A permitted archive can contain 100,000 records: spreading their endpoints exceeds
  // JavaScript's argument limit. Accumulate the extent without a second array of endpoints.
  for (const event of rows) {
    if (event.startMs === null) continue
    const end = event.startMs + (event.durationMs ?? 0)
    minTimestampMs = Math.min(minTimestampMs ?? event.startMs, event.startMs)
    maxTimestampMs = Math.max(maxTimestampMs ?? end, end)
  }
  let observedSpanMs: number | null = null
  if (minTimestampMs !== null && maxTimestampMs !== null)
    observedSpanMs = maxTimestampMs - minTimestampMs
  return { events: rows, minTimestampMs, maxTimestampMs, observedSpanMs }
}

/** Counts only the loaded observations and keeps all unproven provider metrics unknown. */
export function buildStatistics(batch: SourceBatch, events: SessionEvent[]): SessionStatistics {
  const count = (category: SessionEvent['category']) =>
    events.filter((event) => event.category === category).length
  return {
    recordCount: events.length,
    rawRecordCount: batch.rawRecords.length,
    replacedRecordCount: batch.replacedCount,
    userMessageCount: count('user'),
    assistantTextCount: count('assistant'),
    thoughtCount: count('thought'),
    toolCount: count('tool'),
    explicitUserMarkerCount: count('user'),
    inputTokens: null,
    outputTokens: null,
    costUsd: null,
    llmCallCount: null,
    latencyMs: null,
  }
}
