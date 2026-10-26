import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import type { ScheduledTask } from './api'
import { ScheduledTasksPage } from './scheduled-tasks-page'

const TID = 'tid1'
const LIST = '/api/v1/tenants/tid1/scheduled-tasks'
const ISSUES = '/api/v1/tenants/tid1/issues'
const TARGETS = '/api/v1/tenants/tid1/collaboration/targets'
const MODEL_CONNECTIONS = '/api/v1/me/model-connections'
const MODEL_DEFAULT = '/api/v1/me/model-default'

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function task(overrides: Partial<ScheduledTask> = {}): ScheduledTask {
  return {
    id: 'st1',
    issueId: 'i1',
    creatorUserId: 'u1',
    name: '每日同步',
    prompt: '同步仓库',
    agentId: 'a1',
    schedule: { kind: 'daily', hour: 9, minute: 0 },
    timezone: 'Asia/Shanghai',
    revision: 1,
    version: 1,
    enabled: false,
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    model: null,
    ...overrides,
  }
}

function serve(tasks: ScheduledTask[] = [], acceptanceEnabled = false) {
  server.use(
    http.get(LIST, () => HttpResponse.json({ tasks, acceptanceEnabled })),
    http.get(ISSUES, () =>
      HttpResponse.json({ items: [{ id: 'i1', number: 1, title: '问题一' }], nextCursor: '' }),
    ),
    http.get(TARGETS, () =>
      HttpResponse.json({
        items: [{ type: 'agent', id: 'a1', displayName: 'Codex', description: '' }],
        nextCursor: '',
      }),
    ),
    http.get(MODEL_CONNECTIONS, () =>
      HttpResponse.json({
        items: [
          {
            id: 'mc1',
            name: '个人模型',
            protocol: 'openai-completions',
            baseUrl: 'https://models.example.test/v1',
            authMode: 'bearer',
            models: [
              { id: 'vendor/model-v1', name: 'Model V1', contextWindow: 128000, maxTokens: 8192 },
            ],
            enabled: true,
            credentialConfigured: true,
            version: 1,
            createdAt: '2026-10-01T00:00:00Z',
            updatedAt: '2026-10-01T00:00:00Z',
          },
        ],
        nextCursor: '',
      }),
    ),
    http.get(MODEL_DEFAULT, () =>
      HttpResponse.json({ connectionId: 'mc1', modelId: 'vendor/model-v1', version: 1 }),
    ),
  )
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <ScheduledTasksPage slug={TID} />
    </QueryClientProvider>,
  )
}

