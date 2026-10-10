import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import { ModelConnectionsPage } from './model-connections-page'
import {
  clearCredential,
  modelConnectionFailure,
  writeCredential,
  type ModelConnection,
} from './api'

const CONNECTIONS = '/api/v1/me/model-connections'
const DEFAULT = '/api/v1/me/model-default'

function connection(overrides: Partial<ModelConnection> = {}): ModelConnection {
  return {
    id: 'mc1',
    name: 'Personal OpenAI',
    protocol: 'openai-completions',
    baseUrl: 'https://models.example.com/v1',
    authMode: 'bearer',
    enabled: true,
    credentialConfigured: true,
    version: 3,
    models: [{ id: 'vendor/model', name: 'Model One', contextWindow: 128000, maxTokens: 8192 }],
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-01T00:00:00Z',
    ...overrides,
  }
}

function serve(items: ModelConnection[] = []) {
  server.use(
    http.get(CONNECTIONS, () => HttpResponse.json({ items, nextCursor: '' })),
    http.get(DEFAULT, () => HttpResponse.json({ connectionId: '', modelId: '', version: 0 })),
  )
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  render(
    <QueryClientProvider client={queryClient}>
      <ModelConnectionsPage />
    </QueryClientProvider>,
  )
  return queryClient
}

async function choose(user: ReturnType<typeof userEvent.setup>, label: string, name: string) {
  await user.click(screen.getByRole('combobox', { name: label }))
  await user.click(await screen.findByRole('option', { name }))
}

async function enter(user: ReturnType<typeof userEvent.setup>, input: HTMLElement, value: string) {
  await user.click(input)
  await user.paste(value)
}

function fault(code: string, status: number) {
  return HttpResponse.json({ code, params: {}, requestId: 'r' }, { status })
}

