import { buildEvents } from './events'
import { buildStatistics, buildTimeline } from './projection'
import { normalizeRecords } from './records'
import type { SourceBatch } from './records'
import { summarizeTools } from './tool-data'
import type { ArchiveSessionInput, SessionSnapshot, ThreadSessionInput } from './types'
import { frozenModelValue } from './values'

function snapshot(batch: SourceBatch, metadata: unknown): SessionSnapshot {
  const frozenModel = frozenModelValue(metadata)
  const events = buildEvents(batch, frozenModel)
  const tools = events.map((event) => event.tool).filter((tool) => tool !== null)
  return {
    runId: batch.runId,
    source: batch.source,
    events,
    rawRecords: batch.rawRecords,
    timeline: buildTimeline(events),
    tools: summarizeTools(tools),
    stats: buildStatistics(batch, events),
    diagnostics: batch.diagnostics,
    frozenModel,
  }
}

/** Replays final archive versions by seq while preserving physical inputs for raw inspection. */
export function adaptArchive(input: ArchiveSessionInput): SessionSnapshot {
  return snapshot(
    normalizeRecords({ runId: input.runId, values: input.records, source: 'archive' }),
    input.frozenModel,
  )
}

/** Adapts public Thread pages without substituting database time or assuming hidden turn IDs. */
export function adaptThread(input: ThreadSessionInput): SessionSnapshot {
  return snapshot(
    normalizeRecords({ runId: input.runId, values: input.entries, source: 'thread' }),
    input.frozenModel,
  )
}
