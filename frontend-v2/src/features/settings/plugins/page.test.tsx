import { beforeEach, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { PluginCatalogEntry } from '@/api/generated.schemas'
import {
  installMarketplaceBackend,
  marketplaceEntry,
  marketplaceSelection,
  renderSettingsPage,
} from '../test-utils'
import PluginsPage from './page'
import { useDemoPluginSelections } from './demo-state'

const writing = marketplaceEntry('writing-kit', {
  kind: 'skill',
  title: '写作工具箱',
  description: '让团队文档保持清晰与一致。',
  logo: {
    universal: {
      role: 'universal',
      extension: 'svg',
      data: 'data:image/svg+xml;base64,PHN2Zy8+',
    },
  } as PluginCatalogEntry['logo'],
})
const github = marketplaceEntry('github-tools', {
  title: 'GitHub 工具',
  version: '2.1.0',
})
const pack = marketplaceEntry('starter-pack', {
  kind: 'pack',
  title: '团队入门合集',
})
const official = marketplaceEntry('mattpocock.skills', {
  kind: 'skill',
  title: 'Matt Pocock Skills',
  sourceNamespace: 'official',
})

beforeEach(() => {
  useDemoPluginSelections.getState().reset()
  localStorage.clear()
})

it('searches and filters the server catalog, then confirms a server-backed removal', async () => {
  const backend = installMarketplaceBackend({
    catalog: [writing, github, pack],
    selections: [marketplaceSelection(writing, { version: 4 })],
  })
  const user = userEvent.setup()
  renderSettingsPage(<PluginsPage />, '/settings/plugins')
  const row = await screen.findByRole('article', { name: '写作工具箱' })
  expect(screen.getByRole('heading', { name: '技能' })).toBeInTheDocument()
  expect(screen.getByRole('heading', { name: 'MCP' })).toBeInTheDocument()
  expect(screen.getByRole('heading', { name: '合集' })).toBeInTheDocument()
  expect(
    within(row).getByText('技能', { selector: '.settings-plugin-kind' }),
  ).toBeInTheDocument()
  expect(row.querySelector('img')).toHaveAttribute(
    'src',
    'data:image/svg+xml;base64,PHN2Zy8+',
  )
  await user.type(screen.getByRole('textbox', { name: '搜索插件' }), '写作')
  expect(
    screen.queryByRole('article', { name: 'GitHub 工具' }),
  ).not.toBeInTheDocument()
  await user.click(
    within(row).getByRole('button', { name: '查看插件 写作工具箱' }),
  )
  expect(await screen.findByRole('dialog')).toHaveTextContent(
    'demo/writing-kit',
  )
  await user.click(screen.getByRole('button', { name: '关闭详情' }))
  await user.click(within(row).getByRole('button', { name: '移除' }))
  const dialog = await screen.findByRole('alertdialog')
  await user.click(within(dialog).getByRole('button', { name: '移除' }))
  await waitFor(() =>
    expect(within(row).getByText('未安装')).toBeInTheDocument(),
  )
  expect(backend.bodies.at(-1)).toEqual({
    identifier: 'demo/writing-kit',
    version: 4,
  })
})

it('installs and removes MCP and Hooks as browser-local demo state', async () => {
  const backend = installMarketplaceBackend({
    catalog: [writing, github, pack],
  })
  const user = userEvent.setup()
  renderSettingsPage(<PluginsPage />)
  const row = await screen.findByRole('article', { name: 'GitHub 工具' })
  await user.click(within(row).getByRole('button', { name: '安装' }))
  expect(
    within(row).getByText('已安装（演示）', { exact: true }),
  ).toBeInTheDocument()
  await user.click(within(row).getByRole('button', { name: '移除' }))
  const dialog = await screen.findByRole('alertdialog')
  expect(dialog).toHaveTextContent('不会卸载真实插件包')
  await user.click(within(dialog).getByRole('button', { name: '移除' }))
  expect(within(row).getByText('未安装', { exact: true })).toBeInTheDocument()

  const hook = screen.getByRole('article', { name: '示例 Hooks' })
  expect(screen.getByRole('heading', { name: 'Hooks' })).toBeInTheDocument()
  await user.click(within(hook).getByRole('button', { name: '安装' }))
  expect(
    within(hook).getByText('已安装（演示）', { exact: true }),
  ).toBeInTheDocument()
  expect(localStorage.getItem('ora-v2-plugin-demo:v1')).toContain(
    'demo/sample-hooks',
  )

  await user.click(
    within(row).getByRole('button', { name: '查看插件 GitHub 工具' }),
  )
  expect(await screen.findByRole('dialog')).toHaveTextContent(
    'demo/github-tools',
  )
  expect(backend.bodies).toHaveLength(0)
})

it('keeps unsupported packs disabled and clears no-result filters', async () => {
  installMarketplaceBackend({ catalog: [writing, github, pack] })
  const user = userEvent.setup()
  renderSettingsPage(<PluginsPage />)
  const packRow = await screen.findByRole('article', { name: '团队入门合集' })
  expect(within(packRow).getByText('合集安装尚未支持')).toBeInTheDocument()
  await user.type(
    screen.getByRole('textbox', { name: '搜索插件' }),
    'nothing-matches',
  )
  await user.click(screen.getByRole('button', { name: '清除筛选' }))
  expect(screen.getAllByRole('article')).toHaveLength(4)
})

it('lets a member browse the catalog but not change it', async () => {
  installMarketplaceBackend({
    role: 'member',
    catalog: [writing, github],
    selections: [marketplaceSelection(github)],
  })
  renderSettingsPage(<PluginsPage />)
  const row = await screen.findByRole('article', { name: '写作工具箱' })
  expect(within(row).getByRole('button', { name: '安装' })).toBeDisabled()
  expect(
    screen.getByText(/安装与移除需要工作区 owner 权限/),
  ).toBeInTheDocument()
})

it('keeps runtime-owned Agents out of the plugin marketplace', async () => {
  installMarketplaceBackend({
    catalog: [
      writing,
      marketplaceEntry('ora-space.opencode', {
        kind: 'agent',
        title: 'OpenCode',
        sourceNamespace: 'official',
      }),
    ],
  })
  renderSettingsPage(<PluginsPage />)
  expect(
    await screen.findByRole('article', { name: '写作工具箱' }),
  ).toBeVisible()
  expect(
    screen.queryByRole('article', { name: 'OpenCode' }),
  ).not.toBeInTheDocument()
})

it('shows the shared official market and manages space sources', async () => {
  installMarketplaceBackend({ catalog: [official] })
  const user = userEvent.setup()
  renderSettingsPage(<PluginsPage />)
  await user.click(screen.getByRole('button', { name: '管理市场' }))
  const dialog = await screen.findByRole('dialog')
  const officialRow = within(dialog).getByRole('article', {
    name: '官方市场',
  })
  expect(officialRow).toHaveTextContent('https://github.com/ora-space/marketplace')
  expect(
    within(officialRow).getByRole('button', { name: '同步' }),
  ).toBeEnabled()

  const sourceURL = 'https://github.com/ora-space/marketplace.git'
  await user.type(
    within(dialog).getByLabelText('HTTPS 仓库地址'),
    sourceURL,
  )
  await user.click(within(dialog).getByRole('button', { name: '添加市场' }))
  const sourceRow = (
    await within(dialog).findByText(sourceURL, { selector: 'strong' })
  ).closest('article')
  expect(sourceRow).not.toBeNull()
  await user.click(within(sourceRow!).getByRole('button', { name: '停用' }))
  await waitFor(() =>
    expect(
      within(dialog)
        .getByText(sourceURL, { selector: 'strong' })
        .closest('article'),
    ).toHaveTextContent('已停用'),
  )
  const disabledRow = within(dialog)
    .getByText(sourceURL, { selector: 'strong' })
    .closest('article')!
  await user.click(within(disabledRow).getByRole('button', { name: '移除来源' }))
  await user.click(
    within(disabledRow).getByRole('button', { name: '确认移除来源' }),
  )
  await waitFor(() =>
    expect(within(dialog).queryByText(sourceURL)).not.toBeInTheDocument(),
  )
})

it('keeps the market source list read-only for members', async () => {
  installMarketplaceBackend({
    role: 'member',
    catalog: [official],
    marketSources: [
      {
        namespace: 'custom-market',
        url: 'https://github.com/ora-space/marketplace.git',
        branch: 'main',
        enabled: true,
        syncedAt: null,
        failure: '',
        retrieval: { mode: 'direct_https', useProxy: false },
      },
    ],
  })
  const user = userEvent.setup()
  renderSettingsPage(<PluginsPage />)
  await user.click(screen.getByRole('button', { name: '管理市场' }))
  const dialog = await screen.findByRole('dialog')
  expect(
    await within(dialog).findByRole('article', { name: '官方市场' }),
  ).toBeVisible()
  expect(
    within(dialog).getByText(
      'https://github.com/ora-space/marketplace.git',
    ),
  ).toBeVisible()
  expect(
    dialog.querySelector('form[aria-label="添加市场"]'),
  ).not.toBeInTheDocument()
  expect(
    within(dialog).queryByRole('button', { name: '添加市场' }),
  ).not.toBeInTheDocument()
  expect(
    within(dialog).queryByRole('button', { name: '同步' }),
  ).not.toBeInTheDocument()
})
