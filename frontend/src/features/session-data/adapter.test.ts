import { describe, expect, it } from 'vitest'
import { adaptArchive, adaptThread } from './index'

const AT = '2025-01-02T03:04:05.000Z'
const START = '2025-01-02T03:04:04.000Z'
const RUN = 'synthetic-run'

function message(seq: number, text: string, sessionUpdate = 'agent_message_chunk') {
  return { seq, at: AT, type: 'update', update: { sessionUpdate, content: { type: 'text', text } } }
}

function tool(seq: number, timing: unknown, status = 'completed') {
  return {
    seq,
    at: AT,
    type: 'update',
    tool_timing: timing,
    update: {
      sessionUpdate: 'tool_call',
      toolCallId: `tool-${seq}`,
      kind: 'read',
      title: 'Read fixture',
      status,
      rawInput: { path: 'synthetic.txt' },
      rawOutput: { output: 'synthetic result', metadata: { truncated: false } },
    },
  }
}

function thread(seq: number, record: unknown) {
  return { seq, source: 'node', kind: 'update', record, createdAt: '2025-02-01T00:00:00Z' }
}

describe('archive session observations', () => {
  it('replays the last physical seq version and keeps the original rows without mutation', () => {
    const records = [
      message(2, 'old'),
      message(0, 'prompt', 'user_message_chunk'),
      message(2, 'final'),
    ]
    const copy = structuredClone(records)
    const model = adaptArchive({ runId: RUN, records })
    expect(model.events.map((event) => [event.seq, event.text])).toEqual([
      [0, 'prompt'],
      [2, 'final'],
    ])
    expect(model.rawRecords).toEqual(records)
    expect(model.stats.replacedRecordCount).toBe(1)
    expect(records).toEqual(copy)
    expect(model.events[1]?.turn).toEqual({
      id: model.events[0]?.id,
      ordinal: 1,
      source: 'archive_user_marker',
    })
    const other = adaptArchive({ runId: 'different-run', records })
    expect(other.events[0]?.id).not.toBe(model.events[0]?.id)
  })

  it('retains thought lifecycle gap and unknown observations outside the three lanes', () => {
    const records = [
      message(0, 'thinking', 'agent_thought_chunk'),
      { seq: 1, type: 'meta' },
      { seq: 2, type: 'turnEnded' },
      { seq: 3, type: 'gap', extra: [1, null] },
      message(4, 'future', 'future_variant'),
    ]
    const model = adaptArchive({ runId: RUN, records })
    expect(model.events.map((event) => event.category)).toEqual([
      'thought',
      'metadata',
      'lifecycle',
      'gap',
      'unknown',
    ])
    expect(model.events.map((event) => event.raw)).toEqual(records)
    expect(model.timeline).toEqual({
      events: [],
      minTimestampMs: null,
      maxTimestampMs: null,
      observedSpanMs: null,
    })
    expect(model.diagnostics.map((item) => item.code)).toEqual(['history_gap', 'unknown_event'])
    expect(model.stats.thoughtCount).toBe(1)
    expect(model.stats.llmCallCount).toBeNull()
  })

  it('retains malformed input as unknown instead of silently dropping it', () => {
    const records: unknown[] = [
      null,
      'unparsed',
      { seq: -1, type: 'meta' },
      { seq: 1.5, type: 'meta' },
    ]
    const model = adaptArchive({ runId: RUN, records })
    expect(model.events).toHaveLength(4)
    expect(model.rawRecords).toEqual(records)
    expect(model.events.every((event) => event.seq === null && event.category === 'unknown')).toBe(
      true,
    )
    expect(new Set(model.events.map((event) => event.id)).size).toBe(4)
    expect(model.diagnostics.some((item) => item.code === 'invalid_record')).toBe(true)
    expect(() => adaptArchive({ runId: ' ', records })).toThrow('Session run identity is required')
  })

  it('preserves unknown time in timeline rows and validates timezone and calendar dates', () => {
    const invalid = [
      undefined,
      null,
      0,
      'bad',
      '2025-02-30T10:00:00Z',
      '2025-01-02T10:00:00',
      '2025-13-02T10:00:00Z',
      '2025-01-02T25:00:00Z',
      '2025-01-02T10:00:00+25:00',
    ]
    const records = invalid.map((at, seq) => ({ ...message(seq, 'text'), at }))
    const model = adaptArchive({ runId: RUN, records })
    expect(model.timeline.events).toHaveLength(invalid.length)
    expect(model.timeline.events.every((event) => event.startMs === null)).toBe(true)
    expect(model.timeline.minTimestampMs).toBeNull()
    expect(model.timeline.observedSpanMs).toBeNull()
    const precise = '2025-01-02T11:04:05.123456789+08:00'
    const known = adaptArchive({ runId: RUN, records: [{ ...message(0, 'text'), at: precise }] })
    expect(known.events[0]?.timestamp).toBe(precise)
    expect(known.events[0]?.timestampMs).toBe(Date.parse(precise))
  })

  it('keeps usage cost and latency unknown and uses only a validated frozen model id', () => {
    const model = adaptArchive({
      runId: RUN,
      records: [message(0, 'model prose is not identity')],
      frozenModel: { id: 'provider/synthetic-model' },
    })
    expect(model.frozenModel).toEqual({
      id: 'provider/synthetic-model',
      source: 'frozen_run_binding',
    })
    expect(model.events[0]).toMatchObject({
      model: model.frozenModel,
      usage: null,
      costUsd: null,
      latencyMs: null,
      turn: null,
    })
    expect(model.stats).toMatchObject({
      inputTokens: null,
      outputTokens: null,
      costUsd: null,
      llmCallCount: null,
      latencyMs: null,
      assistantTextCount: 1,
    })
    for (const id of [undefined, null, '', ' invalid ', 'model\nline', 4]) {
      expect(adaptArchive({ runId: RUN, records: [], frozenModel: { id } }).frozenModel).toBeNull()
    }
  })
})

