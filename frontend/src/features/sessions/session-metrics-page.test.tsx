import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'
import { SessionDetailPage } from './session-detail-page'

const endpoint = `/api/v1/tenants/${TEST_TENANT_ID}/issues/metric-issue/runs/metric-run`
const observedAt = '2026-02-01T00:00:00Z'

function reportedUsage() {
  return {
    inputTokens: 11,
    outputTokens: 3,
    totalTokens: 14,
    cacheReadTokens: 0,
    cacheWriteTokens: null,
    reasoningTokens: null,
    inputSemantics: 'includes_cache',
    availability: 'partial',
  }
}

function requestObservation(id: string) {
  return {
    id,
    startedAt: observedAt,
    finishedAt: observedAt,
    modelId: 'fixture/gateway-model',
    protocol: 'openai-completions',
    streamed: true,
    outcome: 'succeeded',
    errorCode: null,
    httpStatus: 200,
    durationMs: 0,
    firstOutputMs: 0,
    terminalObserved: true,
    usage: reportedUsage(),
  }
}

function statisticalResponse(total = 2) {
  const aggregate = (value: unknown) => ({
    value: typeof value === 'number' && total > 0 ? value : null,
    knownCount: typeof value === 'number' ? total : 0,
    totalCount: total,
    availability: typeof value === 'number' && total > 0 ? 'complete' : 'unavailable',
  })
  return {
    schemaVersion: 1,
    source: 'model_gateway',
    scope: 'recorded_http_attempts',
    inputSemantics: 'includes_cache',
    counts: {
      total,
      started: 0,
      succeeded: total,
      failed: 0,
      cancelled: 0,
      interrupted: 0,
      incomplete: 0,
    },
    firstRecordedAt: total ? observedAt : null,
    lastRecordedAt: total ? observedAt : null,
    usage: Object.fromEntries(
      Object.entries(reportedUsage())
        .filter(([key]) => key.endsWith('Tokens'))
        .map(([key, value]) => [key, aggregate(value)]),
    ),
    timing: {
      meanDurationMs: aggregate(0),
      p90DurationMs: aggregate(0),
      meanFirstOutputMs: aggregate(0),
    },
  }
}

function prepareSession() {
  installCloudSpaceHandlers('member')
  server.use(
    http.get(endpoint, () =>
      HttpResponse.json({
        id: 'metric-run',
        issueId: 'metric-issue',
        executorType: 'agent',
        status: 'completed',
        createdAt: observedAt,
        completedAt: observedAt,
        revision: { id: 'metric-revision' },
      }),
    ),
    http.get(`${endpoint}/thread`, () =>
      HttpResponse.json({ code: 'unavailable' }, { status: 503 }),
    ),
    http.get(`${endpoint}/history`, () =>
      HttpResponse.json({ code: 'history_invalid' }, { status: 502 }),
    ),
    http.get(`${endpoint}/statistics`, () => HttpResponse.json(statisticalResponse())),
    http.get(`${endpoint}/model-calls`, () =>
      HttpResponse.json({ items: [requestObservation('page-one')], nextCursor: '' }),
    ),
  )
}

async function openMetrics() {
  renderAtRoute(
    '/w/:workspaceSlug/sessions/agent/:issueId/:runId',
    <SessionDetailPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/sessions/agent/metric-issue/metric-run',
  )
  await userEvent.setup().click(screen.getByRole('tab', { name: '模型请求' }))
}

it('reads gateway statistics despite unavailable history and pages attempts with a tenant-scoped cursor', async () => {
  prepareSession()
  const cursors: (string | null)[] = []
  server.use(
    http.get(`${endpoint}/model-calls`, ({ request }) => {
      const params = new URL(request.url).searchParams
      cursors.push(params.get('after'))
      expect(params.get('limit')).toBe('25')
      return HttpResponse.json({
        items: [requestObservation(params.has('after') ? 'page-two' : 'page-one')],
        nextCursor: params.has('after') ? '' : 'cursor-one',
      })
    }),
  )
  await openMetrics()
  expect(await screen.findByText('page-one')).toBeInTheDocument()
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  expect(
    within(screen.getByLabelText('模型请求汇总')).getByText('已记录请求').nextElementSibling,
  ).toHaveTextContent('2')
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '下一页请求' }))
  expect(await screen.findByText('page-two')).toBeInTheDocument()
  expect(screen.queryByText('page-one')).not.toBeInTheDocument()
  expect(cursors).toEqual([null, 'cursor-one'])
  await user.click(screen.getByRole('button', { name: '上一页请求' }))
  expect(await screen.findByText('page-one')).toBeInTheDocument()
})

it('does not request metrics before opening their tab and shows no-data history honestly', async () => {
  prepareSession()
  let reads = 0
  server.use(
    http.get(`${endpoint}/statistics`, () => {
      reads += 1
      return HttpResponse.json(statisticalResponse(0))
    }),
    http.get(`${endpoint}/model-calls`, () => HttpResponse.json({ items: [], nextCursor: '' })),
  )
  renderAtRoute(
    '/w/:workspaceSlug/sessions/agent/:issueId/:runId',
    <SessionDetailPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/sessions/agent/metric-issue/metric-run',
  )
  await screen.findByRole('alert')
  expect(reads).toBe(0)
  await userEvent.setup().click(screen.getByRole('tab', { name: '模型请求' }))
  expect(await screen.findByText('暂无模型请求观测')).toBeInTheDocument()
  expect(reads).toBe(1)
})

it('hides cached metrics and other views when a statistics refresh loses authorization', async () => {
  prepareSession()
  await openMetrics()
  await screen.findByText('page-one')
  server.use(
    http.get(`${endpoint}/statistics`, () =>
      HttpResponse.json({ code: 'membership_required' }, { status: 403 }),
    ),
  )
  await userEvent.setup().click(screen.getByRole('button', { name: '刷新' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('没有此空间的访问权限')
  expect(screen.queryByLabelText('模型请求统计')).not.toBeInTheDocument()
  await userEvent.setup().click(screen.getByRole('tab', { name: '概览' }))
  expect(screen.queryByLabelText('会话概览')).not.toBeInTheDocument()
  expect(screen.queryByText('page-one')).not.toBeInTheDocument()
})

it('hides old attempt rows after a failed request-page refresh instead of retaining stale success', async () => {
  prepareSession()
  await openMetrics()
  await screen.findByText('page-one')
  server.use(
    http.get(`${endpoint}/model-calls`, () =>
      HttpResponse.json({ code: 'unavailable' }, { status: 503 }),
    ),
  )
  await userEvent.setup().click(screen.getByRole('button', { name: '刷新' }))
  await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('读取失败'))
  expect(screen.queryByText('page-one')).not.toBeInTheDocument()
})
