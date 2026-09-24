import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { PluginsPage } from '@/features/plugins/plugins-page'
import {
  catalogEntry,
  pluginCloudHandlers,
  PLUGIN_FIXTURE_SYNCED_AT,
  spacePlugin,
} from '@/mocks/handlers/plugins'
import { installCloudSpaceHandlers } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'

const FIXTURE_CATALOG = [
  catalogEntry('official/hello-world', {
    kind: 'agent',
    title: 'Hello World',
    description: 'A friendly agent.',
  }),
  catalogEntry('official/native-tool', { kind: 'hook', title: 'Native Tool', version: '2.0.0' }),
  catalogEntry('official/all-in-one', { kind: 'pack', title: 'All In One' }),
]

/** Opens the removal dialog of the selected section and confirms it. */
async function confirmRemove(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(screen.getByRole('button', { name: '移除' }))
  const dialog = await screen.findByRole('alertdialog')
  await user.click(within(dialog).getByRole('button', { name: '移除' }))
}

/** Installs the signed-in cloud fixtures plus the plugin endpoint fixtures. */
function installFixtures(
  options: {
    plugins?: ReturnType<typeof spacePlugin>[]
    removeConflict?: 'version_conflict'
    onRemove?: (identifier: string, version: number) => void
  } = {},
) {
  installCloudSpaceHandlers('owner')
  server.use(...pluginCloudHandlers({ catalog: FIXTURE_CATALOG, ...options }))
}

describe('PluginsPage', () => {
  it('renders the catalog grid with titles, versions, kind chips and a disabled pack', async () => {
    installFixtures()
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    expect(await screen.findByText('Hello World')).toBeInTheDocument()
    expect(screen.getByText('Native Tool')).toBeInTheDocument()
    expect(screen.getByText('All In One')).toBeInTheDocument()
    expect(screen.getAllByText('v1.0.0').length).toBeGreaterThan(0)
    expect(screen.getAllByText('v2.0.0').length).toBeGreaterThan(0)
    for (const kind of ['智能体', '工作台', 'WebView', '技能', 'MCP', '钩子', '插件包']) {
      expect(screen.getByRole('button', { name: kind })).toBeInTheDocument()
    }
    expect(screen.getByRole('button', { name: '暂不支持' })).toBeDisabled()
  })

  it('filters by search text and kind without issuing new requests', async () => {
    let catalogReads = 0
    installFixtures()
    server.use(
      http.get('/api/v1/tenants/:tid/spaces/:spaceId/plugins/catalog', () => {
        catalogReads += 1
        return HttpResponse.json({ items: FIXTURE_CATALOG, syncedAt: PLUGIN_FIXTURE_SYNCED_AT })
      }),
    )
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    await screen.findByText('Hello World')

    const user = userEvent.setup()
    await user.type(screen.getByRole('textbox', { name: '搜索插件' }), 'native')
    expect(screen.queryByText('Hello World')).not.toBeInTheDocument()
    expect(screen.getByText('Native Tool')).toBeInTheDocument()

    // The kind chip narrows the same already-fetched snapshot further.
    await user.clear(screen.getByRole('textbox', { name: '搜索插件' }))
    await user.click(screen.getByRole('button', { name: '插件包' }))
    expect(screen.queryByText('Native Tool')).not.toBeInTheDocument()
    expect(screen.getByText('All In One')).toBeInTheDocument()
    // Filtering is pure frontend state: the catalog was fetched once.
    expect(catalogReads).toBe(1)
  })

  it('installs a plugin: the POST fires, the row appears pending and the button disables', async () => {
    installFixtures()
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    const user = userEvent.setup()
    const installButtons = await screen.findAllByRole('button', { name: '安装' })
    const first = installButtons[0]
    if (!first) throw new Error('the catalog must offer at least one install button')
    await user.click(first)

    // The id appears in the catalog card and in the selected list; scope the
    // assertion to the selected section, then pin the badge and button state.
    const selected = screen.getByRole('region', { name: '已选插件' })
    expect(await within(selected).findByText('official/hello-world')).toBeInTheDocument()
    expect(within(selected).getByText('待安装')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '安装中' })).toBeDisabled()
  })

  it('shows every observed state badge with its error summary', async () => {
    installFixtures({
      plugins: [
        spacePlugin('official/hello-world', { observedState: 'pending', observedVersion: null }),
        spacePlugin('official/native-tool', {
          observedState: 'failed',
          observedVersion: null,
          installError: 'sha256 mismatch',
        }),
      ],
    })
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    expect(await screen.findByText('待安装')).toBeInTheDocument()
    expect(screen.getByText('失败')).toBeInTheDocument()
    expect(screen.getByText(/sha256 mismatch/)).toBeInTheDocument()
  })

  it('removes a plugin after confirmation and sends the optimistic version', async () => {
    const sent: Array<{ identifier: string; version: number }> = []
    installFixtures({
      plugins: [spacePlugin('official/hello-world', { version: 4 })],
      onRemove: (identifier, version) => sent.push({ identifier, version }),
    })
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    const user = userEvent.setup()
    const selected = screen.getByRole('region', { name: '已选插件' })
    await within(selected).findByText('official/hello-world')
    await confirmRemove(user)

    await waitFor(() =>
      expect(within(selected).queryByText('official/hello-world')).not.toBeInTheDocument(),
    )
    expect(sent).toEqual([{ identifier: 'official/hello-world', version: 4 }])
  })

  it('refetches the latest row when a removal hits a version conflict', async () => {
    installFixtures({
      plugins: [spacePlugin('official/hello-world', { version: 4 })],
      removeConflict: 'version_conflict',
    })
    renderWithProviders(<PluginsPage slug="cloud-dev" />, { slug: 'cloud-dev' })
    const user = userEvent.setup()
    const selected = screen.getByRole('region', { name: '已选插件' })
    await within(selected).findByText('official/hello-world')
    await confirmRemove(user)

    // The conflict is surfaced inline and the row stays, refetched fresh.
    expect(await within(selected).findByText(/版本冲突/)).toBeInTheDocument()
    expect(within(selected).getByText('official/hello-world')).toBeInTheDocument()
  })
})
