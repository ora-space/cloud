import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { installCloudSpaceHandlers, TEST_SPACE_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderWithProviders } from '@/test/render'
import { AgentConfigurationDialog } from './dialog'
import { agentConnection, opencodeContext } from './test-fixtures'

const PLUGIN = 'official/ora-space.opencode'
const PATH = `/api/v1/me/agent-configurations/${TEST_SPACE_ID}/${PLUGIN}`
const CONNECTIONS = '/api/v1/me/model-connections'
const MODEL_DEFAULT = '/api/v1/me/model-default'

const SECOND = agentConnection({
  id: 'mc2',
  name: 'Second Messages',
  models: [{ id: 'vendor/second', name: 'Second Model', contextWindow: 64000, maxTokens: 4096 }],
})

/** Mutable server state one dialog test reads back through both the read and write boundaries. */
type PluginState = { revision: number; cleared: boolean; settings: Record<string, string> }

/**
 * Serves one plugin with a shared default and two ready connections, so a dialog
 * test can separate the plugin-option write from the independent default draft.
 */
function servePlugin(state: PluginState) {
  server.use(
    http.get(PATH, () =>
      HttpResponse.json(
        opencodeContext({
          spaceId: TEST_SPACE_ID,
          revision: state.revision,
          cleared: state.cleared,
          settings: state.settings,
          modelDefault: { connectionId: 'mc1', modelId: 'vendor/model', version: 2 },
        }),
      ),
    ),
    http.get(CONNECTIONS, () =>
      HttpResponse.json({ items: [agentConnection(), SECOND], nextCursor: '' }),
    ),
    http.get(MODEL_DEFAULT, () =>
      HttpResponse.json({ connectionId: 'mc1', modelId: 'vendor/model', version: 2 }),
    ),
  )
}

/** Reads the settings object a submitted write carried, without asserting the wire shape. */
function submittedSettings(body: unknown): Record<string, string> {
  if (typeof body !== 'object' || body === null || !('settings' in body)) return {}
  const settings = body.settings
  if (typeof settings !== 'object' || settings === null) return {}
  return Object.fromEntries(Object.entries(settings).map(([key, value]) => [key, String(value)]))
}

function serveWrite(state: PluginState) {
  const writes: unknown[] = []
  server.use(
    http.put(PATH, async ({ request }) => {
      const body: unknown = await request.json()
      writes.push(body)
      state.revision += 1
      state.cleared = false
      state.settings = submittedSettings(body)
      return HttpResponse.json({
        resource: { revision: state.revision, cleared: false, settings: state.settings },
      })
    }),
  )
  return writes
}

async function openDialog() {
  const user = userEvent.setup()
  renderWithProviders(<AgentConfigurationDialog pluginId={PLUGIN} />, { slug: 'cloud-dev' })
  await user.click(screen.getByRole('button', { name: '我的配置' }))
  await screen.findByRole('combobox', { name: '默认连接' })
  return user
}

/** Edits only the default draft: neither choice is saved before the scenario acts. */
async function draftDefaultSelection(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('combobox', { name: '默认连接' }))
  await user.click(await screen.findByRole('option', { name: 'Second Messages' }))
  await user.click(screen.getByRole('combobox', { name: '默认模型' }))
  await user.click(await screen.findByRole('option', { name: 'Second Model (vendor/second)' }))
}

function expectDefaultDraftPreserved() {
  expect(screen.getByRole('combobox', { name: '默认连接' })).toHaveTextContent('Second Messages')
  expect(screen.getByRole('combobox', { name: '默认模型' })).toHaveTextContent('Second Model')
  expect(screen.getByRole('button', { name: '保存默认模型' })).toBeEnabled()
}
describe('personal Agent configuration dialog', () => {
  it('advances the option baseline on save without remounting the default draft', async () => {
    installCloudSpaceHandlers('member')
    const state = { revision: 1, cleared: false, settings: { style: 'initial' } }
    servePlugin(state)
    const writes = serveWrite(state)
    const defaults: unknown[] = []
    server.use(
      http.put(MODEL_DEFAULT, async ({ request }) => {
        defaults.push(await request.json())
        return HttpResponse.json({ connectionId: 'mc2', modelId: 'vendor/second', version: 3 })
      }),
    )
    const user = await openDialog()
    await draftDefaultSelection(user)
    await user.type(screen.getByLabelText('风格 *'), ' from the dialog')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual(
      expect.objectContaining({
        revision: 1,
        settings: expect.objectContaining({ style: 'initial from the dialog' }),
      }),
    )

    expectDefaultDraftPreserved()
    await user.click(screen.getByRole('button', { name: '保存默认模型' }))
    await waitFor(() =>
      expect(defaults).toEqual([{ connectionId: 'mc2', modelId: 'vendor/second', version: 2 }]),
    )

    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1]).toEqual(expect.objectContaining({ revision: 2 }))
  })

  it('requires reconciliation when the read after a save shows a later write by another editor', async () => {
    installCloudSpaceHandlers('member')
    const state: PluginState = { revision: 1, cleared: false, settings: { style: 'initial' } }
    servePlugin(state)
    const writes: unknown[] = []
    server.use(
      http.put(PATH, async ({ request }) => {
        const body: unknown = await request.json()
        writes.push(body)
        const confirmed = state.revision + 1
        // Another editor's save lands between this write's response and the follow-up read.
        state.revision = confirmed + 1
        state.settings = { style: 'other editor' }
        return HttpResponse.json({
          resource: { revision: confirmed, cleared: false, settings: submittedSettings(body) },
        })
      }),
    )
    const user = await openDialog()
    await user.type(screen.getByLabelText('风格 *'), ' my edit')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(1))

    expect(await screen.findByRole('heading', { name: '最新保存版本 3' })).toBeInTheDocument()
    expect(screen.getByText('other editor')).toBeInTheDocument()
    expect(screen.getByLabelText('风格 *')).toHaveValue('initial my edit')
    expect(screen.getByRole('button', { name: '保存我的配置' })).toBeDisabled()

    await user.click(screen.getByRole('button', { name: '采用服务器配置' }))
    expect(screen.getByLabelText('风格 *')).toHaveValue('other editor')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(2))
    expect(writes[1]).toEqual(
      expect.objectContaining({
        revision: 3,
        settings: expect.objectContaining({ style: 'other editor' }),
      }),
    )
  })

  it('advances the option baseline on clear without remounting the default draft', async () => {
    installCloudSpaceHandlers('member')
    const state: PluginState = { revision: 1, cleared: false, settings: { style: 'initial' } }
    servePlugin(state)
    const writes = serveWrite(state)
    const removals: unknown[] = []
    server.use(
      http.delete(PATH, async ({ request }) => {
        removals.push(await request.json())
        state.revision += 1
        state.cleared = true
        state.settings = {}
        return HttpResponse.json({
          resource: { revision: state.revision, cleared: true, settings: {} },
        })
      }),
    )
    const user = await openDialog()
    await draftDefaultSelection(user)
    await user.click(screen.getByRole('button', { name: '清除个人选项' }))
    await user.click(screen.getByRole('button', { name: '确认清除' }))
    await waitFor(() => expect(removals).toEqual([{ revision: 1 }]))

    expectDefaultDraftPreserved()
    await user.type(screen.getByLabelText('风格 *'), 'after clearing')
    await user.click(screen.getByRole('button', { name: '保存我的配置' }))
    await waitFor(() => expect(writes).toHaveLength(1))
    expect(writes[0]).toEqual(
      expect.objectContaining({
        revision: 2,
        settings: expect.objectContaining({ style: 'after clearing' }),
      }),
    )
  })
})