describe('reported tool observations', () => {
  it('projects an archive at the server record limit without exhausting the argument stack', () => {
    const records = Array.from({ length: 100_000 }, (_, seq) =>
      tool(seq, { startedAt: START, durationMs: seq }),
    )
    const model = adaptArchive({ runId: RUN, records })
    expect(model.timeline.events).toHaveLength(records.length)
    expect(model.timeline.minTimestampMs).toBe(Date.parse(START))
    expect(model.timeline.maxTimestampMs).toBe(Date.parse(START) + records.length - 1)
    expect(model.timeline.observedSpanMs).toBe(records.length - 1)
    expect(model.tools[0]?.maxDurationMs).toBe(records.length - 1)
  })

  it('retains raw payloads failure and measured zero while calculating coverage-aware durations', () => {
    const records = [
      tool(0, { startedAt: START, durationMs: 0 }),
      tool(1, { startedAt: START, durationMs: 2000 }, 'failed'),
      tool(2, { startedAt: START }, 'in_progress'),
      tool(3, null, 'unrecognized'),
    ]
    const model = adaptArchive({ runId: RUN, records })
    expect(model.events[0]?.tool).toMatchObject({
      rawInput: records[0]?.update.rawInput,
      rawOutput: records[0]?.update.rawOutput,
      nativeName: null,
      durationMs: 0,
      endedAtMs: Date.parse(START),
      rawInputPresent: true,
      rawOutputPresent: true,
    })
    expect(model.events[1]?.tool?.isError).toBe(true)
    expect(model.events[2]?.tool?.isError).toBeNull()
    expect(model.tools).toEqual([
      {
        kind: 'read',
        count: 4,
        completedCount: 1,
        failedCount: 1,
        incompleteCount: 1,
        unknownStatusCount: 1,
        durationKnownCount: 2,
        totalDurationMs: 2000,
        averageDurationMs: 1000,
        maxDurationMs: 2000,
      },
    ])
    expect(model.timeline.maxTimestampMs).toBe(Date.parse(START) + 2000)
    expect(model.timeline.events[3]?.startMs).toBeNull()
    expect(model.timeline.events[0]?.durationMs).toBe(0)
  })

  it('keeps failed text and explicit empty output distinct from absent result and timing', () => {
    const failure = {
      ...tool(0, { durationMs: -1 }, 'failed'),
      update: {
        sessionUpdate: 'tool_call',
        status: 'failed',
        rawOutput: { error: 'synthetic error' },
      },
    }
    const empty = {
      ...tool(1, { durationMs: Number.NaN }),
      update: { sessionUpdate: 'tool_call', rawOutput: { output: '' } },
    }
    const absent = {
      ...tool(2, null),
      update: {
        sessionUpdate: 'tool_call_update',
        content: [{ type: 'content', content: { type: 'text', text: 'observed' } }],
      },
    }
    const model = adaptArchive({ runId: RUN, records: [failure, empty, absent] })
    expect(model.events.map((event) => event.tool?.output)).toEqual([
      'synthetic error',
      '',
      'observed',
    ])
    expect(model.events.map((event) => event.tool?.durationMs)).toEqual([null, null, null])
    expect(model.events[2]?.tool?.rawOutputPresent).toBe(false)
    expect(model.tools[0]).toMatchObject({
      kind: 'unknown',
      durationKnownCount: 0,
      averageDurationMs: null,
      totalDurationMs: null,
      maxDurationMs: null,
    })
  })
})

