/** Categories preserve observations outside the three visual timeline lanes. */
export type SessionCategory =
  | 'user'
  | 'user_echo'
  | 'assistant'
  | 'thought'
  | 'tool'
  | 'metadata'
  | 'lifecycle'
  | 'gap'
  | 'unknown'

/** A configured Run model is provenance, not proof of provider billing or execution. */
export interface FrozenSessionModel {
  id: string
  source: 'frozen_run_binding'
}

/** User-marker grouping is an observation ordinal, never an LLM-call count. */
export interface SessionTurnMarker {
  id: string
  ordinal: number
  source: 'archive_user_marker' | 'thread_user_marker'
}

/** Tool observations retain raw payloads and distinguish absent timing from measured zero. */
export interface SessionTool {
  id: string | null
  kind: string | null
  title: string | null
  nativeName: null
  status: string | null
  isError: boolean | null
  rawInput: unknown
  rawOutput: unknown
  rawInputPresent: boolean
  rawOutputPresent: boolean
  output: string | null
  startedAt: string | null
  startedAtMs: number | null
  durationMs: number | null
  endedAtMs: number | null
}

/** One replayed observation; raw retains its original archive record or Thread envelope. */
export interface SessionEvent {
  id: string
  seq: number | null
  sequenceSource: 'archive' | 'thread'
  archiveSeq: number | null
  category: SessionCategory
  text: string | null
  timestamp: string | null
  timestampMs: number | null
  timestampSource: 'record.at' | null
  createdAt: string | null
  turn: SessionTurnMarker | null
  explicitTurnId: string | null
  model: FrozenSessionModel | null
  tool: SessionTool | null
  usage: null
  costUsd: null
  latencyMs: null
  raw: unknown
  record: Record<string, unknown>
}

/** Timeline rows include untimed observations; only positioned bars require startMs. */
export interface SessionTimelineEvent extends SessionEvent {
  kind: 'user' | 'llm' | 'tool'
  startMs: number | null
  durationMs: number | null
}

/** The observed time extent includes known tool ends and is not total session runtime. */
export interface SessionTimelineModel {
  events: SessionTimelineEvent[]
  minTimestampMs: number | null
  maxTimestampMs: number | null
  observedSpanMs: number | null
}

/** Counts describe tool records; duration averages use only records with known durations. */
export interface SessionToolSummary {
  kind: string
  count: number
  completedCount: number
  failedCount: number
  incompleteCount: number
  unknownStatusCount: number
  durationKnownCount: number
  totalDurationMs: number | null
  averageDurationMs: number | null
  maxDurationMs: number | null
}

/** Loaded-record statistics never imply provider call totals or complete-session billing. */
export interface SessionStatistics {
  recordCount: number
  rawRecordCount: number
  replacedRecordCount: number
  userMessageCount: number
  assistantTextCount: number
  thoughtCount: number
  toolCount: number
  explicitUserMarkerCount: number
  inputTokens: null
  outputTokens: null
  costUsd: null
  llmCallCount: null
  latencyMs: null
}

/** Safe diagnostic codes contain no record bodies or personal data. */
export interface SessionDiagnostic {
  code: 'invalid_record' | 'invalid_sequence' | 'invalid_time' | 'unknown_event' | 'history_gap'
  inputIndex: number
}

/** Both sources retain original inputs separately from final-version replayed observations. */
export interface SessionSnapshot {
  runId: string
  source: 'archive' | 'thread'
  events: SessionEvent[]
  rawRecords: unknown[]
  timeline: SessionTimelineModel
  tools: SessionToolSummary[]
  stats: SessionStatistics
  diagnostics: SessionDiagnostic[]
  frozenModel: FrozenSessionModel | null
}

/** Archive inputs are untrusted JSON values; optional metadata must contain a valid id. */
export interface ArchiveSessionInput {
  runId: string
  records: readonly unknown[]
  frozenModel?: unknown
}

/** Thread inputs use public top-level seq; internal database fields are never consulted. */
export interface ThreadSessionInput {
  runId: string
  entries: readonly unknown[]
  frozenModel?: unknown
}
