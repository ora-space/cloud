import type { GraphWorkflowNodeStatus } from '@/features/workflows/runtime/types'

/**
 * One node's execution row as the backend stores it in `workflow_runs.node_states`.
 *
 * The engine-agnostic record intentionally mirrors the wire shape (plus the status the
 * overview needs), not the richer desktop runtime state: cloud's run viewer is built to
 * render exactly what a persistent run row can carry.
 */
export interface RunNodeEntry {
  status: GraphWorkflowNodeStatus
  /** Title the trace labelled the node with; absent when a node never ran. */
  displayName?: string
  startedAt?: string
  finishedAt?: string
  /** The 1-based attempt this row describes; absent for a node that never started. */
  attempt?: number
  /** When the next attempt becomes due; present only while the node is retrying. */
  retryAt?: string
  error?: string
  /** Whatever the node produced; rendered as text by the inspector. */
  output?: unknown
}

const NODE_STATUSES = new Set<GraphWorkflowNodeStatus>([
  'idle',
  'inactive',
  'running',
  'succeeded',
  'failed',
  'cancelled',
  'retrying',
])

/**
 * Decodes the persisted `node_states` map into typed node rows.
 *
 * Entries are dropped when they are not objects or carry no recognizable status: a malformed
 * row is rendered as an idle node, never thrown or guessed at. A `pending` run has no rows at
 * all, so its overview paints every node idle.
 *
 * @param raw - The raw `node_states` jsonb value from the run row.
 * @returns Valid node rows keyed by node id.
 */
export function parseRunNodeStates(
  raw: Record<string, unknown> | undefined,
): Record<string, RunNodeEntry> {
  const states: Record<string, RunNodeEntry> = {}
  if (raw === undefined) {
    return states
  }
  for (const [nodeId, value] of Object.entries(raw)) {
    const entry = readNodeEntry(value)
    if (entry !== undefined) {
      states[nodeId] = entry
    }
  }
  return states
}

/** Reads one entry, or `undefined` when the value is not a recognizable node state. */
function readNodeEntry(value: unknown): RunNodeEntry | undefined {
  if (!isRecord(value)) {
    return undefined
  }
  const status = value['status']
  if (typeof status !== 'string' || !isRunNodeStatus(status)) {
    return undefined
  }
  const entry: RunNodeEntry = { status }
  const startedAt = readTimestamp(value['startedAt'])
  if (startedAt !== undefined) {
    entry.startedAt = startedAt
  }
  const finishedAt = readTimestamp(value['finishedAt'])
  if (finishedAt !== undefined) {
    entry.finishedAt = finishedAt
  }
  const attempt = value['attempt']
  if (typeof attempt === 'number' && Number.isInteger(attempt) && attempt >= 1) {
    entry.attempt = attempt
  }
  const retryAt = readTimestamp(value['retryAt'])
  if (retryAt !== undefined) {
    entry.retryAt = retryAt
  }
  const error = value['error']
  if (typeof error === 'string') {
    entry.error = error
  }
  if ('output' in value) {
    entry.output = value['output']
  }
  const displayName = value['displayName']
  if (typeof displayName === 'string') {
    entry.displayName = displayName
  }
  return entry
}

/** A non-null, non-array object: the only shape a node state entry may be. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

/**
 * A timestamp worth showing, or `undefined` when the field is absent or blank.
 *
 * The backend writes `startedAt`/`finishedAt` as `""` for a node that never ran
 * (or has not reached that instant), so the empty string is "unset", not a time.
 */
function readTimestamp(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined
}

/** Narrows a persisted status string to one this viewer can render. */
function isRunNodeStatus(value: string): value is GraphWorkflowNodeStatus {
  return (NODE_STATUSES as ReadonlySet<string>).has(value)
}
