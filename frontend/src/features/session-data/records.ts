import type { SessionDiagnostic } from './types'
import { objectValue, sequenceValue, stringValue } from './values'

/** Normalized envelopes keep cursor identity distinct from the archive's own sequence. */
export interface SourceRecord {
  raw: unknown
  record: Record<string, unknown>
  seq: number | null
  archiveSeq: number | null
  inputIndex: number
  createdAt: string | null
  explicitTurnId: string | null
  userTurn: boolean
  valid: boolean
}

/** Source batches preserve every input while replaying only recognized sequence identities. */
export interface SourceBatch {
  runId: string
  source: 'archive' | 'thread'
  rawRecords: unknown[]
  records: SourceRecord[]
  diagnostics: SessionDiagnostic[]
  replacedCount: number
}

function archiveRecord(raw: unknown, inputIndex: number): SourceRecord {
  const record = objectValue(raw)
  const seq = sequenceValue(record?.['seq'], 0)
  return {
    raw,
    record: record ?? {},
    seq,
    archiveSeq: seq,
    inputIndex,
    createdAt: null,
    explicitTurnId: null,
    userTurn: false,
    valid: record !== null,
  }
}

function threadRecord(raw: unknown, inputIndex: number): SourceRecord {
  const entry = objectValue(raw)
  const record = objectValue(entry?.['record'])
  const source = entry?.['source']
  const userTurn = entry?.['kind'] === 'user_turn' && (source === 'user' || source === 'system')
  return {
    raw,
    record: record ?? {},
    inputIndex,
    seq: sequenceValue(entry?.['seq'], 1),
    archiveSeq: sequenceValue(record?.['seq'], 0),
    createdAt: stringValue(entry?.['createdAt']),
    explicitTurnId: stringValue(entry?.['turnId']),
    userTurn,
    valid: record !== null && ['node', 'user', 'system'].includes(String(source)),
  }
}

/** Performs last-physical-version replay without interpreting record.seq as a Thread cursor. */
export function normalizeRecords(input: {
  runId: string
  values: readonly unknown[]
  source: 'archive' | 'thread'
}): SourceBatch {
  if (!input.runId.trim()) throw new Error('Session run identity is required')
  const rawRecords: unknown[] = structuredClone([...input.values])
  const bySeq = new Map<number, SourceRecord>()
  const invalid: SourceRecord[] = []
  const diagnostics: SessionDiagnostic[] = []
  let replacedCount = 0
  const convert = input.source === 'archive' ? archiveRecord : threadRecord
  rawRecords.forEach((raw, inputIndex) => {
    const record = convert(raw, inputIndex)
    if (!record.valid) diagnostics.push({ code: 'invalid_record', inputIndex })
    if (record.seq === null) {
      diagnostics.push({ code: 'invalid_sequence', inputIndex })
      invalid.push(record)
      return
    }
    if (bySeq.has(record.seq)) replacedCount += 1
    bySeq.set(record.seq, record)
  })
  const ordered = [...bySeq.entries()]
    .toSorted(([left], [right]) => left - right)
    .map(([, value]) => value)
  return {
    runId: input.runId,
    source: input.source,
    rawRecords,
    records: [...ordered, ...invalid],
    diagnostics,
    replacedCount,
  }
}