describe('ModelConnectionsPage', () => {
  it.each([
    {
      protocol: 'openai-completions',
      protocolLabel: 'OpenAI Chat Completions',
      authMode: 'bearer',
      authLabel: 'Bearer',
    },
    {
      protocol: 'anthropic-messages',
      protocolLabel: 'Anthropic Messages',
      authMode: 'x-api-key',
      authLabel: 'x-api-key',
    },
  ])(
    'creates $protocol metadata and multiple models without sending a credential',
    async ({ protocol, protocolLabel, authMode, authLabel }) => {
      serve()
      let body: unknown
      let idempotencyKey: string | null = null
      server.use(
        http.post(CONNECTIONS, async ({ request }) => {
          body = await request.json()
          idempotencyKey = request.headers.get('Idempotency-Key')
          return HttpResponse.json({ resource: connection() })
        }),
      )
      const user = userEvent.setup()
      renderPage()
      await user.click(await screen.findByRole('button', { name: '新建连接' }))
      await enter(user, screen.getByLabelText('连接名称'), 'Personal Connection')
      if (protocol === 'anthropic-messages') {
        await choose(user, '协议', protocolLabel)
        await choose(user, '认证方式', authLabel)
      }
      expect(screen.getByRole('combobox', { name: '协议' })).toHaveTextContent(protocolLabel)
      expect(screen.getByRole('combobox', { name: '认证方式' })).toHaveTextContent(authLabel)
      await enter(user, screen.getByLabelText('服务地址'), 'https://messages.example.com/v1')
      await enter(user, screen.getByLabelText('模型标识'), 'vendor/model-a')
      await enter(user, screen.getByLabelText('显示名称'), 'Model A')
      await user.click(screen.getByRole('button', { name: '添加模型' }))
      const lastModel = screen.getAllByRole('group').at(-1)
      if (!lastModel) throw new Error('Expected the newly added model fieldset')
      await enter(user, within(lastModel).getByLabelText('模型标识'), 'vendor/model-b')
      await enter(user, within(lastModel).getByLabelText('显示名称'), 'Model B')
      await user.click(screen.getByRole('button', { name: '保存连接' }))
      await waitFor(() =>
        expect(body).toEqual({
          name: 'Personal Connection',
          protocol,
          baseUrl: 'https://messages.example.com/v1',
          authMode,
          enabled: true,
          models: [
            { id: 'vendor/model-a', name: 'Model A', contextWindow: 128000, maxTokens: 8192 },
            { id: 'vendor/model-b', name: 'Model B', contextWindow: 128000, maxTokens: 8192 },
          ],
        }),
      )
      expect(idempotencyKey).toEqual(expect.any(String))
      expect(screen.queryByLabelText('API Key')).not.toBeInTheDocument()
    },
  )

  it('selects a slash-containing model using the loaded default version and excludes unready connections', async () => {
    serve([
      connection(),
      connection({ id: 'mc2', name: 'No credential', credentialConfigured: false }),
    ])
    let saved: unknown
    server.use(
      http.get(DEFAULT, () => HttpResponse.json({ connectionId: '', modelId: '', version: 4 })),
      http.put(DEFAULT, async ({ request }) => {
        saved = await request.json()
        return HttpResponse.json({ connectionId: 'mc1', modelId: 'vendor/model', version: 5 })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await screen.findByRole('article', { name: 'Personal OpenAI' })
    await user.click(screen.getByRole('combobox', { name: '默认连接' }))
    expect(screen.queryByRole('option', { name: 'No credential' })).not.toBeInTheDocument()
    await user.click(await screen.findByRole('option', { name: 'Personal OpenAI' }))
    await choose(user, '默认模型', 'Model One (vendor/model)')
    await user.click(screen.getByRole('button', { name: '保存默认模型' }))
    await waitFor(() =>
      expect(saved).toEqual({ connectionId: 'mc1', modelId: 'vendor/model', version: 4 }),
    )
  })

  it('sends a write-only secret once, clears the input before completion, and retains only metadata', async () => {
    const item = connection({ credentialConfigured: false })
    serve([item])
    const syntheticKey = crypto.randomUUID()
    let received = false
    let idempotencyKey: string | null = null
    let finish: (() => void) | undefined
    server.use(
      http.put(`${CONNECTIONS}/mc1/credential`, async ({ request }) => {
        idempotencyKey = request.headers.get('Idempotency-Key')
        if (!idempotencyKey) return fault('idempotency_key_required', 400)
        const input: unknown = await request.json()
        received = JSON.stringify(input) === JSON.stringify({ apiKey: syntheticKey, version: 3 })
        await new Promise<void>((resolve) => {
          finish = resolve
        })
        item.credentialConfigured = true
        item.version = 4
        return HttpResponse.json({ resource: item })
      }),
    )
    const user = userEvent.setup()
    const queryClient = renderPage()
    await user.type(await screen.findByLabelText('API Key'), syntheticKey)
    await user.click(screen.getByRole('button', { name: '保存 API Key' }))
    await waitFor(() => expect(received).toBe(true))
    expect(idempotencyKey).toEqual(expect.any(String))
    expect(screen.getByLabelText('API Key')).toHaveValue('')
    expect(queryClient.getMutationCache().getAll()).toHaveLength(0)
    finish?.()
    expect(await screen.findByText('API Key 已更新')).toBeInTheDocument()
    expect(
      JSON.stringify(
        queryClient
          .getQueryCache()
          .getAll()
          .map((query) => query.state.data),
      ),
    ).not.toContain(syntheticKey)
    expect(screen.getByLabelText('API Key')).toHaveValue('')
  })

  it('updates metadata with its version and preserves a refused draft', async () => {
    serve([connection()])
    let body: unknown
    server.use(
      http.put(`${CONNECTIONS}/mc1`, async ({ request }) => {
        body = await request.json()
        return fault('version_conflict', 409)
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '编辑连接' }))
    await user.type(screen.getByLabelText('连接名称'), ' Updated')
    await user.clear(screen.getByLabelText('上下文上限'))
    await user.type(screen.getByLabelText('上下文上限'), '64000')
    await user.clear(screen.getByLabelText('输出上限'))
    await user.type(screen.getByLabelText('输出上限'), '4096')
    await user.click(screen.getByRole('checkbox', { name: '启用连接' }))
    await user.click(screen.getByRole('button', { name: '保存连接' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('配置已在别处修改')
    expect(body).toMatchObject({
      name: 'Personal OpenAI Updated',
      version: 3,
      enabled: false,
      models: [{ id: 'vendor/model', name: 'Model One', contextWindow: 64000, maxTokens: 4096 }],
    })
    expect(screen.getByLabelText('连接名称')).toHaveValue('Personal OpenAI Updated')
    await user.click(screen.getByRole('button', { name: '移除模型' }))
    expect(screen.getByRole('button', { name: '保存连接' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '取消' }))
    expect(screen.queryByLabelText('连接名称')).not.toBeInTheDocument()
  })

  it('clears and deletes versioned resources only after the appropriate action', async () => {
    const item = connection()
    serve([item])
    const deleted: { path: string; body: unknown; key: string | null }[] = []
    server.use(
      http.delete(`${CONNECTIONS}/:id/credential`, async ({ request }) => {
        deleted.push({
          path: 'credential',
          body: await request.json(),
          key: request.headers.get('Idempotency-Key'),
        })
        item.credentialConfigured = false
        item.version = 4
        return HttpResponse.json({ resource: item })
      }),
      http.delete(`${CONNECTIONS}/:id`, async ({ request }) => {
        deleted.push({
          path: 'connection',
          body: await request.json(),
          key: request.headers.get('Idempotency-Key'),
        })
        return HttpResponse.json({ resource: item })
      }),
    )
    const user = userEvent.setup()
    renderPage()
    await user.click(await screen.findByRole('button', { name: '清除 API Key' }))
    expect(await screen.findByText('API Key 已更新')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '删除连接' }))
    expect(deleted).toHaveLength(1)
    await user.click(screen.getByRole('button', { name: '取消删除' }))
    await user.click(screen.getByRole('button', { name: '删除连接' }))
    await user.click(screen.getByRole('button', { name: '确认删除连接' }))
    await waitFor(() =>
      expect(deleted).toEqual([
        { path: 'credential', body: { version: 3 }, key: expect.any(String) },
        { path: 'connection', body: { version: 4 }, key: expect.any(String) },
      ]),
    )
  })

  it('shows metadata load failures and credential failures without raw response details', async () => {
    serve([connection()])
    server.use(
      http.put(`${CONNECTIONS}/mc1/credential`, () => fault('invalid_model_credential', 400)),
    )
    const user = userEvent.setup()
    renderPage()
    await user.type(await screen.findByLabelText('API Key'), crypto.randomUUID())
    await user.click(screen.getByRole('button', { name: '保存 API Key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('API Key 不可为空')
    expect(screen.getByLabelText('API Key')).toHaveValue('')
  })

  it('reports a failed default read rather than offering a partial configuration', async () => {
    serve()
    server.use(http.get(DEFAULT, () => fault('internal', 500)))
    renderPage()
    expect(await screen.findByRole('alert')).toHaveTextContent('模型连接加载失败')
    expect(screen.queryByRole('button', { name: '新建连接' })).not.toBeInTheDocument()
  })

  it('explains an unavailable loaded default and preserves a refused default selection', async () => {
    serve([connection(), connection({ id: 'mc2', name: 'Unavailable', enabled: false })])
    server.use(
      http.get(DEFAULT, () =>
        HttpResponse.json({ connectionId: 'mc2', modelId: 'vendor/model', version: 2 }),
      ),
      http.put(DEFAULT, () => fault('model_connection_unavailable', 409)),
    )
    const user = userEvent.setup()
    renderPage()
    expect(
      await screen.findByText('当前默认不可用，请启用连接并配置 API Key。'),
    ).toBeInTheDocument()
    await choose(user, '默认连接', 'Personal OpenAI')
    await choose(user, '默认模型', 'Model One (vendor/model)')
    await user.click(screen.getByRole('button', { name: '保存默认模型' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('模型连接当前不可用')
    expect(screen.getByRole('combobox', { name: '默认连接' })).toHaveTextContent('Personal OpenAI')
  })

  it('pages every connection and renders disabled connection metadata', async () => {
    serve()
    server.use(
      http.get(CONNECTIONS, ({ request }) => {
        const after = new URL(request.url).searchParams.get('after')
        return HttpResponse.json({
          items: [
            connection({
              id: after ? 'mc2' : 'mc1',
              name: after ? 'Disabled' : 'Personal OpenAI',
              enabled: !after,
            }),
          ],
          nextCursor: after ? '' : 'next',
        })
      }),
    )
    renderPage()
    const card = await screen.findByRole('article', { name: 'Disabled' })
    expect(within(card).getByText('已停用')).toBeInTheDocument()
    expect(screen.getAllByLabelText('API Key')).toHaveLength(2)
  })
})

describe('credential boundary', () => {
  it('drops request configuration from rejections for both credential operations', async () => {
    const item = connection()
    server.use(
      http.put(`${CONNECTIONS}/mc1/credential`, () => fault('version_conflict', 409)),
      http.delete(`${CONNECTIONS}/mc1/credential`, () => fault('version_conflict', 409)),
    )
    await expect(writeCredential(item, crypto.randomUUID())).rejects.toEqual(
      new Error('version_conflict'),
    )
    await expect(clearCredential(item)).rejects.toEqual(new Error('version_conflict'))
    expect(modelConnectionFailure(new Error('unknown'))).toBe('操作失败，请稍后重试')
  })
})
