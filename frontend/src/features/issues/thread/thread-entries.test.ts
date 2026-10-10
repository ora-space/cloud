import { AxiosError, AxiosHeaders } from 'axios'
import { describe, expect, it } from 'vitest'
import type { ThreadEntry } from '@/api/generated.schemas'
import type { IssueRun } from '@/features/issues/types'
import {
  endFailureMessage,
  forwardCursor,
  sendFailureMessage,
  THREAD_DECLARE_POLL_MS,
  THREAD_LIVE_POLL_MS,
  threadPollInterval,
} from './thread-api'
import {
  isThreadClosed,
  latestAgentRun,
  mergeEntries,
  threadItems,
  threadStateLabel,
  userStatusLabel,
} from './thread-entries'

function entry(seq: number, overrides: Partial<ThreadEntry> = {}): ThreadEntry {
  return {
    seq,
    kind: 'update',
    source: 'node',
    record: {},
    createdAt: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

function chunk(seq: number, text: string, turnId: string): ThreadEntry {
  return entry(seq, {
    turnId,
    record: { update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } } },
  })
}

function run(id: string, executorType: IssueRun['executorType'], createdAt: string): IssueRun {
  return {
    id,
    tenantId: 't1',
    issueId: 'i1',
    executorType,
    executorId: 'e1',
    input: {},
    status: 'running',
    createdAt,
    updatedAt: createdAt,
  }
}

function fault(code: string, status: number): AxiosError {
  return new AxiosError('rejected', String(status), undefined, undefined, {
    status,
    statusText: '',
    headers: {},
    config: { headers: new AxiosHeaders() },
    data: { code, params: {}, requestId: 'r' },
  })
}

describe('latestAgentRun', () => {
  it('picks the most recently created agent run and ignores other executors', () => {
    const runs = [
      run('old', 'agent', '2026-10-01T00:00:00Z'),
      run('wf', 'workflow', '2026-10-03T00:00:00Z'),
      run('new', 'agent', '2026-10-02T00:00:00Z'),
    ]
    expect(latestAgentRun(runs)?.id).toBe('new')
    expect(latestAgentRun([run('wf', 'workflow', '2026-10-03T00:00:00Z')])).toBeUndefined()
  })
})

describe('mergeEntries', () => {
  it('unions windows by seq, ascending, letting the re-read copy win', () => {
    const queued = entry(2, { source: 'user', kind: 'user_turn', status: 'queued' })
    const delivered = { ...queued, status: 'delivered' as const }
    expect(mergeEntries([entry(1), queued], [delivered, entry(3)])).toEqual([
      entry(1),
      delivered,
      entry(3),
    ])
    expect(mergeEntries([entry(3)], [entry(1), entry(2)]).map((e) => e.seq)).toEqual([1, 2, 3])
  })
})

describe('threadItems', () => {
  it('renders the prompt, user turns, merged agent chunks, notes and separators', () => {
    const entries = [
      entry(1, { source: 'system', kind: 'user_turn', record: { content: 'Fix the login' } }),
      entry(2, { kind: 'meta', record: { sessionId: 's' } }),
      chunk(3, 'Hel', 't1'),
      chunk(4, 'lo', 't1'),
      entry(5, { record: { update: { sessionUpdate: 'tool_call' } } }),
      entry(6, { kind: 'turnEnded', turnId: 't1' }),
      entry(7, {
        source: 'user',
        kind: 'user_turn',
        status: 'queued',
        record: { content: [{ type: 'text', text: 'Now add tests' }, { type: 'image' }] },
      }),
      entry(8, { record: { update: { sessionUpdate: 'user_message_chunk' } } }),
      chunk(9, 'On it', 't2'),
      chunk(10, 'Other turn', 't3'),
      entry(11, { kind: 'gap', record: {} }),
      entry(12, { record: { update: 'malformed' } }),
    ]
    expect(threadItems(entries)).toEqual([
      { variant: 'prompt', key: 1, text: 'Fix the login' },
      { variant: 'note', key: 2, text: '会话已开始' },
      { variant: 'agent', key: 3, text: 'Hello', turnId: 't1' },
      { variant: 'note', key: 5, text: 'tool_call' },
      { variant: 'separator', key: 6 },
      { variant: 'user', key: 7, text: 'Now add tests', status: '排队中' },
      { variant: 'agent', key: 9, text: 'On it', turnId: 't2' },
      { variant: 'agent', key: 10, text: 'Other turn', turnId: 't3' },
      { variant: 'note', key: 11, text: 'gap' },
      { variant: 'note', key: 12, text: 'update' },
    ])
  })

  it('tolerates content that is a single block or not text at all', () => {
    const items = threadItems([
      entry(1, {
        source: 'user',
        kind: 'user_turn',
        record: { content: { type: 'text', text: 'one' } },
      }),
      entry(2, { source: 'user', kind: 'user_turn', record: { content: 42 } }),
    ])
    expect(items.map((item) => ('text' in item ? item.text : ''))).toEqual(['one', ''])
  })
})

