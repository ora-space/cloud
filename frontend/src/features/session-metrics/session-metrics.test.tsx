import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { SessionModelAttempt, SessionModelStatistics } from '@/api/generated.schemas'
import { describe, expect, it, vi } from 'vitest'
import { SessionMetrics } from './index'

function aggregate(
  value: number | null,
  knownCount = 1,
  totalCount = 3,
): SessionModelStatistics['usage']['inputTokens'] {
  const availability = 'partial' as const
  if (knownCount === 0) return { value, knownCount, totalCount, availability: 'unavailable' }
  if (knownCount === totalCount) return { value, knownCount, totalCount, availability: 'complete' }
  return { value, knownCount, totalCount, availability }
}

function statistics(): SessionModelStatistics {
  return {
    schemaVersion: 1,
    source: 'model_gateway',
    scope: 'recorded_http_attempts',
    inputSemantics: 'unknown',
    counts: {
      total: 3,
      started: 1,
      succeeded: 1,
      failed: 1,
      cancelled: 0,
      interrupted: 0,
      incomplete: 0,
    },
    firstRecordedAt: '2026-01-01T00:00:00Z',
    lastRecordedAt: '2026-01-01T00:00:02Z',
    usage: {
      inputTokens: aggregate(0),
      outputTokens: aggregate(6, 2),
      totalTokens: aggregate(null, 0),
      cacheReadTokens: aggregate(null, 0),
      cacheWriteTokens: aggregate(0),
      reasoningTokens: aggregate(1),
    },
    timing: {
      meanDurationMs: aggregate(100, 2),
      p90DurationMs: aggregate(200, 2),
      meanFirstOutputMs: aggregate(0),
    },
  }
}

function attempts(): SessionModelAttempt[] {
  const first: SessionModelAttempt = {
    id: 'synthetic-request-1',
    startedAt: '2026-01-01T00:00:00Z',
    finishedAt: '2026-01-01T00:00:00Z',
    modelId: 'test/model',
    protocol: 'openai-completions',
    streamed: true,
    outcome: 'succeeded',
    errorCode: null,
    httpStatus: 200,
    durationMs: 0,
    firstOutputMs: 0,
    terminalObserved: true,
    usage: {
      inputTokens: 0,
      outputTokens: 4,
      totalTokens: null,
      cacheReadTokens: null,
      cacheWriteTokens: 0,
      reasoningTokens: 1,
      inputSemantics: 'includes_cache',
      availability: 'partial',
    },
  }
  return [
    first,
    {
      ...first,
      id: 'synthetic-request-2',
      outcome: 'failed',
      errorCode: 'upstream_error',
      httpStatus: 500,
      durationMs: 200,
      firstOutputMs: null,
      terminalObserved: false,
      streamed: false,
      finishedAt: null,
      usage: {
        ...first.usage,
        inputTokens: null,
        outputTokens: 2,
        inputSemantics: 'excludes_cache',
      },
    },
  ]
}

function props() {
  return {
    statistics: statistics(),
    attempts: attempts(),
    pageIndex: 0,
    pageSize: 25,
    hasNextPage: true,
    isFetching: false,
    onPrevious: vi.fn<() => void>(),
    onNext: vi.fn<() => void>(),
  }
}

function trendAttempts(values: (number | null)[]): SessionModelAttempt[] {
  const template = attempts()[0]
  if (!template) throw new Error('Synthetic request missing')
  return values.map((value, index) => ({
    ...template,
    id: `synthetic-trend-${index + 1}`,
    outcome: value === null ? 'cancelled' : 'succeeded',
    usage: { ...template.usage, inputTokens: value },
  }))
}

type DecodeCase = {
  name: string
  expected: string
  patch?: Partial<SessionModelAttempt>
  usage?: Partial<SessionModelAttempt['usage']>
}

