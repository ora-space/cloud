import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { AgentSessionSummary } from '@/api/generated.schemas'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'
import { SessionDetailPage } from './session-detail-page'
import { SessionListPage } from './session-list-page'
import { sessionFailure, sessionTime } from './presentation'

const base = `/api/v1/tenants/${TEST_TENANT_ID}`
const runPath = `${base}/issues/i1/runs/r1`
const date = '2026-10-01T00:00:00Z'

function summary(id = 'r1'): AgentSessionSummary {
  return {
    id,
    issueId: 'i1',
    issueNumber: 3,
    issueTitle: '验证工具输出',
    projectId: 'p1',
    projectName: '测试项目',
    agentPluginId: 'official/ora-space.opencode',
    status: 'completed',
    threadState: 'ended',
    createdAt: date,
    startedAt: date,
    completedAt: date,
    historyAvailable: true,
    model: { connectionName: '测试连接', modelName: '测试模型', modelId: 'test/model' },
  }
}

function serveDetail(archive = true) {
  installCloudSpaceHandlers('member')
  server.use(
    http.get(runPath, () =>
      HttpResponse.json({
        id: 'r1',
        issueId: 'i1',
        executorType: 'agent',
        status: 'completed',
        createdAt: date,
        completedAt: date,
        revision: archive ? { id: 'rev1' } : null,
      }),
    ),
    http.get(`${runPath}/thread`, () =>
      HttpResponse.json({
        items: [
          {
            seq: 1,
            source: 'system',
            kind: 'user_turn',
            record: { content: '执行测试任务' },
            createdAt: date,
          },
        ],
        model: { connectionName: '测试连接', modelId: 'test/model', modelName: '测试模型' },
        threadState: 'ended',
      }),
    ),
    http.get(`${runPath}/history`, () =>
      HttpResponse.json({
        source: 'archive',
        records: [
          {
            seq: 1,
            at: date,
            type: 'update',
            update: {
              sessionUpdate: 'agent_thought_chunk',
              content: { type: 'text', text: '归档思考内容' },
            },
          },
        ],
        sizeBytes: 100,
        sha256: 'a'.repeat(64),
      }),
    ),
  )
}

function renderDetail() {
  return renderAtRoute(
    '/w/:workspaceSlug/sessions/agent/:issueId/:runId',
    <SessionDetailPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/sessions/agent/i1/r1',
  )
}

function serveProjects() {
  server.use(
    http.get(`${base}/projects`, ({ request }) => {
      const after = new URL(request.url).searchParams.get('after')
      return HttpResponse.json({
        items: [{ id: after ? 'p2' : 'p1', name: after ? '后一页项目' : '测试项目' }],
        nextCursor: after ? '' : 'p1',
      })
    }),
  )
}

it('pages session summaries and applies filters at the tenant HTTP boundary', async () => {
  installCloudSpaceHandlers('member')
  serveProjects()
  const reads: URLSearchParams[] = []
  server.use(
    http.get(`${base}/agent-sessions`, ({ request }) => {
      const params = new URL(request.url).searchParams
      reads.push(params)
      return HttpResponse.json({
        items: [summary(params.get('after') ? 'r2' : 'r1')],
        nextCursor: params.has('after') || params.has('status') ? '' : 'r1',
      })
    }),
  )
  renderAtRoute(
    '/w/:workspaceSlug/sessions',
    <SessionListPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/sessions',
  )
  expect(await screen.findByRole('link', { name: '分析会话 r1' })).toHaveAttribute(
    'href',
    '/w/cloud-dev/sessions/agent/i1/r1',
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: '加载更多会话' }))
  expect(await screen.findByRole('link', { name: '分析会话 r2' })).toBeInTheDocument()
  expect(await screen.findByRole('option', { name: '后一页项目' })).toBeInTheDocument()
  await user.selectOptions(screen.getByLabelText('项目'), 'p2')
  await user.selectOptions(screen.getByLabelText('运行状态'), 'failed')
  await user.type(screen.getByLabelText('Agent 插件'), 'official/test')
  await user.click(screen.getByRole('button', { name: '应用筛选' }))
  await waitFor(() => expect(reads.at(-1)?.get('status')).toBe('failed'))
  expect(reads.at(-1)?.get('projectId')).toBe('p2')
  expect(reads.at(-1)?.get('agent')).toBe('official/test')
  expect(reads.at(-1)?.has('after')).toBe(false)
  await user.click(screen.getByRole('button', { name: '重置' }))
  expect(screen.getByLabelText('运行状态')).toHaveValue('')
})

it('uses verified archive records and frozen model metadata with a task backlink', async () => {
  serveDetail()
  renderDetail()
  expect(await screen.findByText('完整归档 · 已校验')).toBeInTheDocument()
  expect(await screen.findByText('test/model')).toBeInTheDocument()
  expect(screen.getByText(/模型请求次数、Token 与请求耗时请查看「模型请求」/)).toBeInTheDocument()
  expect(screen.getByRole('link', { name: '返回原任务' })).toHaveAttribute(
    'href',
    '/w/cloud-dev/issues/i1',
  )
  await userEvent.setup().click(screen.getByRole('tab', { name: '完整事件' }))
  expect(await screen.findByText('归档思考内容')).toBeInTheDocument()
})

