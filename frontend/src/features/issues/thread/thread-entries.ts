import type {
  GetApiV1TenantsTidIssuesIidRunsRidThread200ThreadState,
  ThreadEntry,
} from '@/api/generated.schemas'
import type { IssueRun } from '@/features/issues/types'

/** The Thread lifecycle states the Thread GET reports, as the contract enumerates them. */
export type ThreadState = GetApiV1TenantsTidIssuesIidRunsRidThread200ThreadState

const THREAD_STATE_LABELS: Record<ThreadState, string> = {
  pending: '等待中',
  active: '进行中',
  idle: '空闲',
  ending: '结束中',
  ended: '已结束',
}

/** Chinese badge text for a Thread state; the browser acceptance test reads it verbatim. */
export function threadStateLabel(state: ThreadState): string {
  return THREAD_STATE_LABELS[state]
}

/**
 * True once the Thread refuses new turns. The server answers 409 thread_closed
 * in exactly these states, so the composer disables itself on the same rule.
 */
export function isThreadClosed(state: ThreadState): boolean {
  return state === 'ending' || state === 'ended'
}

/**
 * The run whose Thread the issue page shows: the most recently created agent
 * run. Team and workflow runs have no Thread, so they are never picked.
 */
export function latestAgentRun(runs: readonly IssueRun[]): IssueRun | undefined {
  return runs
    .filter((run) => run.executorType === 'agent')
    .reduce<IssueRun | undefined>(
      (latest, run) => (latest === undefined || run.createdAt > latest.createdAt ? run : latest),
      undefined,
    )
}

/**
 * Unions two windows of one Thread by seq, ascending. Seq is unique and the
 * server never rewrites an entry's identity, so a re-read entry replaces its
 * earlier copy (a user turn's `status` moves from queued to delivered) instead
 * of appearing twice.
 */
export function mergeEntries(
  current: readonly ThreadEntry[],
  incoming: readonly ThreadEntry[],
): ThreadEntry[] {
  const bySeq = new Map<number, ThreadEntry>()
  for (const entry of current) bySeq.set(entry.seq, entry)
  for (const entry of incoming) bySeq.set(entry.seq, entry)
  return [...bySeq.values()].toSorted((a, b) => a.seq - b.seq)
}

/** What one rendered row of the Thread is; the panel styles each variant differently. */
export type ThreadItem =
  | { variant: 'prompt'; key: number; text: string }
  | { variant: 'user'; key: number; text: string; status: string | undefined }
  | { variant: 'agent'; key: number; text: string; turnId: string | undefined }
  | { variant: 'note'; key: number; text: string }
  | { variant: 'separator'; key: number }

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

/** Joins the text blocks of a content array; non-text blocks contribute nothing. */
function textOfBlocks(content: unknown): string {
  if (typeof content === 'string') return content
  if (!Array.isArray(content)) return isRecord(content) ? textOfBlock(content) : ''
  return content
    .map((block) => (isRecord(block) ? textOfBlock(block) : ''))
    .filter(Boolean)
    .join('\n')
}

function textOfBlock(block: Record<string, unknown>): string {
  return block['type'] === 'text' && typeof block['text'] === 'string' ? block['text'] : ''
}

const USER_STATUS_LABELS: Record<string, string> = {
  queued: '排队中',
  delivered: '已送达',
  discarded: '已丢弃',
}

/** Chinese label for a user turn's delivery status, or undefined for node/system entries. */
export function userStatusLabel(status: string | null | undefined): string | undefined {
  return status ? (USER_STATUS_LABELS[status] ?? status) : undefined
}

/** Maps a node `update` record onto a row; agent chunks become bubbles, the rest notes. */
function updateItem(entry: ThreadEntry): ThreadItem | undefined {
  const update = entry.record['update']
  const kind = isRecord(update) ? update['sessionUpdate'] : undefined
  // The Node echoes the user's own turn as user_message_chunk; the user entry
  // already shows it, so rendering the echo would duplicate the message.
  if (kind === 'user_message_chunk') return undefined
  if (kind === 'agent_message_chunk' && isRecord(update)) {
    return {
      variant: 'agent',
      key: entry.seq,
      text: textOfBlocks(update['content']),
      turnId: entry.turnId ?? undefined,
    }
  }
  return { variant: 'note', key: entry.seq, text: typeof kind === 'string' ? kind : 'update' }
}

/** Maps one entry to its row, or undefined when the entry carries nothing worth showing. */
function itemOf(entry: ThreadEntry): ThreadItem | undefined {
  if (entry.source === 'system') {
    return { variant: 'prompt', key: entry.seq, text: textOfBlocks(entry.record['content']) }
  }
  if (entry.source === 'user') {
    return {
      variant: 'user',
      key: entry.seq,
      text: textOfBlocks(entry.record['content']),
      status: userStatusLabel(entry.status),
    }
  }
  if (entry.kind === 'update') return updateItem(entry)
  if (entry.kind === 'meta') return { variant: 'note', key: entry.seq, text: '会话已开始' }
  if (entry.kind === 'turnEnded') return { variant: 'separator', key: entry.seq }
  return { variant: 'note', key: entry.seq, text: entry.kind }
}

/**
 * Turns ascending entries into display rows. Consecutive agent chunks of the
 * same turn are merged into one bubble, because the agent streams a reply as
 * many small chunks and one bubble per chunk would be unreadable.
 */
export function threadItems(entries: readonly ThreadEntry[]): ThreadItem[] {
  const items: ThreadItem[] = []
  for (const entry of entries) {
    const item = itemOf(entry)
    if (item === undefined) continue
    const last = items.at(-1)
    if (item.variant === 'agent' && last?.variant === 'agent' && last.turnId === item.turnId) {
      last.text += item.text
    } else {
      items.push(item)
    }
  }
  return items
}