describe('public Thread snapshots', () => {
  it('uses public cursors independently of repeated archive seq and never invents submission time', () => {
    const prompt = {
      seq: 1,
      source: 'system',
      kind: 'user_turn',
      createdAt: AT,
      record: { content: 'prompt' },
    }
    const entries = [
      thread(3, message(0, 'response')),
      prompt,
      thread(2, message(0, 'thought', 'agent_thought_chunk')),
    ]
    const model = adaptThread({ runId: RUN, entries })
    expect(model.events.map((event) => [event.seq, event.archiveSeq])).toEqual([
      [1, null],
      [2, 0],
      [3, 0],
    ])
    expect(model.events[0]).toMatchObject({
      category: 'user',
      timestamp: null,
      timestampMs: null,
      createdAt: AT,
      text: 'prompt',
    })
    expect(model.timeline.events[0]?.startMs).toBeNull()
    expect(model.events[2]?.explicitTurnId).toBeNull()
    expect(model.events[2]?.turn).toEqual({
      id: model.events[0]?.id,
      ordinal: 1,
      source: 'thread_user_marker',
    })
    expect(model.rawRecords).toEqual(entries)
  })

  it('replays overlapping public rows and keeps echo records separate from user markers', () => {
    const user = {
      seq: 1,
      source: 'user',
      kind: 'user_turn',
      turnId: 'explicit-turn',
      status: 'queued',
      record: { content: [{ type: 'text', text: 'prompt' }] },
    }
    const entries = [
      user,
      thread(2, message(1, 'prompt', 'user_message_chunk')),
      { ...user, status: 'delivered' },
    ]
    const model = adaptThread({ runId: RUN, entries })
    expect(model.events.map((event) => event.category)).toEqual(['user', 'user_echo'])
    expect(model.stats.userMessageCount).toBe(1)
    expect(model.stats.explicitUserMarkerCount).toBe(1)
    expect(model.events[0]?.explicitTurnId).toBe('explicit-turn')
    expect(model.events[1]?.explicitTurnId).toBeNull()
    expect(model.stats.replacedRecordCount).toBe(1)
    expect(model.timeline.events).toHaveLength(1)
  })

  it('retains invalid public envelopes without trusting internal field names', () => {
    const model = adaptThread({
      runId: RUN,
      entries: [
        { seq: 0, source: 'node', record: message(5, 'text') },
        { seq: 1, source: 'unexpected', record: {} },
        { seq: 2, source: 'node', record: null },
        { ...thread(3, message(7, 'text')), turn_id: 'private-field', created_at: AT },
      ],
    })
    expect(model.events).toHaveLength(4)
    expect(model.events.filter((event) => event.category === 'unknown')).toHaveLength(3)
    expect(model.events.find((event) => event.seq === 3)?.explicitTurnId).toBeNull()
    expect(model.diagnostics.some((item) => item.code === 'invalid_record')).toBe(true)
  })
})
