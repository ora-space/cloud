import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { ModelConnection } from '@/features/model-connections/api'
import { getGetApiV1MeModelDefaultQueryKey } from '@/api/me/me'
import { server } from '@/test/msw-server'
import type { AgentConfiguration } from './api'
import { AgentConfigurationEditor } from './editor'
import { agentConnection, agentContext, opencodeContext } from './test-fixtures'

const PATH = '/api/v1/me/agent-configurations/space1/official/ora-space.claude'
const CONNECTIONS = '/api/v1/me/model-connections'
const MODEL_DEFAULT = '/api/v1/me/model-default'

function serveConnections(items: ModelConnection[] = [agentConnection()]) {
  server.use(http.get(CONNECTIONS, () => HttpResponse.json({ items, nextCursor: '' })))
}

function mountEditor(
  context = agentContext(),
  {
    pluginId = 'official/ora-space.claude',
    connections = [agentConnection()],
  }: { pluginId?: string; connections?: ModelConnection[] } = {},
) {
  serveConnections(connections)
  server.use(
    http.get(MODEL_DEFAULT, () =>
      HttpResponse.json({ connectionId: 'mc1', modelId: 'vendor/model', version: 2 }),
    ),
  )
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  const onSaved = vi.fn<() => Promise<AgentConfiguration>>(async () => context)
  const onReload = vi.fn<() => Promise<AgentConfiguration>>(async () => context)
  const scope = { spaceId: 'space1', pluginId }
  function element(current: AgentConfiguration) {
    return (
      <QueryClientProvider client={client}>
        <AgentConfigurationEditor
          context={current}
          scope={scope}
          onSaved={onSaved}
          onReload={onReload}
        />
      </QueryClientProvider>
    )
  }
  const rendered = render(element(context))
  return {
    client,
    onSaved,
    onReload,
    rerender: (current: AgentConfiguration) => rendered.rerender(element(current)),
  }
}

async function enter(user: ReturnType<typeof userEvent.setup>, label: string, value: string) {
  const input = screen.getByLabelText(label)
  await user.clear(input)
  await user.paste(value)
}

