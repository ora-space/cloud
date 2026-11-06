import { describe, expect, it } from 'vitest'
import type { SessionEvent as ChatEvent, SessionHistoryRecord as ChatHistoryRecord, SessionResult as ChatResult } from './wire'
import { applySessionEvent, conversationOf, emptySessionView, type SessionView } from './history'

const at = (s: number) => `2026-10-30T09:00:${String(s).padStart(2, '0')}Z`
const ms = (s: number) => Date.parse(at(s))

const user = (seq: number, text: string): ChatHistoryRecord => ({
  sequence: seq,
  data: { at: at(seq), seq, type: 'update', update: { sessionUpdate: 'user_message_chunk', content: { type: 'text', text } } },
})
const say = (seq: number, text: string, preview = false): ChatHistoryRecord => ({
  sequence: seq,
  data: { at: at(seq), seq, type: 'update', update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } } },
  ...(preview ? { preview } : {}),
})
const tool = (seq: number, status: 'completed' | 'in_progress'): ChatHistoryRecord => ({
  sequence: seq,
  data: {
    at: at(seq + 5),
    seq,
    type: 'update',
    update: { sessionUpdate: 'tool_call', toolCallId: `call-${seq}`, title: '读取 README.md', kind: 'read', status },
    tool_timing: { startedAt: at(seq), durationMs: 2000 },
  },
})
/** A record Node is still writing: a bare HistoryRecord with no at or seq. */
const pending = (seq: number, update: Record<string, unknown>, extra: Record<string, unknown> = {}): ChatHistoryRecord => ({
  sequence: seq,
  data: { type: 'update', update, ...extra },
  preview: true,
})
const ended = (seq: number, stop = 'end_turn'): ChatHistoryRecord => ({ sequence: seq, data: { at: at(seq), seq, type: 'turnEnded', stop_reason: stop } })
const meta: ChatHistoryRecord = { sequence: 0, data: { at: at(0), seq: 0, type: 'meta', schemaVersion: 2, sessionId: 'x' } }
const done: ChatResult = { status: 'completed', answer: '', answerComplete: true, stopReason: 'end_turn', failureCode: '' }

const view = (patch: Partial<SessionView>): SessionView => ({ ...emptySessionView('s1'), ...patch })

describe('applySessionEvent', () => {
  it('replaces the whole history on each snapshot and keeps turn results in order', () => {
    let sequence = 0
    const event = (e: Partial<ChatEvent>): ChatEvent => ({ sequence: ++sequence, occurredAt: at(0), kind: 'history_snapshot', turnId: 't1', ...e })
    let v = emptySessionView('s1')
    v = applySessionEvent(v, event({ history: { records: [user(1, 'hi'), say(2, '草稿', true)], incomplete: true } }))
    v = applySessionEvent(v, event({ history: { records: [user(1, 'hi'), say(2, '定稿')], incomplete: false } }))
    v = applySessionEvent(v, event({ kind: 'turn_ended', result: done }))
    v = applySessionEvent(v, event({ kind: 'session_ended' }))
    expect(v.records.map((r) => r.data)).toEqual([user(1, 'hi').data, say(2, '定稿').data])
    expect(v).toMatchObject({ incomplete: false, results: [done], ended: true, cursor: 4 })
    // An event already folded in (a replay after following again) changes nothing.
    expect(applySessionEvent(v, { sequence: 3, occurredAt: at(0), kind: 'turn_ended', turnId: 't1', result: done })).toBe(v)
  })
})

describe('applySessionEvent with previews', () => {
  it('replaces only the pending records, and a settled record at the same position wins', () => {
    let sequence = 0
    const event = (e: Partial<ChatEvent>): ChatEvent => ({ sequence: ++sequence, occurredAt: at(0), kind: 'history_preview', turnId: 't1', ...e })
    const draft = (text: string) => pending(2, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } })
    let v = applySessionEvent(emptySessionView('s1'), event({ kind: 'history_snapshot', history: { records: [user(1, 'hi')], incomplete: false } }))
    v = applySessionEvent(v, event({ history: { records: [draft('定')], incomplete: false } }))
    v = applySessionEvent(v, event({ history: { records: [draft('定稿')], incomplete: false } }))
    expect(v.records).toEqual([user(1, 'hi'), draft('定稿')])
    v = applySessionEvent(v, event({ kind: 'history_snapshot', history: { records: [user(1, 'hi'), say(2, '定稿')], incomplete: false } }))
    // A preview that raced the settled record cannot bring the draft back.
    v = applySessionEvent(v, event({ history: { records: [draft('定')], incomplete: false } }))
    expect(v.records).toEqual([user(1, 'hi'), say(2, '定稿')])
    v = applySessionEvent(v, event({ history: { records: [], incomplete: false } }))
    expect(v.records).toEqual([user(1, 'hi'), say(2, '定稿')])
  })

  it('keeps position order when a pending record sits before a newly settled one', () => {
    let sequence = 0
    const event = (e: Partial<ChatEvent>): ChatEvent => ({ sequence: ++sequence, occurredAt: at(0), kind: 'history_preview', turnId: 't1', ...e })
    const thought = pending(2, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: '先想想' } })
    // Record 3 settled while record 2, which Node no longer previews, is still pending.
    let v = applySessionEvent(emptySessionView('s1'), event({ kind: 'history_snapshot', history: { records: [user(1, 'hi'), thought, say(3, '答')], incomplete: false } }))
    v = applySessionEvent(v, event({ history: { records: [thought], incomplete: false } }))
    expect(v.records).toEqual([user(1, 'hi'), thought, say(3, '答')])
  })
})