describe('labels', () => {
  it('names every thread state and user-turn status in Chinese', () => {
    expect(
      (['pending', 'active', 'idle', 'ending', 'ended'] as const).map(threadStateLabel),
    ).toEqual(['等待中', '进行中', '空闲', '结束中', '已结束'])
    expect(['queued', 'delivered', 'discarded', 'other'].map(userStatusLabel)).toEqual([
      '排队中',
      '已送达',
      '已丢弃',
      'other',
    ])
    expect(userStatusLabel(null)).toBeUndefined()
  })

  it('treats only ending and ended as closed', () => {
    expect((['pending', 'active', 'idle', 'ending', 'ended'] as const).map(isThreadClosed)).toEqual(
      [false, false, false, true, true],
    )
  })
})

describe('forwardCursor', () => {
  it('reads after the last seq, or from just before the oldest queued user turn', () => {
    expect(forwardCursor([])).toBe(0)
    expect(forwardCursor([entry(4), entry(5)])).toBe(5)
    expect(
      forwardCursor([
        entry(4),
        entry(5, { source: 'user', status: 'queued' }),
        entry(6),
        entry(7, { source: 'user', status: 'queued' }),
      ]),
    ).toBe(4)
  })
})

describe('threadPollInterval', () => {
  it('polls fast while undeclared, slowly while live and stops once ended', () => {
    expect(threadPollInterval(undefined)).toBe(false)
    expect(threadPollInterval({ declared: false })).toBe(THREAD_DECLARE_POLL_MS)
    const details = { initiatorUserId: null, model: null, canAppend: false, canEnd: false }
    expect(
      threadPollInterval({ declared: true, entries: [], threadState: 'idle', ...details }),
    ).toBe(THREAD_LIVE_POLL_MS)
    expect(
      threadPollInterval({ declared: true, entries: [], threadState: 'ended', ...details }),
    ).toBe(false)
  })
})

describe('failure messages', () => {
  it('maps the send and end faults to readable sentences', () => {
    expect(sendFailureMessage(fault('thread_closed', 409))).toBe('会话已结束，无法继续发送')
    expect(sendFailureMessage(fault('content_too_large', 400))).toBe('消息过长')
    expect(sendFailureMessage(fault('model_run_initiator_required', 403))).toBe(
      '只有运行发起者可以继续发送模型请求',
    )
    expect(sendFailureMessage(fault('model_connection_unavailable', 409))).toBe(
      '此会话的模型授权已失效，请结束后重新发起任务',
    )
    expect(sendFailureMessage(new Error('network'))).toBe('发送失败，请稍后重试')
    expect(endFailureMessage(fault('thread_closed', 409))).toBe('会话已结束')
    expect(endFailureMessage(fault('internal', 500))).toBe('结束会话失败，请稍后重试')
  })
})
