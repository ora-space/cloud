import type * as acp from '@agentclientprotocol/sdk'
import type { SessionEndReason as ChatSessionEndReason, SessionEvent as ChatEvent, SessionHistoryRecord as ChatHistoryRecord, SessionPreparation, SessionResult as ChatResult } from './wire'
import { applyEvent } from './conversation'
import { observeThoughts, type ThoughtTimings } from './thought-timing'
import { type ChatEvent as ModelEvent, type Conversation, emptyConversation, type Turn } from './types'

/**
 * What the page knows about the execution session behind a conversation: the latest history
 * snapshot, the results of its ended turns in order, and whether and why it ended.
 */
export interface SessionView {
  id: string
  records: ChatHistoryRecord[]
  /** Estimates recovered from retained preview event times, independent of settled history. */
  thoughtTimings: ThoughtTimings
  /** What the first turn waited for; absent until the first stage starts. */
  preparation?: SessionPreparation
  /** What the first turn was asked, shown from preparation until its history arrives. */
  preparingPrompt?: string
  /** The snapshot left out process detail; the answers themselves are judged by `results`. */
  incomplete: boolean
  /** One per ended turn, in turn order. */
  results: ChatResult[]
  /** What each ended turn was asked, aligned with `results`. */
  prompts: string[]
  ended: boolean
  /** Why chat ended the session; empty when it ended outside chat (a restart) or is live. */
  endReason: ChatSessionEndReason
  /** The last event folded in; following again resumes after it, so nothing is applied twice. */
  cursor: number
}

/** A session before any event has arrived. */
export const emptySessionView = (id: string): SessionView => ({ id, records: [], thoughtTimings: {}, incomplete: false, results: [], prompts: [], ended: false, endReason: '', cursor: 0 })

/**
 * Folds one event-stream message into a session. A history snapshot replaces the whole history,
 * as the backend sends it; a history preview replaces only its pending records. Nothing is
 * appended chunk by chunk.
 */
export function applySessionEvent(base: SessionView, event: ChatEvent): SessionView {
  if (event.sequence <= base.cursor) return base
  const view = { ...base, cursor: event.sequence, thoughtTimings: observeThoughts(base.thoughtTimings, event, base.records) }
  switch (event.kind) {
    case 'history_snapshot':
      return event.history ? { ...view, records: event.history.records, incomplete: event.history.incomplete } : view
    case 'history_preview':
      return event.history ? { ...view, records: withPending(view.records, event.history.records), incomplete: event.history.incomplete } : view
    case 'turn_ended':
      return event.result ? { ...view, results: [...view.results, event.result], prompts: [...view.prompts, event.prompt ?? ''] } : view
    case 'session_ended':
      return { ...view, ended: true }
    case 'preparation':
      return event.preparation ? { ...view, preparation: event.preparation, preparingPrompt: event.prompt ?? view.preparingPrompt } : view
    default:
      return view
  }
}

/**
 * Swaps a history's pending records for newer ones; a settled record at the same position wins.
 * A pending record can sit before a settled one, so the result is put back in position order.
 */
function withPending(records: ChatHistoryRecord[], pending: ChatHistoryRecord[]): ChatHistoryRecord[] {
  const settled = records.filter((r) => !r.preview)
  const positions = new Set(settled.map((r) => r.sequence))
  return [...settled, ...pending.filter((r) => !positions.has(r.sequence))].sort((a, b) => a.sequence - b.sequence)
}

/**
 * The conversation its session shows. The history is rebuilt from scratch through the
 * conversation reducer; a marker after an ended session says the conversation cannot continue. A conversation has one session; the list is empty until its first turn.
 */
export function conversationOf(sessions: SessionView[]): Conversation {
  let conversation = emptyConversation
  for (const session of sessions) {
    conversation = appendSession(conversation, session)
    conversation = { ...conversation, preparation: conversation.preparation ?? session.preparation }
    if (session.ended && session.endReason !== 'deleted') {
      const afterTurn = conversation.turns.length
      conversation = {
        ...conversation,
        markers: [...conversation.markers, { kind: 'session', id: `session-${session.id}`, afterTurn, reason: session.endReason || 'interrupted' }],
      }
    }
  }
  return conversation
}

/** The parts of a fixed-Node HistoryLine the page reads; a preview's record has no at or seq. */
interface HistoryLine {
  at?: string
  seq?: number
  type?: string
  update?: acp.SessionUpdate
  tool_timing?: { startedAt?: string; durationMs?: number }
  stop_reason?: acp.StopReason
}