describe('reasoning durations from retained events', () => {
  const thought = (seq: number, preview = true, source?: string): ChatHistoryRecord => ({
    sequence: seq, preview,
    data: { type: 'update', ...(!preview && { at: at(20) }), update: {
      sessionUpdate: 'agent_thought_chunk', messageId: `thought-${seq}`, content: { type: 'text', text: '先想一想' },
      ...(source && { _meta: { 'opencode/child-session': { id: source, parentID: 'parent' } } }),
    } },
  })
  const event = (sequence: number, seconds: number, records: ChatHistoryRecord[], kind: ChatEvent['kind'] = 'history_preview'): ChatEvent => ({
    sequence, occurredAt: at(seconds), kind, turnId: 't1', history: { records, incomplete: false },
  })
  const duration = (v: SessionView) => conversationOf([v]).turns[0].items.find((item) => item.kind === 'thought')?.durationMs

  it('keeps the observed interval across previews, a batch-settled snapshot, duplicate events and cold replay', () => {
    const events = [
      event(1, 1, [user(1, 'hi')], 'history_snapshot'),
      event(2, 3, [thought(2)]),
      event(3, 6, [thought(2)]),
      event(4, 8, [thought(2), pending(3, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: '你好' } })]),
      // Node writes the whole batch with at=20s; these are append times, not reasoning times.
      event(5, 20, [user(1, 'hi'), thought(2, false), { ...say(3, '你好'), data: { ...say(3, '你好').data, at: at(20) } }, ended(20)], 'history_snapshot'),
    ]
    const streaming = events.slice(0, 4).reduce(applySessionEvent, emptySessionView('s1'))
    expect(duration(streaming)).toBe(5000)
    const settled = applySessionEvent(streaming, events[4])
    expect(duration(settled)).toBe(5000)
    expect(applySessionEvent(settled, events[4])).toBe(settled)
    expect(duration(events.reduce(applySessionEvent, emptySessionView('s1')))).toBe(5000)
  })

  it('does not infer a duration from settled-only history or a late preview of that history', () => {
    const settled = applySessionEvent(emptySessionView('s1'), event(1, 20, [user(1, 'hi'), thought(2, false), say(3, '你好'), ended(20)], 'history_snapshot'))
    expect(duration(settled)).toBeNull()
    const late = applySessionEvent(settled, event(2, 21, [thought(2)]))
    expect(late.thoughtTimings).toEqual({})
    expect(duration(late)).toBeNull()
  })

  it('does not invent an interval when reasoning and the answer are first seen together', () => {
    const v = applySessionEvent(emptySessionView('s1'), event(1, 8, [user(1, 'hi'), thought(2), say(3, '你好')]))
    expect(duration(v)).toBeNull()
  })

  it('does not end reasoning on an empty answer delta', () => {
    let v = applySessionEvent(emptySessionView('s1'), event(1, 2, [user(1, 'hi'), thought(2)]))
    v = applySessionEvent(v, event(2, 3, [thought(2), pending(3, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: '  ' } })]))
    expect(v.thoughtTimings[2].endedAt).toBeUndefined()
    v = applySessionEvent(v, event(3, 7, [thought(2), say(3, '你好')]))
    expect(duration(v)).toBe(5000)
  })

  it('ignores another source moving on, and closes at a tool from the same source', () => {
    let v = applySessionEvent(emptySessionView('s1'), event(1, 2, [user(1, 'hi'), thought(2)]))
    v = applySessionEvent(v, event(2, 4, [thought(3, true, 'child')]))
    expect(v.thoughtTimings[2].endedAt).toBeUndefined()
    v = applySessionEvent(v, event(3, 7, [tool(4, 'in_progress')]))
    expect(v.thoughtTimings[2]).toMatchObject({ startedAt: ms(2), endedAt: ms(7) })
    expect(v.thoughtTimings[3].endedAt).toBeUndefined()
  })

  it.each(['turn_ended', 'session_ended'] as const)('closes on %s without using the browser clock', (kind) => {
    let v = applySessionEvent(emptySessionView('s1'), event(1, 2, [user(1, 'hi'), thought(2)]))
    v = applySessionEvent(v, { sequence: 2, occurredAt: at(5), kind, turnId: 't1', result: { ...done, status: 'cancelled', stopReason: 'cancelled' } })
    expect(duration(v)).toBe(3000)
  })

  it('does not change earlier intervals in later turns or accept invalid event times', () => {
    let v = applySessionEvent(emptySessionView('s1'), event(1, 2, [user(1, 'hi'), thought(2)]))
    v = applySessionEvent(v, event(2, 5, [ended(3)]))
    v = applySessionEvent(v, event(3, 7, [user(4, 'again'), thought(5)]))
    v = applySessionEvent(v, { ...event(4, 9, [say(6, '你好')]), occurredAt: 'invalid' })
    expect(v.thoughtTimings[5].endedAt).toBeUndefined()
    v = applySessionEvent(v, event(5, 10, [say(6, '你好')]))
    expect(v.thoughtTimings[2]).toMatchObject({ startedAt: ms(2), endedAt: ms(5) })
    expect(v.thoughtTimings[5]).toMatchObject({ startedAt: ms(7), endedAt: ms(10) })
  })
})

