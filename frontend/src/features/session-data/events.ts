import type { SourceBatch, SourceRecord } from './records'
import type { FrozenSessionModel, SessionCategory, SessionEvent, SessionTurnMarker } from './types'
import { objectValue, stringValue, textValue, timestampValue } from './values'
import { readTool } from './tool-data'

const UPDATE_CATEGORIES: Record<string, SessionCategory> = {
  agent_message_chunk: 'assistant',
  agent_thought_chunk: 'thought',
  tool_call: 'tool',
  tool_call_update: 'tool',
}
const RECORD_CATEGORIES: Record<string, SessionCategory> = {
  meta: 'metadata',
  turnEnded: 'lifecycle',
  agentSwitched: 'lifecycle',
  handoffDelivered: 'lifecycle',
  gap: 'gap',
  session_failed: 'lifecycle',
}

function categoryOf(row: SourceRecord, source: SourceBatch['source']): SessionCategory {
  if (!row.valid || row.seq === null) return 'unknown'
  if (row.userTurn) return 'user'
  const type = stringValue(row.record['type']) ?? ''
  if (type !== 'update') return RECORD_CATEGORIES[type] ?? 'unknown'
  const update = objectValue(row.record['update'])
  const tag = stringValue(update?.['sessionUpdate']) ?? ''
  if (tag === 'user_message_chunk') return source === 'archive' ? 'user' : 'user_echo'
  return UPDATE_CATEGORIES[tag] ?? 'unknown'
}

function eventIdentity(batch: SourceBatch, row: SourceRecord): string {
  const sequence = row.seq === null ? `invalid:${row.inputIndex}` : `seq:${row.seq}`
  return `${encodeURIComponent(batch.runId)}:${batch.source}:${sequence}`
}

function createEvent(input: {
  batch: SourceBatch
  row: SourceRecord
  model: FrozenSessionModel | null
  turn: SessionTurnMarker | null
}): SessionEvent {
  const { batch, row, model, turn } = input
  const category = categoryOf(row, batch.source)
  const at = timestampValue(row.record['at'])
  const update = objectValue(row.record['update'])
  const content = row.userTurn ? row.record['content'] : update?.['content']
  if (Object.hasOwn(row.record, 'at') && !at)
    batch.diagnostics.push({ code: 'invalid_time', inputIndex: row.inputIndex })
  if (category === 'unknown')
    batch.diagnostics.push({ code: 'unknown_event', inputIndex: row.inputIndex })
  if (category === 'gap')
    batch.diagnostics.push({ code: 'history_gap', inputIndex: row.inputIndex })
  return {
    id: eventIdentity(batch, row),
    seq: row.seq,
    sequenceSource: batch.source,
    archiveSeq: row.archiveSeq,
    category,
    text: textValue(content),
    timestamp: at?.text ?? null,
    timestampMs: at?.ms ?? null,
    timestampSource: at ? 'record.at' : null,
    createdAt: row.createdAt,
    turn,
    explicitTurnId: row.explicitTurnId,
    model,
    tool: category === 'tool' ? readTool(row.record) : null,
    usage: null,
    costUsd: null,
    latencyMs: null,
    raw: row.raw,
    record: row.record,
  }
}

/** Preserves all replayed rows; only explicit user records begin display marker groups. */
export function buildEvents(batch: SourceBatch, model: FrozenSessionModel | null): SessionEvent[] {
  let marker: SessionTurnMarker | null = null
  let ordinal = 0
  return batch.records.map((row) => {
    if (categoryOf(row, batch.source) === 'user') {
      ordinal += 1
      marker = {
        id: eventIdentity(batch, row),
        ordinal,
        source: batch.source === 'archive' ? 'archive_user_marker' : 'thread_user_marker',
      }
    }
    return createEvent({ batch, row, model, turn: marker })
  })
}