function appendSession(base: Conversation, session: SessionView): Conversation {
  const records = [...session.records].sort((a, b) => a.sequence - b.sequence)
  let conversation = base
  let index = -1 // this session's turn being built
  const startTurns = base.turns.length

  // A turn the history never closed takes its result when the next prompt or the end arrives.
  const close = (at: number) => {
    const turn = conversation.turns.at(-1)
    if (index < 0 || turn?.status !== 'streaming') return
    const result = session.results[index]
    if (result) conversation = applyEvent(conversation, endOf(result), at)
    else if (session.ended) conversation = applyEvent(conversation, { type: 'turnError', message: '会话已结束，这一轮没有完成。' }, at)
  }

  let last = 0
  for (const record of records) {
    const line = record.data as HistoryLine
    // A record still being written carries no time of its own; it is no older than the one before.
    const at = timeOf(line.at) || last
    last = at
    if (line.type === 'turnEnded') {
      if (line.stop_reason) conversation = applyEvent(conversation, { type: 'turnEnd', stopReason: line.stop_reason }, at)
      continue
    }
    if (line.type !== 'update' || !line.update) continue
    const update = line.update
    if (update.sessionUpdate === 'user_message_chunk') {
      const text = update.content.type === 'text' ? update.content.text : ''
      const last = conversation.turns.at(-1)
      if (index >= 0 && last?.status === 'streaming' && last.items.length === 0) {
        // A prompt of several blocks arrives as consecutive user chunks.
        conversation = { ...conversation, turns: [...conversation.turns.slice(0, -1), { ...last, prompt: { ...last.prompt, text: last.prompt.text + text } }] }
        continue
      }
      close(at)
      index++
      conversation = applyEvent(conversation, { type: 'prompt', turnId: `${session.id}:${record.sequence}`, text }, at)
      continue
    }
    for (const event of eventsOf(update, record.sequence, line, at)) conversation = applyEvent(conversation, event.event, event.at)
    if (update.sessionUpdate === 'agent_thought_chunk') {
      const timing = session.thoughtTimings[record.sequence]
      const duration = timing?.endedAt !== undefined ? timing.endedAt - timing.startedAt : 0
      const turn = conversation.turns.at(-1)
      if (turn) {
        const id = `thought-${update.messageId ?? `h${record.sequence}`}`
        const items = turn.items.map((item) => item.kind === 'thought' && item.id === id
          ? { ...item, durationMs: duration > 0 ? duration : null } : item)
        conversation = { ...conversation, turns: [...conversation.turns.slice(0, -1), { ...turn, items }] }
      }
    }
  }
  close(Date.now())
  // A turn stopped before its Agent started never reached the history; show it from its result.
  for (let i = index + 1; i < session.results.length; i++) {
    const at = Date.now()
    conversation = applyEvent(conversation, { type: 'prompt', turnId: `${session.id}:turn-${i}`, text: session.prompts[i] ?? '' }, at)
    conversation = applyEvent(conversation, endOf(session.results[i]), at)
  }
  // The history holds the first turn only once its Agent has started; until then it is still asked.
  if (conversation.turns.length === startTurns && !session.ended && session.preparingPrompt !== undefined) {
    const at = timeOf(session.preparation?.stages[0]?.startedAt)
    conversation = applyEvent(conversation, { type: 'prompt', turnId: `${session.id}:preparing`, text: session.preparingPrompt }, at)
  }
  return markFailures(conversation, startTurns, session.results)
}

/**
 * Settled history holds whole items, not deltas. Each message or thought gets its own id so two
 * settled runs never merge, and a settled tool is closed at the time it settled.
 */
function eventsOf(update: acp.SessionUpdate, sequence: number, line: HistoryLine, at: number): { event: ModelEvent; at: number }[] {
  switch (update.sessionUpdate) {
    case 'agent_message_chunk':
    case 'agent_thought_chunk':
      return [{ event: { type: 'update', update: { ...update, messageId: update.messageId ?? `h${sequence}` } }, at }]
    case 'tool_call': {
      const started = timeOf(line.tool_timing?.startedAt) || at
      const events: { event: ModelEvent; at: number }[] = [{ event: { type: 'update', update }, at: started }]
      if (update.status === 'completed' || update.status === 'failed') {
        const ended = line.tool_timing?.durationMs != null ? started + line.tool_timing.durationMs : at
        events.push({ event: { type: 'update', update: { sessionUpdate: 'tool_call_update', toolCallId: update.toolCallId, status: update.status } }, at: ended })
      }
      return events
    }
    default:
      return [{ event: { type: 'update', update }, at }]
  }
}

function endOf(result: ChatResult): ModelEvent {
  switch (result.status) {
    case 'completed':
      return { type: 'turnEnd', stopReason: (result.stopReason || 'end_turn') as acp.StopReason }
    case 'cancelled':
      return { type: 'turnEnd', stopReason: 'cancelled' }
    default:
      return { type: 'turnError', message: failureText(result) }
  }
}

/**
 * A turn whose authoritative result failed shows as failed even when the history closed it
 * normally: an answer that could not be read back in full is never presented as complete.
 */
function markFailures(conversation: Conversation, from: number, results: ChatResult[]): Conversation {
  let changed = false
  const turns = conversation.turns.map((turn, i): Turn => {
    const result = i >= from ? results[i - from] : undefined
    if (!result || (result.status !== 'failed' && result.status !== 'interrupted') || turn.status === 'failed') return turn
    changed = true
    return { ...turn, status: 'failed', error: failureText(result) }
  })
  return changed ? { ...conversation, turns } : conversation
}

function failureText(result: ChatResult): string {
  if (result.status === 'interrupted') return '服务重启，这一轮被中断。'
  if (!result.answerComplete) return `回答没能完整读取${result.failureCode ? `（${result.failureCode}）` : ''}，这一轮按失败处理。`
  return `这一轮失败了${result.failureCode ? `（${result.failureCode}）` : ''}。`
}

function timeOf(at: string | undefined): number {
  const t = at ? Date.parse(at) : NaN
  return Number.isNaN(t) ? 0 : t
}