describe('personal Agent configuration editor', () => {
  it('keeps exact numeric text through the rendered control and the real HTTP serialization', async () => {
    let submitted = ''
    server.use(
      http.put(PATH, async ({ request }) => {
        submitted = await request.text()
        return HttpResponse.json({ resource: { revision: 2, cleared: false, settings: {} } })
      }),
    )
    mountEditor(
      agentContext({
        settings: { style: 'exact', budget: 9007199254740992 },
        numericValues: { budget: '9007199254740993' },
      }),
    )
    const user = userEvent.setup()
    expect(screen.getByLabelText('预算')).toHaveAttribute('value', '9007199254740993')
    fireEvent.change(screen.getByLabelText('预算'), {
      target: { value: '0.123456789012345678901' },
    })
    const save = screen.getByRole('button', { name: '保存我的配置' })
    expect(save).toBeEnabled()
    expect(save.closest('form')?.checkValidity()).toBe(true)
    await user.click(save)
    await waitFor(() => expect(submitted).toContain('"budget":0.123456789012345678901'))
  })

  it('creates a library connection inline and selects its model without saving options implicitly', async () => {
    const created = agentConnection({
      id: 'mc2',
      name: 'New Messages',
      credentialConfigured: false,
    })
    let payload: unknown
    let submitted: unknown
    server.use(
      http.post(CONNECTIONS, async ({ request }) => {
        payload = await request.json()
        server.use(
          http.get(CONNECTIONS, () =>
            HttpResponse.json({ items: [agentConnection(), created], nextCursor: '' }),
          ),
        )
        return HttpResponse.json({ resource: created })
      }),
      http.put(PATH, async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ resource: { revision: 2, cleared: false, settings: {} } })
      }),
    )
    mountEditor()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '新建个人连接' }))
    expect(screen.getByRole('combobox', { name: '协议' })).toHaveTextContent('Anthropic Messages')
    await enter(user, '连接名称', 'New Messages')
    await enter(user, '服务地址', 'https://models.example.invalid')
    await enter(user, '模型标识', 'vendor/model')
    await enter(user, '显示名称', 'Private Model')
    await user.click(screen.getByRole('button', { name: '保存连接' }))
    expect(await screen.findByRole('combobox', { name: '模型连接' })).toHaveTextContent(
      'New Messages',
    )
    expect(submitted).toBeUndefined()
    expect(payload).toEqual(
      expect.objectContaining({ name: 'New Messages', protocol: 'anthropic-messages' }),
    )
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() =>
      expect(submitted).toEqual(
        expect.objectContaining({ connectionId: 'mc2', modelId: 'vendor/model' }),
      ),
    )
    expect(document.querySelectorAll('form form')).toHaveLength(0)
  })

  it('saves typed values and a frozen edit baseline with a stable idempotency key', async () => {
    let submitted: unknown
    let key: string | null = null
    server.use(
      http.put(PATH, async ({ request }) => {
        submitted = await request.json()
        key = request.headers.get('Idempotency-Key')
        return HttpResponse.json({ resource: { revision: 2, cleared: false, settings: {} } })
      }),
    )
    const { onSaved } = mountEditor()
    const user = userEvent.setup()
    await screen.findByRole('combobox', { name: '模型连接' })
    await enter(user, '风格 *', 'literal $(not-a-command)')
    await enter(user, '预算', '2.5')
    await user.selectOptions(screen.getByLabelText('详细输出'), 'true')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(onSaved).toHaveBeenCalledOnce())
    expect(submitted).toEqual({
      revision: 1,
      selectionVersion: 3,
      declarationFingerprint: 'a'.repeat(64),
      connectionId: 'mc1',
      modelId: 'vendor/model',
      settings: { style: 'literal $(not-a-command)', budget: 2.5, verbose: true },
    })
    expect(key).toEqual(expect.any(String))
    expect(document.querySelectorAll('form form')).toHaveLength(0)
  })

  it('preserves a stale draft and requires an explicit reconciliation before advancing revision', async () => {
    const writes: unknown[] = []
    server.use(
      http.put(PATH, async ({ request }) => {
        writes.push(await request.json())
        return writes.length === 1
          ? HttpResponse.json({ code: 'revision_conflict', params: {} }, { status: 409 })
          : HttpResponse.json({ resource: { revision: 3, cleared: false, settings: {} } })
      }),
    )
    const page = mountEditor()
    const user = userEvent.setup()
    await enter(user, '风格 *', 'my retained draft')
    page.rerender(
      agentContext({
        revision: 2,
        settings: { style: 'server winner', budget: 0.123 },
        numericValues: { budget: '0.123456789012345678901' },
      }),
    )
    expect(screen.getByLabelText('风格 *')).toHaveValue('my retained draft')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('草稿已保留')
    await user.click(screen.getByRole('button', { name: '重新核对最新配置' }))
    expect(await screen.findByText('server winner')).toBeInTheDocument()
    expect(screen.getByText('0.123456789012345678901')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '保存我的配置' })).toBeDisabled()
    await user.click(screen.getByRole('button', { name: '保留草稿，按最新要求核对' }))
    expect(screen.getByLabelText('风格 *')).toHaveValue('my retained draft')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[0]).toEqual(expect.objectContaining({ revision: 1 }))
    expect(writes[1]).toEqual(
      expect.objectContaining({
        revision: 2,
        settings: expect.objectContaining({ style: 'my retained draft' }),
      }),
    )
  })

  it('repairs only after confirmation, names removed fields and can adopt the server values', async () => {
    let submitted: unknown
    server.use(
      http.post(PATH + '/repair', async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ resource: { revision: 2, cleared: false, settings: {} } })
      }),
    )
    const context = agentContext({
      requiresRepair: true,
      settings: { style: 'old', retiredOption: true },
    })
    const page = mountEditor(context)
    const user = userEvent.setup()
    expect(screen.getByText(/retiredOption/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '确认修复并保存' })).toBeDisabled()
    await user.click(screen.getByRole('checkbox', { name: '我已核对当前版本的选项和模型' }))
    await user.click(screen.getByRole('button', { name: '确认修复并保存' }))
    await waitFor(() => expect(page.onSaved).toHaveBeenCalledOnce())
    expect(submitted).toEqual(
      expect.objectContaining({ settings: { style: 'old', budget: 5, verbose: false } }),
    )
    await enter(user, '风格 *', 'unsaved')
    page.rerender(agentContext({ revision: 2, settings: { style: 'new server' } }))
    await user.click(screen.getByRole('button', { name: '重新核对最新配置' }))
    await user.click(await screen.findByRole('button', { name: '采用服务器配置' }))
    expect(screen.getByLabelText('风格 *')).toHaveValue('new server')
  })

  it('clears only personal options with confirmation and preserves the connection library', async () => {
    let submitted: unknown
    server.use(
      http.delete(PATH, async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ resource: { revision: 2, cleared: true, settings: {} } })
      }),
    )
    const page = mountEditor()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '清除个人选项' }))
    await user.click(screen.getByRole('button', { name: '取消清除' }))
    expect(submitted).toBeUndefined()
    await user.click(screen.getByRole('button', { name: '清除个人选项' }))
    await user.click(screen.getByRole('button', { name: '确认清除' }))
    await waitFor(() => expect(page.onSaved).toHaveBeenCalledOnce())
    expect(submitted).toEqual({ revision: 1 })
    expect(await screen.findByText('API Key 已配置', { exact: false })).toBeInTheDocument()
  })

  it('keeps write-only credentials outside query and mutation caches and clears input before I/O', async () => {
    const secret = 'synthetic-' + crypto.randomUUID()
    const page = mountEditor()
    let requestInputWasCleared = false
    server.use(
      http.put(CONNECTIONS + '/mc1/credential', async ({ request }) => {
        requestInputWasCleared = screen.getByLabelText('API Key').getAttribute('value') !== secret
        const body = await request.json()
        expect(body).toEqual({ apiKey: secret, version: 1 })
        expect(screen.getByLabelText('API Key')).toHaveValue('')
        return HttpResponse.json(
          { code: 'invalid_model_credential', params: { privateValue: secret } },
          { status: 400 },
        )
      }),
    )
    const user = userEvent.setup()
    await user.type(await screen.findByLabelText('API Key'), secret)
    await user.click(screen.getByRole('button', { name: '保存 API Key' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('API Key 不可为空')
    expect(requestInputWasCleared).toBe(true)
    expect(
      JSON.stringify(
        page.client
          .getQueryCache()
          .getAll()
          .map((query) => query.state.data),
      ),
    ).not.toContain(secret)
    expect(
      JSON.stringify(
        page.client
          .getMutationCache()
          .getAll()
          .map((mutation) => mutation.state.variables),
      ),
    ).not.toContain(secret)
    expect(screen.queryByText(secret)).not.toBeInTheDocument()
  })
})

describe('global default credentials', () => {
  it('keeps credentials on the mounted default draft when the server default changes in the background', async () => {
    const second = agentConnection({ id: 'mc2', name: 'Second Messages' })
    const page = mountEditor(opencodeContext(), {
      pluginId: 'official/ora-space.opencode',
      connections: [agentConnection(), second],
    })
    await screen.findByText('API Key 属于所选连接：Personal Messages')
    server.use(
      http.get(MODEL_DEFAULT, () =>
        HttpResponse.json({
          connectionId: 'mc2',
          modelId: 'vendor/model',
          version: 3,
        }),
      ),
    )
    await act(async () => {
      await page.client.invalidateQueries({ queryKey: getGetApiV1MeModelDefaultQueryKey() })
    })
    expect(screen.getByRole('combobox', { name: '默认连接' })).toHaveTextContent(
      'Personal Messages',
    )
    expect(await screen.findByText('当前默认：Second Messages · Private Model')).toBeInTheDocument()
    expect(screen.getByText('API Key 属于所选连接：Personal Messages')).toBeInTheDocument()
  })

  it('targets the existing default connection although the plugin stores none', async () => {
    let cleared = ''
    server.use(
      http.delete(CONNECTIONS + '/mc1/credential', ({ request }) => {
        cleared = request.url
        return HttpResponse.json({ resource: agentConnection({ credentialConfigured: false }) })
      }),
    )
    mountEditor(opencodeContext(), { pluginId: 'official/ora-space.opencode' })
    const user = userEvent.setup()
    expect(await screen.findByText('API Key 已配置', { exact: false })).toBeInTheDocument()
    expect(screen.getByText('API Key 属于所选连接：Personal Messages')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '清除 API Key' }))
    await waitFor(() => expect(cleared).toContain('/mc1/credential'))
  })

  it('offers the write-only control when the default connection has no key', async () => {
    let submitted: unknown
    const keyless = agentConnection({ credentialConfigured: false })
    server.use(
      http.put(CONNECTIONS + '/mc1/credential', async ({ request }) => {
        submitted = await request.json()
        return HttpResponse.json({ resource: agentConnection({ credentialConfigured: true }) })
      }),
    )
    mountEditor(opencodeContext(), {
      pluginId: 'official/ora-space.opencode',
      connections: [keyless],
    })
    const user = userEvent.setup()
    expect(await screen.findByText('API Key 未配置', { exact: false })).toBeInTheDocument()
    await user.type(screen.getByLabelText('API Key'), 'synthetic-key')
    await user.click(screen.getByRole('button', { name: '保存 API Key' }))
    await waitFor(() => expect(submitted).toEqual({ apiKey: 'synthetic-key', version: 1 }))
  })

  it('follows the connection chosen in the default draft', async () => {
    const second = agentConnection({
      id: 'mc2',
      name: 'Second Messages',
      models: [
        { id: 'vendor/second', name: 'Second Model', contextWindow: 64000, maxTokens: 4096 },
      ],
    })
    mountEditor(opencodeContext(), {
      pluginId: 'official/ora-space.opencode',
      connections: [agentConnection(), second],
    })
    const user = userEvent.setup()
    expect(await screen.findByText('API Key 属于所选连接：Personal Messages')).toBeInTheDocument()
    await user.click(screen.getByRole('combobox', { name: '默认连接' }))
    await user.click(await screen.findByRole('option', { name: 'Second Messages' }))
    expect(screen.getByText('API Key 属于所选连接：Second Messages')).toBeInTheDocument()
  })
})
