import { fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { adaptArchive } from '@/features/session-data'
import { SessionRecords, SessionTimeline, ToolSummary } from './index'

const origin = Date.parse('2026-01-01T10:00:00Z')

function stamp(seconds: number) {
  return new Date(origin + seconds * 1000).toISOString()
}

function message(seq: number, seconds: number | null, kind: string, text: string) {
  return {
    seq,
    at: seconds === null ? null : stamp(seconds),
    type: 'update',
    update: { sessionUpdate: kind, content: { type: 'text', text } },
  }
}

function snapshot() {
  return adaptArchive({
    runId: 'synthetic-run',
    records: [
      { seq: 0, at: stamp(0), type: 'meta', agentRef: 'test/synthetic', schemaVersion: 2 },
      message(1, 0, 'user_message_chunk', 'Synthetic prompt'),
      message(2, 2, 'agent_message_chunk', 'First reply'),
      {
        seq: 3,
        at: stamp(3),
        type: 'update',
        tool_timing: { startedAt: stamp(3), durationMs: 0 },
        update: {
          sessionUpdate: 'tool_call',
          toolCallId: 'fixture-tool',
          kind: 'read',
          title: 'Read fixture',
          status: 'completed',
          rawInput: { path: 'fixture.txt' },
          rawOutput: { text: 'Synthetic result' },
        },
      },
      message(4, 6, 'agent_message_chunk', 'Later reply'),
      {
        seq: 5,
        type: 'update',
        update: {
          sessionUpdate: 'tool_call',
          kind: 'execute',
          title: 'Untimed tool',
          toolCallId: 'untimed',
          status: 'failed',
        },
      },
      message(6, 7, 'agent_thought_chunk', 'Synthetic thought'),
      { seq: 7, at: stamp(8), type: 'turnEnded', stop_reason: 'end_turn' },
      { seq: 8, type: 'future_record', description: 'Synthetic unsupported record' },
    ],
  })
}

function timelineRows() {
  return within(screen.getByRole('table', { name: '时间轴事件' }))
}

function details() {
  return within(screen.getByRole('complementary', { name: '事件详情' }))
}

describe('SessionTimeline', () => {
  it('labels file targets as read operations across the list, lane and details', async () => {
    const user = userEvent.setup()
    const files = ['Cargo.toml', 'package.json', 'README.md']
    const model = adaptArchive({
      runId: 'file-operations',
      records: files.map((file, seq) => ({
        seq,
        type: 'update',
        tool_timing: { startedAt: stamp(seq), durationMs: 25 },
        update: {
          sessionUpdate: 'tool_call',
          kind: 'read',
          title: file,
          status: 'completed',
          rawInput: { filePath: `/workspace/${file}` },
        },
      })),
    })
    render(<SessionTimeline model={model} />)
    for (const file of files) {
      expect(timelineRows().queryByRole('button', { name: file })).toBeNull()
      expect(timelineRows().getByRole('button', { name: `读取 · ${file}` })).toBeInTheDocument()
      expect(screen.getByRole('img', { name: `读取 · ${file} · 25ms` })).toBeInTheDocument()
    }
    await user.click(timelineRows().getByRole('button', { name: '读取 · Cargo.toml' }))
    expect(details().getByRole('heading', { name: '读取 · Cargo.toml' })).toBeInTheDocument()
    expect(details().getByText('原始操作标题').nextElementSibling).toHaveTextContent('Cargo.toml')
    expect(details().getByText('原生工具名').nextElementSibling).toHaveTextContent('未知')
    await user.click(details().getByRole('tab', { name: 'Payload' }))
    expect(details().getByRole('tabpanel', { name: 'Payload' })).toHaveTextContent('Cargo.toml')
    expect(model.stats.toolCount).toBe(3)
  })

  it.each([
    ['read', 'read', '读取'],
    ['read', 'READ', '读取'],
    ['read', null, '读取'],
    ['execute', 'npm test', '执行命令 · npm test'],
    ['future_kind', 'README.md', '工具调用 · README.md'],
    ['__proto__', 'README.md', '工具调用 · README.md'],
    [null, null, '工具调用'],
  ])(
    'uses the reported operation %s without inferring a native tool from %s',
    (kind, title, label) => {
      const model = adaptArchive({
        runId: 'operation-label',
        records: [{ seq: 0, type: 'update', update: { sessionUpdate: 'tool_call', kind, title } }],
      })
      render(<SessionTimeline model={model} />)
      expect(timelineRows().getByRole('button', { name: label ?? '' })).toBeInTheDocument()
    },
  )

  it('keeps three lanes and filters only the event list, leaving unknown time visible', async () => {
    const user = userEvent.setup()
    render(<SessionTimeline model={snapshot()} />)
    expect(screen.getByRole('region', { name: '三泳道时间轴' })).toBeInTheDocument()
    expect(
      timelineRows().getByRole('button', { name: '执行命令 · Untimed tool' }),
    ).toBeInTheDocument()
    expect(timelineRows().queryByText('Synthetic thought')).not.toBeInTheDocument()
    await user.click(
      within(screen.getByRole('group', { name: '时间轴类型筛选' })).getByRole('button', {
        name: '模型文本',
      }),
    )
    expect(timelineRows().queryByRole('button', { name: 'First reply' })).not.toBeInTheDocument()
    expect(screen.getByRole('img', { name: 'First reply · 未知' })).toBeInTheDocument()
    await user.type(screen.getByRole('textbox', { name: '搜索时间轴事件' }), 'fixture.txt')
    expect(timelineRows().getByRole('button', { name: '读取 · Read fixture' })).toBeInTheDocument()
    expect(
      timelineRows().queryByRole('button', { name: '执行命令 · Untimed tool' }),
    ).not.toBeInTheDocument()
  })

  it('preserves selected identity through filtering and a refreshed model', async () => {
    const user = userEvent.setup()
    const model = snapshot()
    const view = render(<SessionTimeline model={model} />)
    await user.click(timelineRows().getByRole('button', { name: '读取 · Read fixture' }))
    await user.type(screen.getByRole('textbox', { name: '搜索时间轴事件' }), 'Later reply')
    expect(details().getByRole('heading', { name: '读取 · Read fixture' })).toBeInTheDocument()
    expect(screen.getByRole('status')).toHaveTextContent('当前选中事件被筛选隐藏')
    view.rerender(
      <SessionTimeline
        model={adaptArchive({
          runId: model.runId,
          records: [
            ...model.rawRecords,
            message(9, 10, 'agent_message_chunk', 'An appended reply'),
          ],
        })}
      />,
    )
    expect(details().getByRole('heading', { name: '读取 · Read fixture' })).toBeInTheDocument()
    await user.clear(screen.getByRole('textbox', { name: '搜索时间轴事件' }))
    expect(timelineRows().getByRole('button', { name: '读取 · Read fixture' })).toHaveAttribute(
      'aria-pressed',
      'true',
    )
    await user.click(details().getByRole('button', { name: '关闭事件详情' }))
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })

  it('brushes the plot in either direction and retains unpositioned events', () => {
    render(<SessionTimeline model={snapshot()} />)
    fireEvent.click(screen.getByRole('button', { name: '真实时间' }))
    const plot = screen.getByRole('group', { name: '时间轴框选区域' })
    vi.spyOn(plot, 'getBoundingClientRect').mockReturnValue(new DOMRect(100, 0, 600, 120))
    fireEvent.pointerDown(plot, { pointerId: 1, button: 0, clientX: 450 })
    fireEvent.pointerMove(plot, { pointerId: 1, clientX: 250 })
    fireEvent.pointerUp(plot, { pointerId: 1, clientX: 250 })
    expect(timelineRows().getByRole('button', { name: 'First reply' })).toBeInTheDocument()
    expect(timelineRows().getByRole('button', { name: '读取 · Read fixture' })).toBeInTheDocument()
    expect(
      timelineRows().getByRole('button', { name: '执行命令 · Untimed tool' }),
    ).toBeInTheDocument()
    expect(
      timelineRows().queryByRole('button', { name: 'Synthetic prompt' }),
    ).not.toBeInTheDocument()
    expect(timelineRows().queryByRole('button', { name: 'Later reply' })).not.toBeInTheDocument()
    fireEvent.pointerDown(plot, { pointerId: 2, button: 0, clientX: 260 })
    fireEvent.pointerUp(plot, { pointerId: 2, clientX: 262 })
    expect(timelineRows().getByRole('button', { name: 'Later reply' })).toBeInTheDocument()
  })

  it('cancels unfinished gestures and offers keyboard-accessible range selection', () => {
    render(<SessionTimeline model={snapshot()} />)
    fireEvent.click(screen.getByRole('button', { name: '真实时间' }))
    const plot = screen.getByRole('group', { name: '时间轴框选区域' })
    vi.spyOn(plot, 'getBoundingClientRect').mockReturnValue(new DOMRect(0, 0, 600, 120))
    fireEvent.pointerDown(plot, { pointerId: 1, button: 0, clientX: 200 })
    fireEvent.pointerMove(plot, { pointerId: 1, clientX: 400 })
    fireEvent.pointerCancel(plot, { pointerId: 1 })
    fireEvent.pointerUp(plot, { pointerId: 1, clientX: 400 })
    expect(timelineRows().getByRole('button', { name: 'Synthetic prompt' })).toBeInTheDocument()
    fireEvent.change(screen.getByRole('slider', { name: '框选起点' }), {
      target: { value: origin + 2500 },
    })
    expect(timelineRows().queryByRole('button', { name: 'First reply' })).not.toBeInTheDocument()
    expect(timelineRows().getByRole('button', { name: 'Later reply' })).toBeInTheDocument()
    fireEvent.change(screen.getByRole('slider', { name: '框选终点' }), {
      target: { value: origin + 3500 },
    })
    expect(timelineRows().queryByRole('button', { name: 'Later reply' })).not.toBeInTheDocument()
    expect(timelineRows().getByRole('button', { name: '读取 · Read fixture' })).toBeInTheDocument()
    fireEvent.keyDown(plot, { key: 'Escape' })
    expect(timelineRows().getByRole('button', { name: 'Synthetic prompt' })).toBeInTheDocument()
  })

  it('shows full payload, result, timing and JSON without treating absent measurements as zero', async () => {
    const user = userEvent.setup()
    render(<SessionTimeline model={snapshot()} />)
    await user.click(timelineRows().getByRole('button', { name: '读取 · Read fixture' }))
    expect(details().getByText('0ms')).toBeInTheDocument()
    await user.click(details().getByRole('tab', { name: 'Payload' }))
    expect(details().getByRole('tabpanel', { name: 'Payload' })).toHaveTextContent('fixture.txt')
    await user.click(details().getByRole('tab', { name: 'Result' }))
    expect(details().getByRole('tabpanel', { name: 'Result' })).toHaveTextContent(
      'Synthetic result',
    )
    await user.click(details().getByRole('tab', { name: 'Timing' }))
    expect(details().getByRole('tabpanel', { name: 'Timing' })).toHaveTextContent('0ms')
    await user.click(details().getByRole('tab', { name: 'JSON' }))
    expect(details().getByRole('tabpanel', { name: 'JSON' })).toHaveTextContent('fixture-tool')
    await user.click(timelineRows().getByRole('button', { name: '执行命令 · Untimed tool' }))
    expect(details().getByRole('tab', { name: '摘要' })).toHaveAttribute('aria-selected', 'true')
    expect(details().queryByText('0ms')).not.toBeInTheDocument()
    await user.click(details().getByRole('tab', { name: 'Result' }))
    expect(details().getByRole('tabpanel', { name: 'Result' })).toHaveTextContent('未记录工具结果')
  })

  it('does not turn a negative adjacent gap into zero or model latency', async () => {
    const user = userEvent.setup()
    const model = adaptArchive({
      runId: 'reordered',
      records: [
        message(1, 8, 'user_message_chunk', 'User observed later'),
        message(2, 3, 'agent_message_chunk', 'Earlier model observation'),
      ],
    })
    render(<SessionTimeline model={model} />)
    await user.click(timelineRows().getByRole('button', { name: 'Earlier model observation' }))
    await user.click(details().getByRole('tab', { name: 'Timing' }))
    const panel = details().getByRole('tabpanel', { name: 'Timing' })
    expect(within(panel).getByText('与前一记录间隔').nextElementSibling).toHaveTextContent('未知')
    expect(within(panel).getByText('模型 API 延迟').nextElementSibling).toHaveTextContent('未知')
    expect(within(panel).queryByText('0ms')).not.toBeInTheDocument()
  })

  it('shows an unpositioned session without inventing chart bounds and resets on a different run', async () => {
    const user = userEvent.setup()
    const model = adaptArchive({
      runId: 'untimed-run',
      records: [message(1, null, 'agent_message_chunk', 'Only untimed text')],
    })
    const view = render(<SessionTimeline model={model} />)
    expect(screen.getByText('没有可定位的时间戳，事件仍可在下方查看。')).toBeInTheDocument()
    await user.click(timelineRows().getByRole('button', { name: 'Only untimed text' }))
    expect(details().getByText('记录时间').nextElementSibling).toHaveTextContent('未知')
    view.rerender(<SessionTimeline model={snapshot()} />)
    expect(screen.queryByRole('complementary')).not.toBeInTheDocument()
  })
})

describe('SessionRecords', () => {
  it('finds thoughts, lifecycle and unknown-time records while retaining raw replaced versions', async () => {
    const user = userEvent.setup()
    const model = snapshot()
    const duplicate = message(2, 2, 'agent_message_chunk', 'An obsolete synthetic reply')
    render(
      <SessionRecords
        model={adaptArchive({ runId: model.runId, records: [duplicate, ...model.rawRecords] })}
      />,
    )
    const filter = screen.getByRole('combobox', { name: '记录类型' })
    await user.selectOptions(filter, 'thought')
    await user.click(screen.getByRole('button', { name: 'Synthetic thought' }))
    expect(details().getByRole('heading', { name: 'Synthetic thought' })).toBeInTheDocument()
    await user.selectOptions(filter, 'lifecycle')
    expect(screen.getByRole('button', { name: 'turnEnded' })).toBeInTheDocument()
    await user.selectOptions(filter, 'unknown')
    expect(screen.getByRole('button', { name: 'future_record' })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'future_record' }))
    expect(details().getByText('记录时间').nextElementSibling).toHaveTextContent('未知')
    await user.type(screen.getByRole('textbox', { name: '搜索全部记录' }), 'unsupported')
    expect(screen.getByRole('button', { name: 'future_record' })).toBeInTheDocument()
    await user.click(screen.getByText('查看重放前的全部原始记录（含被替代版本）'))
    expect(screen.getByLabelText('重放前原始记录 JSON')).toHaveTextContent(
      'An obsolete synthetic reply',
    )
  })

  it('makes records beyond the first render window reachable and searchable', async () => {
    const user = userEvent.setup()
    const records = Array.from({ length: 205 }, (_, index) =>
      message(index, index, 'agent_message_chunk', `Synthetic row ${index}`),
    )
    render(<SessionRecords model={adaptArchive({ runId: 'long-run', records })} />)
    expect(screen.queryByText('Synthetic row 204', { exact: true })).not.toBeInTheDocument()
    await user.click(screen.getByText('显示更多记录', { exact: true }))
    expect(screen.getByText('Synthetic row 204', { exact: true })).toBeVisible()
    await user.click(screen.getByRole('textbox', { name: '搜索全部记录' }))
    await user.paste('row 204')
    expect(screen.getByText('Synthetic row 204', { exact: true })).toBeVisible()
    expect(screen.queryByText('Synthetic row 0', { exact: true })).not.toBeInTheDocument()
  })
})

describe('ToolSummary', () => {
  it('keeps measured zero and absent timing distinct in tool aggregates', () => {
    render(<ToolSummary model={snapshot()} />)
    const read = screen.getByRole('row', { name: /read/ })
    const execute = screen.getByRole('row', { name: /execute/ })
    expect(within(read).getAllByText('0ms')).toHaveLength(2)
    expect(within(execute).getAllByText('未知')).toHaveLength(2)
    expect(execute).toHaveTextContent('0 / 1')
  })

  it('reports the loaded sample contains no tools without claiming missing usage is zero', () => {
    render(<ToolSummary model={adaptArchive({ runId: 'empty', records: [] })} />)
    expect(screen.getByText('已加载记录中没有工具事件。')).toBeInTheDocument()
  })
})