it('does not silently substitute a Thread for an invalid archive', async () => {
  serveDetail()
  server.use(
    http.get(`${runPath}/history`, () =>
      HttpResponse.json({ code: 'history_invalid' }, { status: 502 }),
    ),
  )
  renderDetail()
  expect(await screen.findByRole('alert')).toHaveTextContent('归档校验失败')
  expect(screen.queryByText('Thread 快照 · 非完整归档')).not.toBeInTheDocument()
  expect(screen.queryByText('完整归档 · 已校验')).not.toBeInTheDocument()
})

it('loads older Thread pages without pretending they are a full archive', async () => {
  serveDetail(false)
  const before: (string | null)[] = []
  server.use(
    http.get(`${runPath}/thread`, ({ request }) => {
      const cursor = new URL(request.url).searchParams.get('before')
      before.push(cursor)
      return HttpResponse.json({
        items: [
          {
            seq: cursor ? 1 : 2,
            source: 'system',
            kind: 'user_turn',
            record: { content: cursor ? '早期消息' : '较新消息' },
            createdAt: date,
          },
        ],
        model: null,
      })
    }),
  )
  renderDetail()
  expect(await screen.findByText('Thread 快照 · 非完整归档')).toBeInTheDocument()
  await userEvent.setup().click(screen.getByRole('button', { name: '加载更早事件' }))
  await waitFor(() => expect(before).toEqual([null, '2']))
  await waitFor(() =>
    expect(screen.queryByRole('button', { name: '加载更早事件' })).not.toBeInTheDocument(),
  )
  expect(screen.getByText('2 条原始记录 · 2 条重放事件')).toBeInTheDocument()
})

it('shows a pending session honestly when the Thread is not declared', async () => {
  serveDetail(false)
  server.use(
    http.get(`${runPath}/thread`, () => HttpResponse.json({ code: 'not_found' }, { status: 404 })),
  )
  renderDetail()
  expect(await screen.findByText(/会话尚未开始/)).toBeInTheDocument()
  expect(screen.queryByLabelText('会话概览')).not.toBeInTheDocument()
})

it('hides cached content after membership is revoked on refresh', async () => {
  serveDetail()
  renderDetail()
  await screen.findByText('完整归档 · 已校验')
  server.use(
    http.get(runPath, () => HttpResponse.json({ code: 'membership_required' }, { status: 403 })),
  )
  await userEvent.setup().click(screen.getByRole('button', { name: '刷新' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('没有此空间的访问权限')
  expect(screen.queryByLabelText('会话概览')).not.toBeInTheDocument()
})

describe('display boundaries', () => {
  it('keeps missing dates and unknown faults safe', () => {
    expect(sessionTime(null)).toBe('未知')
    expect(sessionTime('bad')).toBe('未知')
    expect(sessionTime(date)).not.toBe('未知')
    expect(sessionFailure(new Error('secret internal details'))).toBe('读取失败，请重试。')
  })
})

it('hides a cached archive if its next verification fails', async () => {
  serveDetail()
  renderDetail()
  await screen.findByText('完整归档 · 已校验')
  server.use(
    http.get(`${runPath}/history`, () =>
      HttpResponse.json({ code: 'history_invalid' }, { status: 502 }),
    ),
  )
  await userEvent.setup().click(screen.getByRole('button', { name: '刷新' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('归档校验失败')
  expect(screen.queryByLabelText('会话概览')).not.toBeInTheDocument()
})

it('keeps a verified archive usable when optional model metadata cannot load', async () => {
  serveDetail()
  server.use(
    http.get(`${runPath}/thread`, () =>
      HttpResponse.json({ code: 'unavailable' }, { status: 503 }),
    ),
  )
  renderDetail()
  expect(await screen.findByText('完整归档 · 已校验')).toBeInTheDocument()
  expect(await screen.findByText(/模型元信息暂不可用/)).toBeInTheDocument()
})

it('clears a previously visible session list after a forbidden refresh', async () => {
  installCloudSpaceHandlers('member')
  serveProjects()
  server.use(
    http.get(`${base}/agent-sessions`, () =>
      HttpResponse.json({ items: [summary()], nextCursor: '' }),
    ),
  )
  renderAtRoute(
    '/w/:workspaceSlug/sessions',
    <SessionListPage slug={TEST_TENANT_ID} />,
    '/w/cloud-dev/sessions',
  )
  await screen.findByRole('link', { name: '分析会话 r1' })
  server.use(
    http.get(`${base}/agent-sessions`, () =>
      HttpResponse.json({ code: 'membership_required' }, { status: 403 }),
    ),
  )
  await userEvent.setup().click(screen.getByRole('button', { name: '刷新' }))
  expect(await screen.findByRole('alert')).toHaveTextContent('没有此空间的访问权限')
  expect(screen.queryByRole('link', { name: '分析会话 r1' })).not.toBeInTheDocument()
})
