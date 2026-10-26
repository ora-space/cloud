import { AxiosError, AxiosHeaders } from 'axios'
import { describe, expect, it } from 'vitest'
import type { ThreadEntry } from '@/api/generated.schemas'
import type { IssueRun } from '@/features/issues/types'
import { mergeThreadPreview } from '@/features/thread-preview/preview'
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

it('renders a live thought as open and replaces its matching final history position', () => {
  const record = {
    type: 'update',
    update: {
      sessionUpdate: 'agent_thought_chunk',
      content: { type: 'text', text: 'checking the file' },
    },
  }
  const preview = {
    available: true,
    executionId: 'execution',
    version: 1,
    incomplete: false,
    blocks: [{ historySequence: 7, turnId: 'turn', record }],
  }
  expect(threadItems(mergeThreadPreview([], preview))).toEqual([
    { variant: 'thought', key: -8, text: 'checking the file', turnId: 'turn', live: true },
  ])
  const final = entry(4, { historySequence: 7, turnId: 'turn', record })
  expect(threadItems(mergeThreadPreview([final], preview))).toEqual([
    { variant: 'thought', key: -8, text: 'checking the file', turnId: 'turn' },
  ])
})

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
  it('uses explicit Node turns when a future queued user entry precedes an earlier tool update', () => {
    const rows = threadItems([
      entry(1, { source: 'user', kind: 'user_turn', turnId: 'first', status: 'delivered' }),
      entry(2, {
        turnId: 'first',
        record: { update: { sessionUpdate: 'tool_call', toolCallId: 'same', rawInput: 'first' } },
      }),
      entry(3, { source: 'user', kind: 'user_turn', turnId: 'second', status: 'queued' }),
      entry(4, {
        turnId: 'first',
        record: {
          update: { sessionUpdate: 'tool_call_update', toolCallId: 'same', status: 'completed' },
        },
      }),
      entry(5, {
        turnId: 'second',
        record: { update: { sessionUpdate: 'tool_call', toolCallId: 'same', rawInput: 'second' } },
      }),
    ])
    expect(rows.filter((row) => row.variant === 'tool')).toMatchObject([
      { key: 2, turnId: 'first', tool: { rawInput: 'first', status: 'completed' } },
      { key: 5, turnId: 'second', tool: { rawInput: 'second' } },
    ])
  })

  it('keeps repeated tool IDs separate with the public response shape lacking node turnId', () => {
    const rows = threadItems([
      entry(1, { source: 'user', kind: 'user_turn', turnId: 'first', status: 'delivered' }),
      entry(2, {
        record: { update: { sessionUpdate: 'tool_call', toolCallId: 'read', rawInput: 'first' } },
      }),
      entry(3, { source: 'user', kind: 'user_turn', turnId: 'second', status: 'delivered' }),
      entry(4, {
        record: { update: { sessionUpdate: 'tool_call', toolCallId: 'read', rawInput: 'second' } },
      }),
      entry(5, {
        record: {
          update: { sessionUpdate: 'tool_call_update', toolCallId: 'read', status: 'completed' },
        },
      }),
    ])
    expect(rows.filter((row) => row.variant === 'tool')).toMatchObject([
      { key: 2, tool: { rawInput: 'first' } },
      { key: 4, tool: { rawInput: 'second', status: 'completed' } },
    ])
  })

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
      expect.objectContaining({
        variant: 'tool',
        key: 5,
        tool: expect.objectContaining({ id: null }),
      }),
      { variant: 'separator', key: 6 },
      { variant: 'user', key: 7, text: 'Now add tests', status: '排队中' },
      { variant: 'agent', key: 9, text: 'On it', turnId: 't2' },
      { variant: 'agent', key: 10, text: 'Other turn', turnId: 't3' },
      { variant: 'note', key: 11, text: '此处有缺失的会话记录' },
      { variant: 'note', key: 12, text: '暂不支持展示的会话事件，可在执行详情中查看原始记录' },
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

  it('retains thought text and merges sparse tool updates by turn and call identity', () => {
    const update = (seq: number, turnId: string, payload: Record<string, unknown>) =>
      entry(seq, { turnId, record: { update: payload } })
    const rows = threadItems([
      update(1, 'a', {
        sessionUpdate: 'agent_thought_chunk',
        content: { type: 'text', text: 'Checking ' },
      }),
      update(2, 'a', {
        sessionUpdate: 'agent_thought_chunk',
        content: { type: 'text', text: 'files' },
      }),
      update(3, 'a', {
        sessionUpdate: 'tool_call',
        toolCallId: 'read',
        title: 'hello.py',
        kind: 'read',
        status: 'in_progress',
        rawInput: { path: 'hello.py' },
      }),
      update(4, 'a', {
        sessionUpdate: 'tool_call_update',
        toolCallId: 'read',
        status: 'completed',
        rawOutput: { output: 'hello' },
      }),
      update(5, 'b', { sessionUpdate: 'tool_call', toolCallId: 'read', title: 'other.py' }),
    ])
    expect(rows).toHaveLength(3)
    expect(rows[0]).toEqual({ variant: 'thought', key: 1, text: 'Checking files', turnId: 'a' })
    expect(rows[1]).toMatchObject({
      variant: 'tool',
      key: 3,
      tool: {
        title: 'hello.py',
        status: 'completed',
        rawInput: { path: 'hello.py' },
        output: 'hello',
      },
    })
    expect(rows[2]).toMatchObject({ variant: 'tool', key: 5, tool: { title: 'other.py' } })
  })

  it('keeps unidentified tools separate and retains an orphan result', () => {
    const rows = threadItems(
      [1, 2].map((seq) =>
        entry(seq, {
          record: {
            update: {
              sessionUpdate: 'tool_call_update',
              status: 'completed',
              rawOutput: { output: 'result' },
            },
          },
        }),
      ),
    )
    expect(rows).toHaveLength(2)
    expect(rows.every((row) => row.variant === 'tool' && row.tool.output === 'result')).toBe(true)
  })

  it('reads durable failure codes and replaces untrusted diagnostics with a fixed message', () => {
    const rows = threadItems([
      entry(1, { source: 'system', kind: 'session_failed', record: { code: 'agent_turn_failed' } }),
      entry(2, {
        source: 'system',
        kind: 'session_failed',
        record: { code: 'agent_authentication_required' },
      }),
      entry(3, {
        source: 'system',
        kind: 'session_failed',
        record: { code: 'private provider diagnostic' },
      }),
    ])
    expect(rows).toEqual([
      { variant: 'note', key: 1, text: '模型请求失败，会话已结束（agent_turn_failed）' },
      {
        variant: 'note',
        key: 2,
        text: 'Agent 未登录或缺少凭据，请先完成登录或配置凭据（agent_authentication_required）',
      },
      { variant: 'note', key: 3, text: 'Agent 会话失败（agent_failed）' },
    ])
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
    expect(sendFailureMessage(fault('task_handoff_pending', 409))).toBe(
      '正在等待 Agent 交接，请在任务对话中发送后续消息',
    )
    expect(sendFailureMessage(fault('task_paused', 409))).toBe(
      '任务已暂停，请先在任务对话中处理暂停或失败的消息',
    )
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