describe('ScheduledTasksPage', () => {
  it('renders the task list with its schedule summary and enabled state', async () => {
    serve([
      task(),
      task({
        id: 'st2',
        name: '每周报告',
        schedule: { kind: 'weekly', hour: 18, minute: 30, weekdays: ['monday', 'friday'] },
        enabled: true,
      }),
    ])
    renderPage()
    const first = await screen.findByRole('article', { name: '每日同步' })
    expect(first).toHaveTextContent('每天 09:00')
    expect(first).toHaveTextContent('已停用')
    const second = await screen.findByRole('article', { name: '每周报告' })
    expect(second).toHaveTextContent('每周 周一、周五 18:30')
    expect(second).toHaveTextContent('已启用')
  })

  it('hides creation while the deployed acceptance gate is closed', async () => {
    serve()
    renderPage()
    await screen.findByText('还没有定时任务')
    expect(screen.queryByRole('button', { name: '新建定时任务' })).not.toBeInTheDocument()
  })

  it('opens the existing creation form when the deployed acceptance gate is open', async () => {
    serve([], true)
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '新建定时任务' }))
    expect(screen.getByRole('dialog', { name: '新建定时任务' })).toBeInTheDocument()
  })

  it('shows the risk disclosure and requires consent once the task is enabled', async () => {
    serve([], true)
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '新建定时任务' }))
    await user.click(screen.getByRole('switch'))
    expect(screen.getByLabelText('同意无人值守授权')).toBeInTheDocument()
    expect(screen.getByText(/我已知悉并同意/)).toBeInTheDocument()
  })

  it('creates a disabled task as an unauthorised draft while offering the default-connection models', async () => {
    serve([], true)
    let created: Record<string, unknown> | undefined
    server.use(
      http.post(LIST, async ({ request }) => {
        const payload = await request.json()
        if (!isRecord(payload)) {
          return HttpResponse.json({ error: 'expected object' }, { status: 400 })
        }
        created = payload
        return HttpResponse.json({ resource: task() })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '新建定时任务' }))
    await user.selectOptions(screen.getByLabelText('绑定 issue'), 'i1')
    await user.selectOptions(screen.getByLabelText('执行智能体'), 'a1')
    await user.type(screen.getByLabelText('名称'), '无授权草稿')
    await user.type(screen.getByLabelText('提示词'), '稍后再启用')
    expect(await screen.findByRole('option', { name: /Model V1/ })).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '创建' }))
    await waitFor(() => expect(created).toBeDefined())
    expect(created).toMatchObject({ enabled: false, model: '' })
    expect(created).not.toHaveProperty('approval')
  })

  it('deletes a task only after confirmation', async () => {
    serve([task()])
    let deleted = null
    server.use(
      http.delete(`${LIST}/st1`, () => {
        deleted = 'st1'
        return HttpResponse.json({ taskId: 'st1' })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '删除' }))
    expect(deleted).toBeNull()
    await user.click(screen.getByRole('button', { name: '确认删除' }))
    expect(deleted).toBe('st1')
  })

  it('disables an enabled task through the switch', async () => {
    serve([task({ enabled: true })])
    let disabled = false
    server.use(
      http.post(`${LIST}/st1/disable`, () => {
        disabled = true
        return HttpResponse.json({ resource: task({ enabled: false }) })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    expect(await screen.findByRole('article', { name: '每日同步' })).toHaveTextContent('已启用')
    await user.click(screen.getByRole('switch'))
    expect(disabled).toBe(true)
  })

  it('requires an explicit re-consent before enabling a stopped task', async () => {
    serve([task()])
    let approval: Record<string, unknown> | undefined
    server.use(
      http.post(`${LIST}/st1/enable`, async ({ request }) => {
        const payload = await request.json()
        if (!isRecord(payload)) {
          return HttpResponse.json({ error: 'expected object' }, { status: 400 })
        }
        approval = payload
        return HttpResponse.json({ resource: task({ enabled: true }) })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await screen.findByRole('article', { name: '每日同步' })
    const enable = screen.getByRole('button', { name: '重新同意并启用' })
    expect(enable).toBeDisabled()
    await user.click(screen.getByLabelText('同意无人值守授权'))
    await user.click(enable)
    await waitFor(() =>
      expect(approval).toEqual({ approval: { policy: 'full_auto_in_cloud_workspace' } }),
    )
  })

  it('clears a stale consent box after enable and revoke so re-consent is explicit', async () => {
    let current: ScheduledTask = task({
      enabled: false,
      unattendedPolicy: 'full_auto_in_cloud_workspace',
    })
    server.use(
      http.get(LIST, () => HttpResponse.json({ tasks: [current], acceptanceEnabled: false })),
      http.post(`${LIST}/st1/enable`, () => {
        current = task({ enabled: true, unattendedPolicy: 'full_auto_in_cloud_workspace' })
        return HttpResponse.json({ resource: current })
      }),
      http.post(`${LIST}/st1/revoke-consent`, () => {
        current = task({ enabled: false })
        return HttpResponse.json({ resource: current })
      }),
    )
    const userEventSetup = userEvent.setup()
    renderPage()

    await screen.findByRole('article', { name: '每日同步' })
    await userEventSetup.click(screen.getByLabelText('同意无人值守授权'))
    await userEventSetup.click(screen.getByRole('button', { name: '重新同意并启用' }))
    await waitFor(() => expect(screen.getByRole('switch')).toBeChecked())

    await userEventSetup.click(screen.getByRole('button', { name: '撤销授权' }))
    await userEventSetup.click(screen.getByRole('button', { name: '确认撤销授权' }))
    const fresh = await screen.findByLabelText('同意无人值守授权')
    expect(fresh).not.toBeChecked()
    expect(screen.getByRole('button', { name: '重新同意并启用' })).toBeDisabled()

    await userEventSetup.click(fresh)
    expect(screen.getByRole('button', { name: '重新同意并启用' })).toBeEnabled()
  })
})