describe('conversationOf', () => {
  it('rebuilds turns from settled history in position order', () => {
    const c = conversationOf([view({ records: [ended(5), say(4, '第二段'), tool(2, 'completed'), user(1, '读一下'), say(3, '第一段'), meta], results: [done] })])
    expect(c.turns).toHaveLength(1)
    const [turn] = c.turns
    expect(turn).toMatchObject({ id: 's1:1', prompt: { text: '读一下' }, status: 'completed', stopReason: 'end_turn' })
    // Two settled messages stay two items; they are never glued into one.
    expect(turn.items.map((i) => i.kind)).toEqual(['tool', 'message', 'message'])
    expect(turn.items[0]).toMatchObject({ status: 'completed', startedAt: ms(2), endedAt: ms(4) })
  })

  it('shows pending text and a running tool while the turn streams, timed no earlier than the record before', () => {
    const records = [
      meta,
      user(1, '跑一下测试'),
      pending(2, { sessionUpdate: 'agent_thought_chunk', content: { type: 'text', text: '先看看' } }),
      pending(3, { sessionUpdate: 'tool_call', toolCallId: 'call-3', title: 'sleep 12', kind: 'execute', status: 'in_progress' }, { tool_timing: { startedAt: at(4) } }),
      pending(4, { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text: '正在' } }),
    ]
    const [turn] = conversationOf([view({ records })]).turns
    expect(turn.status).toBe('streaming')
    expect(turn.items.map((i) => i.kind)).toEqual(['thought', 'tool', 'message'])
    expect(turn.items[0]).toMatchObject({ text: '先看看', startedAt: ms(1), endedAt: ms(4) })
    expect(turn.items[1]).toMatchObject({ status: 'in_progress', startedAt: ms(4) })
    expect(turn.items[1]).not.toHaveProperty('endedAt')
    expect(turn.items[2]).toMatchObject({ text: '正在' })
  })

  it('closes a turn the history left open with its result, and fails one with no result once the session ended', () => {
    const cancelled: ChatResult = { ...done, status: 'cancelled', stopReason: 'cancelled' }
    const c = conversationOf([view({ records: [user(1, 'a'), tool(2, 'in_progress'), user(3, 'b')], results: [cancelled], ended: true, endReason: 'stopped' })])
    expect(c.turns.map((t) => t.status)).toEqual(['cancelled', 'failed'])
    expect(c.turns[0].items[0]).toMatchObject({ kind: 'tool', status: 'cancelled' })
  })

  it('never shows an answer that could not be read back in full as complete', () => {
    const failed: ChatResult = { status: 'failed', answer: '', answerComplete: false, stopReason: 'end_turn', failureCode: 'history_limit' }
    const [turn] = conversationOf([view({ records: [user(1, 'a'), say(2, '前半段'), ended(3)], results: [failed] })]).turns
    expect(turn.status).toBe('failed')
    expect(turn.error).toContain('history_limit')
  })

  it('marks where an ended session stops the conversation, as interrupted when chat did not end it', () => {
    const stopped = conversationOf([view({ id: 's1', records: [user(1, 'a'), ended(2)], results: [done], ended: true, endReason: 'stopped' })])
    expect(stopped.markers).toEqual([{ kind: 'session', id: 'session-s1', afterTurn: 1, reason: 'stopped' }])
    const restarted = conversationOf([view({ id: 's1', records: [user(1, 'a'), ended(2), user(3, 'b'), ended(4)], results: [done, done], ended: true })])
    expect(restarted.turns.map((t) => t.status)).toEqual(['completed', 'completed'])
    expect(restarted.markers).toEqual([{ kind: 'session', id: 'session-s1', afterTurn: 2, reason: 'interrupted' }])
    expect(conversationOf([view({ id: 's1', records: [user(1, 'a')] })]).markers).toEqual([])
  })

  it('shows a turn stopped before its Agent started, which no history snapshot holds', () => {
    const cancelled: ChatResult = { ...done, status: 'cancelled', stopReason: 'cancelled' }
    let v = emptySessionView('s1')
    v = applySessionEvent(v, { sequence: 1, occurredAt: at(0), kind: 'turn_ended', turnId: 't1', result: cancelled, prompt: '停下来' })
    v = applySessionEvent(v, { sequence: 2, occurredAt: at(0), kind: 'session_ended', turnId: '' })
    const c = conversationOf([{ ...v, endReason: 'stopped' }])
    expect(c.turns.map((t) => [t.prompt.text, t.status])).toEqual([['停下来', 'cancelled']])
    expect(c.markers).toEqual([{ kind: 'session', id: 'session-s1', afterTurn: 1, reason: 'stopped' }])
    // A turn the history does hold is not shown twice.
    const recorded = conversationOf([{ ...v, records: [user(1, '停下来')], endReason: 'stopped' }])
    expect(recorded.turns).toHaveLength(1)
  })

  it('joins a prompt sent as several blocks', () => {
    const [turn] = conversationOf([view({ records: [user(1, '第一行'), user(2, '第二行')] })]).turns
    expect(turn.prompt.text).toBe('第一行第二行')
  })
})