const decodeCases: DecodeCase[] = [
  { name: 'excludes first-output waiting and does not add reasoning tokens', expected: '50' },
  { name: 'supports Anthropic streams', patch: { protocol: 'anthropic-messages' }, expected: '50' },
  { name: 'preserves measured zero', usage: { outputTokens: 0 }, expected: '0' },
  { name: 'preserves a zero first-output delay', patch: { firstOutputMs: 0 }, expected: '40' },
  { name: 'formats fractional rates', usage: { outputTokens: 205 }, expected: '51.3' },
  {
    name: 'does not round positive rates to zero',
    usage: { outputTokens: 1 },
    patch: { durationMs: 21000 },
    expected: '< 0.1',
  },
  { name: 'rejects absent output', usage: { outputTokens: null }, expected: '未知' },
  { name: 'rejects negative output', usage: { outputTokens: -1 }, expected: '未知' },
  { name: 'rejects fractional token counts', usage: { outputTokens: 0.5 }, expected: '未知' },
  {
    name: 'rejects unsafe token counts',
    usage: { outputTokens: Number.MAX_SAFE_INTEGER + 1 },
    expected: '未知',
  },
  { name: 'rejects nonfinite output', usage: { outputTokens: Infinity }, expected: '未知' },
  { name: 'rejects absent duration', patch: { durationMs: null }, expected: '未知' },
  { name: 'rejects absent first output', patch: { firstOutputMs: null }, expected: '未知' },
  { name: 'rejects negative duration', patch: { durationMs: -1 }, expected: '未知' },
  { name: 'rejects negative first output', patch: { firstOutputMs: -1 }, expected: '未知' },
  { name: 'rejects nonfinite duration', patch: { durationMs: Infinity }, expected: '未知' },
  { name: 'rejects invalid first output', patch: { firstOutputMs: NaN }, expected: '未知' },
  {
    name: 'rejects unsafe duration',
    patch: { durationMs: Number.MAX_SAFE_INTEGER + 1 },
    expected: '未知',
  },
  { name: 'rejects fractional first output', patch: { firstOutputMs: 0.5 }, expected: '未知' },
  { name: 'rejects zero duration', patch: { durationMs: 0, firstOutputMs: 0 }, expected: '未知' },
  { name: 'rejects zero decode interval', patch: { firstOutputMs: 5000 }, expected: '未知' },
  { name: 'rejects negative decode interval', patch: { firstOutputMs: 6000 }, expected: '未知' },
  { name: 'rejects non-streaming observations', patch: { streamed: false }, expected: '未知' },
  { name: 'rejects missing completion', patch: { finishedAt: null }, expected: '未知' },
  {
    name: 'rejects missing terminal evidence',
    patch: { terminalObserved: false },
    expected: '未知',
  },
  { name: 'rejects partial usage', usage: { availability: 'partial' }, expected: '未知' },
  { name: 'rejects unavailable usage', usage: { availability: 'unavailable' }, expected: '未知' },
  ...(['started', 'failed', 'cancelled', 'interrupted', 'incomplete'] as const).map((outcome) => ({
    name: `rejects ${outcome} attempts`,
    patch: { outcome },
    expected: '未知',
  })),
]

