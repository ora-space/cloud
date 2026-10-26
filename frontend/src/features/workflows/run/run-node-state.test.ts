import { describe, expect, it } from 'vitest'
import { parseRunNodeStates, type RunNodeEntry } from '@/features/workflows/run/run-node-state'

describe('parseRunNodeStates', () => {
  it('returns an empty map for a run that has no trace yet', () => {
    expect(parseRunNodeStates(undefined)).toEqual({})
  })

  it('reads one node row, keeping only its recognised fields', () => {
    const states = parseRunNodeStates({
      'a-1': {
        status: 'succeeded',
        displayName: 'Agent 1',
        startedAt: '2026-09-20T10:00:00+08:00',
        finishedAt: '2026-09-20T10:00:05+08:00',
        output: { ok: true },
        extra: 'ignored',
      },
    })
    expect(states['a-1']).toEqual({
      status: 'succeeded',
      displayName: 'Agent 1',
      startedAt: '2026-09-20T10:00:00+08:00',
      finishedAt: '2026-09-20T10:00:05+08:00',
      output: { ok: true },
    } satisfies RunNodeEntry)
  })

  it('reads the attempt and retry instant of a retrying node', () => {
    const states = parseRunNodeStates({
      'a-1': {
        status: 'retrying',
        attempt: 2,
        startedAt: '2026-09-20T10:00:00+08:00',
        retryAt: '2026-09-20T10:00:30+08:00',
        error: 'agent_failed',
      },
    })
    expect(states['a-1']).toEqual({
      status: 'retrying',
      attempt: 2,
      startedAt: '2026-09-20T10:00:00+08:00',
      retryAt: '2026-09-20T10:00:30+08:00',
      error: 'agent_failed',
    } satisfies RunNodeEntry)
  })

  it('drops an attempt that is not a positive integer and a blank retry instant', () => {
    const states = parseRunNodeStates({
      'a-1': { status: 'retrying', attempt: 0, retryAt: '' },
      'a-2': { status: 'running', attempt: 1.5 },
      'a-3': { status: 'running', attempt: 'two' },
    })
    expect(states['a-1']).toEqual({ status: 'retrying' } satisfies RunNodeEntry)
    expect(states['a-2']).toEqual({ status: 'running' } satisfies RunNodeEntry)
    expect(states['a-3']).toEqual({ status: 'running' } satisfies RunNodeEntry)
  })

  it('keeps an explicitly falsy output when the row carries one', () => {
    const states = parseRunNodeStates({ 'a-1': { status: 'failed', output: 0 } })
    expect(states['a-1']?.output).toBe(0)
  })

  it('drops rows without a recognisable status', () => {
    const states = parseRunNodeStates({
      'a-1': 'running',
      'a-2': { phase: 'booting' },
      'a-3': { status: 'exploded' },
      'a-4': [1, 2],
      'a-5': null,
    })
    expect(states).toEqual({})
  })

  it('omits optional fields the row never set', () => {
    const states = parseRunNodeStates({ 'a-1': { status: 'idle' } })
    expect(states['a-1']).toEqual({ status: 'idle' } satisfies RunNodeEntry)
  })

  it('treats a blank timestamp as absent', () => {
    const states = parseRunNodeStates({
      'a-1': { status: 'inactive', startedAt: '', finishedAt: '' },
      'a-2': { status: 'running', startedAt: '2026-09-20T10:00:00+08:00', finishedAt: '' },
    })
    expect(states['a-1']).toEqual({ status: 'inactive' } satisfies RunNodeEntry)
    expect(states['a-2']).toEqual({
      status: 'running',
      startedAt: '2026-09-20T10:00:00+08:00',
    } satisfies RunNodeEntry)
  })
})