describe('preparation', () => {
  const event = (sequence: number, stages: { id: 'runtime' | 'agent'; startedAt: string; endedAt?: string }[]): ChatEvent => ({
    sequence, occurredAt: at(sequence), kind: 'preparation', turnId: 't1', preparation: { stages },
  })

  it('keeps the latest stages, so a replayed stream restores them', () => {
    const first = applySessionEvent(view({}), event(1, [{ id: 'runtime', startedAt: at(0) }]))
    const next = applySessionEvent(first, event(2, [{ id: 'runtime', startedAt: at(0), endedAt: at(1) }, { id: 'agent', startedAt: at(1) }]))
    expect(next.preparation?.stages.map((stage) => stage.id)).toEqual(['runtime', 'agent'])
    expect(applySessionEvent(next, event(2, []))).toBe(next)
  })

  it('reaches the conversation before any history exists', () => {
    const session = applySessionEvent(view({}), event(1, [{ id: 'runtime', startedAt: at(0) }]))
    expect(conversationOf([session]).preparation?.stages).toHaveLength(1)
  })

  const asked = (sequence: number, prompt?: string): ChatEvent => ({ ...event(sequence, [{ id: 'runtime', startedAt: at(0) }]), prompt })

  it('asks the first turn from preparation, before any history exists', () => {
    const { turns } = conversationOf([applySessionEvent(view({}), asked(1, '修复登录页'))])
    expect(turns.map((turn) => [turn.prompt.text, turn.status])).toEqual([['修复登录页', 'streaming']])
    expect(turns[0]!.startedAt).toBe(Date.parse(at(0)))
  })

  it('keeps the prompt when a later stage could not read it', () => {
    const session = [asked(1, '修复登录页'), asked(2)].reduce(applySessionEvent, view({}))
    expect(conversationOf([session]).turns[0]?.prompt.text).toBe('修复登录页')
  })

  it('leaves the turn to the history once it arrives, so it never shows twice', () => {
    const prompted = applySessionEvent(view({}), asked(1, '修复登录页'))
    const record = { sequence: 1, data: { type: 'update', at: at(1), update: { sessionUpdate: 'user_message_chunk', content: { type: 'text', text: '修复登录页' } } } }
    const session = applySessionEvent(prompted, { sequence: 2, occurredAt: at(2), kind: 'history_snapshot', turnId: 't1', history: { records: [record], incomplete: false } })
    expect(conversationOf([session]).turns.map((turn) => turn.prompt.text)).toEqual(['修复登录页'])
  })

  it('invents no turn when the prompt is unknown or the session already ended', () => {
    expect(conversationOf([applySessionEvent(view({}), event(1, [{ id: 'runtime', startedAt: at(0) }]))]).turns).toEqual([])
    expect(conversationOf([{ ...applySessionEvent(view({}), asked(1, '修复登录页')), ended: true }]).turns).toEqual([])
  })
})