describe('SessionMetrics', () => {
  it.each(decodeCases)('decode rate $name', ({ patch, usage, expected }) => {
    const input = props()
    const template = input.attempts[0]
    if (!template) throw new Error('Synthetic request missing')
    const attempt: SessionModelAttempt = {
      ...template,
      durationMs: 5000,
      firstOutputMs: 1000,
      ...patch,
      usage: { ...template.usage, availability: 'complete', outputTokens: 200, ...usage },
    }
    render(<SessionMetrics {...input} attempts={[attempt]} />)
    const table = within(screen.getByRole('table'))
    const header = table.getByRole('columnheader', { name: '平均 Decode 速率（Token/s，估算）' })
    const column = table.getAllByRole('columnheader').indexOf(header)
    const row = table.getAllByRole('row').at(1)
    if (!row) throw new Error('Expected request row')
    expect(within(row).getAllByRole('cell').at(column)?.textContent).toBe(expected)
    expect(screen.getByText(/平均 Decode 速率（估算）=/)).toHaveTextContent(
      '输出 Token ÷（请求时长 − 首次有效输出耗时）',
    )
    expect(screen.getByText(/不代表模型内部纯 Decode 性能/)).toBeInTheDocument()
  })

  it('keeps complete-run summaries separate from the current page and states every coverage boundary', () => {
    render(<SessionMetrics {...props()} />)
    const summary = within(screen.getByLabelText('模型请求汇总'))
    expect(summary.getByText('已记录请求').nextElementSibling).toHaveTextContent('3')
    expect(summary.getByText('输入 Token').nextElementSibling).toHaveTextContent('0')
    expect(summary.getByText('输出 Token').nextElementSibling).toHaveTextContent('6')
    expect(summary.getByText('上游总 Token').nextElementSibling).toHaveTextContent('未知')
    expect(summary.getAllByText('1 / 3 次请求已知 · 部分数据或未终结快照').length).toBeGreaterThan(
      0,
    )
    expect(summary.getAllByText('0 / 3 次请求已知 · 未提供')).toHaveLength(2)
    expect(summary.getByText(/不代表完整历史/)).toBeInTheDocument()
    expect(summary.getByText(/不同请求口径不一致/)).toBeInTheDocument()
    expect(screen.getByText('请求明细 · 当前页 2 条')).toBeInTheDocument()
    expect(screen.getByText(/重试分别计数/)).toBeInTheDocument()
    expect(summary.getByText('待完成观测')).toBeInTheDocument()
    expect(summary.getByText(/可能已结束但未采集到完成记录/)).toBeInTheDocument()
    expect(summary.queryByText('进行中')).not.toBeInTheDocument()
  })

  it('preserves zero timing and unknown usage while keeping outcome and protocol completion separate', () => {
    render(<SessionMetrics {...props()} />)
    const rows = within(screen.getByRole('table')).getAllByRole('row')
    const firstRow = rows.at(1)
    const secondRow = rows.at(2)
    if (!firstRow || !secondRow) throw new Error('Expected both request rows')
    const first = within(firstRow)
    const second = within(secondRow)
    expect(first.getAllByText('0ms')).toHaveLength(2)
    expect(first.getByText('已观测协议结束')).toBeInTheDocument()
    expect(first.getByText('输入含缓存')).toBeInTheDocument()
    expect(second.getByText('失败')).toBeInTheDocument()
    expect(second.getByText('未观测协议结束')).toBeInTheDocument()
    expect(second.getByText('输入不含缓存')).toBeInTheDocument()
    expect(second.getByText('upstream_error')).toBeInTheDocument()
    expect(second.getByText('结束：未知')).toBeInTheDocument()
  })

  it('plots actual zero and marks unknown fields separately without zero-filled points', async () => {
    render(<SessionMetrics {...props()} />)
    const user = userEvent.setup()
    const plot = screen.getByRole('img', { name: '输入 Token每请求趋势' })
    expect(plot.querySelectorAll('circle')).toHaveLength(1)
    expect(plot).toHaveTextContent('请求 #1 · 0 · synthetic-request-1')
    await user.selectOptions(screen.getByLabelText('统计字段'), 'outputTokens')
    expect(
      screen.getByRole('img', { name: '输出 Token每请求趋势' }).querySelectorAll('line'),
    ).toHaveLength(1)
    await user.selectOptions(screen.getByLabelText('统计字段'), 'cacheReadTokens')
    expect(screen.getByText('当前页没有此字段的已知值。')).toBeInTheDocument()
    const unknownPlot = screen.getByRole('img', { name: '缓存读取 Token每请求趋势' })
    expect(unknownPlot.querySelectorAll('circle, line')).toHaveLength(0)
    expect(within(unknownPlot).getAllByLabelText(/未报告缓存读取 Token，不代表零/)).toHaveLength(2)
    expect(within(unknownPlot).queryByText('0')).not.toBeInTheDocument()
  })

  it('connects known observations across consecutive missing requests with explicit gap counts', () => {
    render(
      <SessionMetrics
        {...props()}
        attempts={trendAttempts([null, 0, null, null, 8, 4, null])}
        pageIndex={1}
      />,
    )
    const plot = screen.getByRole('img', { name: '输入 Token每请求趋势' })
    const points = plot.querySelectorAll('circle')
    const connections = plot.querySelectorAll('line')
    expect(points).toHaveLength(3)
    expect(connections).toHaveLength(2)
    expect(points.item(0)).toHaveAttribute('cy', '165')
    expect(connections.item(0)).toHaveAttribute('stroke-dasharray', '6 5')
    expect(connections.item(0)).toHaveTextContent(
      '请求 #27 → #30 · 跨 2 次未报告用量，仅连接已知观测，非插值',
    )
    expect(connections.item(1)).not.toHaveAttribute('stroke-dasharray')
    expect(connections.item(1)).toHaveTextContent('请求 #30 → #31 · 相邻已知观测')
    expect(Number(points.item(0).getAttribute('cx'))).toBeCloseTo(50 + 700 / 6)
    expect(Number(points.item(1).getAttribute('cx'))).toBeCloseTo(50 + (4 * 700) / 6)
    expect(Number(points.item(2).getAttribute('cx'))).toBeCloseTo(50 + (5 * 700) / 6)
    expect(plot).toHaveTextContent('#26')
    expect(plot).toHaveTextContent('#32')
    const missing = within(plot).getAllByLabelText(/未报告输入 Token，不代表零/)
    expect(missing).toHaveLength(4)
    expect(missing.map((mark) => mark.getAttribute('aria-label'))).toEqual([
      '请求 #26 未报告输入 Token，不代表零 · 已取消',
      '请求 #28 未报告输入 Token，不代表零 · 已取消',
      '请求 #29 未报告输入 Token，不代表零 · 已取消',
      '请求 #32 未报告输入 Token，不代表零 · 已取消',
    ])
    for (const mark of missing) expect(mark.getAttribute('d')).toContain('191')
    expect(screen.getByLabelText('趋势图例')).toHaveTextContent(
      '虚线：跨未报告请求，仅连接已知观测，非插值',
    )
  })

  it('keeps ten known observations connected across a twenty-request page without filling missing data', () => {
    const values = [
      null,
      null,
      null,
      10,
      20,
      null,
      null,
      30,
      40,
      50,
      60,
      70,
      null,
      null,
      80,
      90,
      null,
      100,
      null,
      null,
    ]
    render(<SessionMetrics {...props()} attempts={trendAttempts(values)} />)
    const plot = screen.getByRole('img', { name: '输入 Token每请求趋势' })
    expect(plot.querySelectorAll('circle')).toHaveLength(10)
    expect(plot.querySelectorAll('line')).toHaveLength(9)
    expect(plot.querySelectorAll('line[stroke-dasharray]')).toHaveLength(3)
    expect(within(plot).getAllByLabelText(/未报告输入 Token，不代表零 · 已取消/)).toHaveLength(10)
    expect(plot).toHaveTextContent('请求 #5 → #8 · 跨 2 次未报告用量，仅连接已知观测，非插值')
    expect(plot).toHaveTextContent('请求 #16 → #18 · 跨 1 次未报告用量，仅连接已知观测，非插值')
  })

  it.each([
    { values: [null, 0, null], known: 1, missing: 2 },
    { values: [null, null, null], known: 0, missing: 3 },
    { values: [0], known: 1, missing: 0 },
  ])(
    'does not invent connections beyond $known known observations',
    ({ values, known, missing }) => {
      render(<SessionMetrics {...props()} attempts={trendAttempts(values)} />)
      const plot = screen.getByRole('img', { name: '输入 Token每请求趋势' })
      expect(plot.querySelectorAll('circle')).toHaveLength(known)
      expect(plot.querySelectorAll('line')).toHaveLength(0)
      expect(within(plot).queryAllByLabelText(/未报告输入 Token，不代表零/)).toHaveLength(missing)
      expect(within(plot).queryAllByText('0')).toHaveLength(known > 0 ? 1 : 0)
    },
  )

  it('delegates bounded pagination and keeps request numbering consistent with the selected page', async () => {
    const input = props()
    const view = render(<SessionMetrics {...input} />)
    const user = userEvent.setup()
    expect(screen.getByRole('button', { name: '上一页请求' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '下一页请求' }))
    expect(input.onNext).toHaveBeenCalledOnce()
    view.rerender(<SessionMetrics {...input} pageIndex={1} hasNextPage={false} />)
    expect(screen.getByText('#26 · test/model')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '下一页请求' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '上一页请求' }))
    expect(input.onPrevious).toHaveBeenCalledOnce()
    view.rerender(<SessionMetrics {...input} pageIndex={1} isFetching />)
    expect(screen.getByRole('button', { name: '上一页请求' })).toBeDisabled()
  })

  it('shows a historical-session empty state without claiming zero consumption', () => {
    const input = props()
    input.statistics.counts.total = 0
    render(<SessionMetrics {...input} attempts={[]} />)
    expect(screen.getByText('暂无模型请求观测')).toBeInTheDocument()
    expect(screen.getByText(/未知不代表零消耗/)).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('uses known-value formatting and complete coverage without converting invalid numbers into metrics', () => {
    const input = props()
    input.statistics.inputSemantics = 'includes_cache'
    input.statistics.firstRecordedAt = null
    input.statistics.lastRecordedAt = 'invalid-date'
    input.statistics.usage.inputTokens = aggregate(1200, 3, 3)
    input.statistics.timing.meanDurationMs = aggregate(1500, 3, 3)
    const attempt = input.attempts[0]
    if (!attempt) throw new Error('Synthetic request missing')
    attempt.usage.outputTokens = -1
    attempt.usage.inputSemantics = 'unknown'
    attempt.usage.availability = 'unavailable'
    attempt.modelId = ''
    attempt.httpStatus = null
    render(<SessionMetrics {...input} />)
    expect(screen.getByText(/输入包含缓存读取/)).toBeInTheDocument()
    expect(screen.getAllByText('3 / 3 次请求已知 · 已记录请求全部覆盖')).toHaveLength(2)
    expect(screen.getByText('1.5s')).toBeInTheDocument()
    expect(screen.getByText('1,200')).toBeInTheDocument()
    expect(screen.getByText('输入口径未知')).toBeInTheDocument()
    expect(screen.getByText('用量未提供')).toBeInTheDocument()
    expect(screen.getByText('HTTP 未知')).toBeInTheDocument()
    expect(screen.queryByText('-1')).not.toBeInTheDocument()
  })

  it('reports an excludes-cache summary and separately retained upstream totals', () => {
    const input = props()
    input.statistics.inputSemantics = 'excludes_cache'
    const attempt = input.attempts[0]
    if (!attempt) throw new Error('Synthetic request missing')
    attempt.usage.availability = 'complete'
    render(<SessionMetrics {...input} />)
    expect(screen.getByText(/输入不含缓存读写/)).toBeInTheDocument()
    expect(screen.getByText('用量完整')).toBeInTheDocument()
  })

  it('does not upgrade all-known interim snapshots to complete usage coverage', () => {
    const input = props()
    input.statistics.usage.outputTokens = {
      value: 50,
      knownCount: 3,
      totalCount: 3,
      availability: 'partial',
    }
    render(<SessionMetrics {...input} />)
    expect(screen.getByText('3 / 3 次请求已知 · 部分数据或未终结快照')).toBeInTheDocument()
    expect(screen.queryByText('3 / 3 次请求已知 · 已记录请求全部覆盖')).not.toBeInTheDocument()
  })
})
