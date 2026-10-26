import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import type { SessionModelStatistics } from '@/api/generated.schemas'
import { SessionStatisticsSummary } from './session-statistics-summary'

function statistics(): SessionModelStatistics {
  const known = { value: 0, knownCount: 1, totalCount: 2, availability: 'partial' as const }
  const unknown = {
    value: null,
    knownCount: 0,
    totalCount: 2,
    availability: 'unavailable' as const,
  }
  return {
    schemaVersion: 1,
    source: 'model_gateway',
    scope: 'recorded_http_attempts',
    inputSemantics: 'unknown',
    counts: {
      total: 2,
      started: 1,
      succeeded: 1,
      failed: 0,
      cancelled: 0,
      interrupted: 0,
      incomplete: 0,
    },
    firstRecordedAt: null,
    lastRecordedAt: null,
    usage: {
      inputTokens: known,
      outputTokens: unknown,
      totalTokens: unknown,
      cacheReadTokens: unknown,
      cacheWriteTokens: unknown,
      reasoningTokens: unknown,
    },
    timing: { meanDurationMs: known, p90DurationMs: unknown, meanFirstOutputMs: unknown },
  }
}

it('requests one selected aggregate without fetching model-call pages and hides stale values after denial', async () => {
  const requested: string[] = []
  let denied = false
  server.use(
    http.get('/api/v1/tenants/t1/issues/i1/runs/:rid/statistics', ({ params }) => {
      requested.push(String(params['rid']))
      return denied
        ? HttpResponse.json({ code: 'forbidden' }, { status: 403 })
        : HttpResponse.json(statistics())
    }),
  )
  const user = userEvent.setup()
  renderWithProviders(
    <SessionStatisticsSummary
      sessionRef={{ tid: 't1', issueId: 'i1', runId: 'selected' }}
      detailsHref="/analysis"
    />,
  )
  expect(await screen.findByText('0ms')).toBeInTheDocument()
  expect(screen.getAllByText('未知')).toHaveLength(2)
  expect(screen.getAllByText(/部分数据或未终结快照/)).toHaveLength(2)
  expect(requested).toEqual(['selected'])
  denied = true
  await user.click(screen.getByRole('button', { name: '刷新统计' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('统计暂不可用')
  expect(screen.queryByText('0ms')).not.toBeInTheDocument()
  expect(requested).toEqual(['selected', 'selected'])
})

it('groups each metric name with its value and coverage in one definition-list group', async () => {
  server.use(
    http.get('/api/v1/tenants/t1/issues/i1/runs/selected/statistics', () =>
      HttpResponse.json(statistics()),
    ),
  )
  renderWithProviders(
    <SessionStatisticsSummary
      sessionRef={{ tid: 't1', issueId: 'i1', runId: 'selected' }}
      detailsHref="/analysis"
    />,
  )
  await screen.findByText('0ms')
  const measuredTerms = screen
    .getAllByRole('term')
    .filter((term) => term.textContent !== '已记录请求')
  for (const term of measuredTerms) {
    const group = term.parentElement
    if (!group) throw new Error('Metric definition group missing')
    expect([...group.children].every((child) => ['DT', 'DD'].includes(child.tagName))).toBe(true)
    expect(within(group).getAllByRole('definition').length).toBeGreaterThan(0)
    expect(within(group).getByText(/次请求已知/)).toBeInTheDocument()
  }
  expect(screen.getByRole('link', { name: '完整时间线与统计' })).toHaveAttribute(
    'href',
    '/analysis',
  )
})

it('keeps the no-observations explanation outside the metric definition list', async () => {
  server.use(
    http.get('/api/v1/tenants/t1/issues/i1/runs/selected/statistics', () => {
      const data = statistics()
      data.counts.total = 0
      return HttpResponse.json(data)
    }),
  )
  renderWithProviders(
    <SessionStatisticsSummary
      sessionRef={{ tid: 't1', issueId: 'i1', runId: 'selected' }}
      detailsHref={undefined}
    />,
  )
  const notice = await screen.findByText('暂无观测，不代表历史消耗为零。')
  expect(notice.closest('dl')).toBeNull()
  expect(screen.getAllByText('未知')).toHaveLength(2)
})
