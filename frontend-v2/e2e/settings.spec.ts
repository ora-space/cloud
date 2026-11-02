import { expect, test, type Page } from '@playwright/test'
import { memberEmail, signIn } from './login.ts'

async function login(page: Page, path: string, email?: string) {
  await page.goto(path)
  // The login screen keeps the requested URL; signIn waits for the authenticated shell.
  await signIn(page, email)
  await expect(page).toHaveURL(new RegExp(path.replace(/[?]/g, '\\?') + '$'))
}
async function theme(page: Page, name: '浅色' | '深色' | '跟随系统') {
  await page.goto('/settings/theme')
  await page.getByRole('button', { name, exact: true }).click()
}
async function resetScroll(page: Page) {
  await page.evaluate(() => {
    window.scrollTo(0, 0)
    document.querySelector('main')?.scrollTo(0, 0)
  })
}
const titles = [
  ['members', '成员'],
  ['model-connections', '模型连接'],
  ['theme', '主题'],
  ['plugins', 'Plugins'],
  ['skills', 'Skills'],
] as const

test('settings subroutes, refresh, history and the existing shell remain navigable', async ({
  page,
}) => {
  await login(page, '/settings/plugins')
  await page.getByRole('link', { name: '设置', exact: true }).click()
  await expect(page).toHaveURL(/\/settings\/members$/)
  for (const [path, title] of titles) {
    await page
      .getByRole('navigation', { name: '设置导航' })
      .getByRole('link', { name: title, exact: true })
      .click()
    await expect(page).toHaveURL(new RegExp('/settings/' + path + '$'))
    await expect(
      page.getByRole('heading', { level: 1, name: title, exact: true }),
    ).toBeVisible()
    await page.reload()
    await expect(
      page.getByRole('heading', { level: 1, name: title, exact: true }),
    ).toBeVisible()
  }
  await page.goBack()
  await expect(
    page.getByRole('heading', { level: 1, name: 'Plugins', exact: true }),
  ).toBeVisible()
  await page.goForward()
  await expect(
    page.getByRole('heading', { level: 1, name: 'Skills', exact: true }),
  ).toBeVisible()
  for (const title of ['对话', '工作流', '定时任务', '仪表盘']) {
    await page
      .getByRole('navigation', { name: '主导航' })
      .getByRole('link', { name: title, exact: true })
      .click()
    await expect(
      page.getByRole('heading', { level: 1, name: title, exact: true }),
    ).toBeVisible()
  }
})

test('server-backed plugin and skill install lifecycle survives reload and a new browser context', async ({
  page,
  browser,
}) => {
  await login(page, '/settings/plugins')
  const row = page.getByRole('article', { name: 'Matt Pocock Skills' })
  await expect(row).toBeVisible()
  for (const name of ['Tavily', 'Workflow Examples', 'SkillHub']) {
    await expect(page.getByRole('article', { name })).toBeVisible()
  }
  const install = row.getByRole('button', { name: /^(安装|重试安装)$/ })
  if (await install.count()) {
    await install.click()
    await expect(row.getByText('等待节点', { exact: true })).toBeVisible()
  }
  await row.getByRole('button', { name: '移除' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '移除' })
    .click()
  await expect(row.getByText('未安装', { exact: true })).toBeVisible()
  await page.goto('/settings/skills')
  const skill = page.getByRole('article', { name: 'Matt Pocock Skills' })
  await expect(skill.getByText('来源插件未安装')).toBeVisible()
  await page.reload()
  await expect(skill.getByText('来源插件未安装')).toBeVisible()
  await skill
    .getByRole('button', { name: '查看技能 Matt Pocock Skills' })
    .click()
  await expect(
    page.getByRole('dialog').getByRole('button', { name: '编辑技能' }),
  ).toHaveCount(0)
  await page.getByRole('link', { name: /official\/mattpocock\.skills/ }).click()
  await page
    .getByRole('dialog')
    .getByRole('button', { name: '安装' })
    .click()
  await expect(
    page.getByRole('dialog').getByText('等待节点', { exact: true }),
  ).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await page.goto('/settings/skills')
  await expect(skill.getByText('等待节点', { exact: true })).toBeVisible()
  await page.reload()
  await expect(skill.getByText('等待节点', { exact: true })).toBeVisible()

  // A separate browser context has no local settings data and still reads the same server state.
  const other = await browser.newContext({ baseURL: new URL(page.url()).origin })
  const otherPage = await other.newPage()
  await login(otherPage, '/settings/skills')
  await expect(
    otherPage
      .getByRole('article', { name: 'Matt Pocock Skills' })
      .getByText('等待节点', {
        exact: true,
      }),
  ).toBeVisible()
  await other.close()

  // Skill removal is the same package selection, not a second local-only state machine.
  await skill.getByRole('button', { name: '移除技能' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '移除技能' })
    .click()
  await expect(skill.getByText('来源插件未安装')).toBeVisible()
  await page.reload()
  await expect(skill.getByText('来源插件未安装')).toBeVisible()
  await skill.getByRole('button', { name: '安装技能' }).click()
  await expect(skill.getByText('等待节点', { exact: true })).toBeVisible()

  await page.goto('/settings/plugins')
  const tavily = page.getByRole('article', { name: 'Tavily' })
  await tavily.getByRole('button', { name: '安装' }).click()
  await expect(tavily.getByText('已安装（演示）', { exact: true })).toBeVisible()
  await page.reload()
  await expect(tavily.getByText('已安装（演示）', { exact: true })).toBeVisible()
  await tavily.getByRole('button', { name: '移除' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '移除' })
    .click()
  await expect(tavily.getByText('未安装', { exact: true })).toBeVisible()

  const hooks = page.getByRole('article', { name: '示例 Hooks' })
  await expect(page.getByRole('heading', { name: 'Hooks' })).toBeVisible()
  await hooks.getByRole('button', { name: '安装' }).click()
  await expect(hooks.getByText('已安装（演示）', { exact: true })).toBeVisible()
  await page.reload()
  await expect(hooks.getByText('已安装（演示）', { exact: true })).toBeVisible()
})

test('Agent cards install and remove while preinstalled Agents stay read-only', async ({
  page,
  browser,
}) => {
  test.setTimeout(120_000)
  await login(page, '/settings/plugins')
  const opencode = page.getByRole('article', { name: 'OpenCode' })
  const codex = page.getByRole('article', { name: 'Codex' })
  await expect(opencode.getByText('运行时预装')).toBeVisible()
  await expect(opencode.getByRole('button', { name: '移除' })).toHaveCount(0)
  await expect(
    page
      .getByRole('article', { name: 'Claude Code' })
      .getByText('运行时预装'),
  ).toBeVisible()

  const remove = codex.getByRole('button', { name: '移除' })
  if (await remove.count()) {
    await remove.click()
    await page
      .getByRole('alertdialog')
      .getByRole('button', { name: '移除' })
      .click()
    await expect(codex.getByText('未安装', { exact: true })).toBeVisible()
  }
  await codex.getByRole('button', { name: '安装' }).click()
  await expect(codex.getByText('已安装', { exact: true })).toBeVisible()
  await page.reload()
  await expect(codex.getByText('已安装', { exact: true })).toBeVisible()

  const other = await browser.newContext({ baseURL: new URL(page.url()).origin })
  const otherPage = await other.newPage()
  await login(otherPage, '/settings/plugins')
  await expect(
    otherPage
      .getByRole('article', { name: 'Codex' })
      .getByText('已安装', { exact: true }),
  ).toBeVisible()
  await other.close()

  await codex.getByRole('button', { name: '移除' }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '移除' })
    .click()
  await expect(codex.getByText('未安装', { exact: true })).toBeVisible()
})

test('owner manages a space market while the official catalog stays shared', async ({
  page,
}) => {
  test.setTimeout(180_000)
  await login(page, '/settings/plugins')
  for (const agent of ['OpenCode', 'Claude Code', 'Codex']) {
    await expect(page.getByRole('article', { name: agent })).toBeVisible()
  }
  await page.getByRole('button', { name: '管理市场' }).click()
  const dialog = page.getByRole('dialog')
  const official = dialog.getByRole('article', { name: '官方市场' })
  await expect(official).toContainText(
    'https://github.com/ora-space/marketplace',
  )
  await expect(official.getByRole('button', { name: '同步' })).toBeEnabled()

  const customURL = 'https://github.com/ora-space/marketplace.git'
  let custom = dialog.getByRole('article').filter({ hasText: customURL })
  if (await custom.count()) {
    await custom.getByRole('button', { name: '移除来源' }).click()
    await custom.getByRole('button', { name: '确认移除来源' }).click()
    await expect(custom).toHaveCount(0)
  }
  await dialog
    .getByLabel('HTTPS 仓库地址', { exact: true })
    .fill(customURL)
  await dialog.getByRole('button', { name: '添加市场', exact: true }).click()
  custom = dialog.getByRole('article').filter({ hasText: customURL })
  await expect(custom).toBeVisible()
  const synced = page.waitForResponse(
    (response) =>
      response.request().method() === 'POST' &&
      /\/plugins\/sources\/[^/]+\/sync$/.test(response.url()),
  )
  await custom.getByRole('button', { name: '同步', exact: true }).click()
  expect((await synced).status()).toBe(204)
  await page.getByRole('button', { name: '关闭详情' }).click()
  await expect(
    page.getByRole('article', { name: 'Matt Pocock Skills' }),
  ).toHaveCount(2)

  await page.getByRole('button', { name: '管理市场' }).click()
  custom = page
    .getByRole('dialog')
    .getByRole('article')
    .filter({ hasText: customURL })
  await custom.getByRole('button', { name: '停用', exact: true }).click()
  await expect(custom).toContainText('已停用')
  await page.getByRole('button', { name: '关闭详情' }).click()
  await expect(
    page.getByRole('article', { name: 'Matt Pocock Skills' }),
  ).toHaveCount(1)

  await page.getByRole('button', { name: '管理市场' }).click()
  custom = page
    .getByRole('dialog')
    .getByRole('article')
    .filter({ hasText: customURL })
  await custom.getByRole('button', { name: '移除来源' }).click()
  await custom.getByRole('button', { name: '确认移除来源' }).click()
  await expect(custom).toHaveCount(0)
})

test('member can inspect market sources but cannot manage them', async ({
  page,
}) => {
  await login(page, '/settings/plugins', memberEmail)
  await page.getByRole('button', { name: '管理市场' }).click()
  const dialog = page.getByRole('dialog')
  await expect(
    dialog.getByRole('article', { name: '官方市场' }),
  ).toBeVisible()
  await expect(
    dialog.getByLabel('HTTPS 仓库地址', { exact: true }),
  ).toHaveCount(0)
  await expect(
    dialog.getByRole('button', { name: '添加市场', exact: true }),
  ).toHaveCount(0)
  await expect(
    dialog.getByRole('button', { name: '同步', exact: true }),
  ).toHaveCount(0)
})

test('corrupt local data shows a recoverable error and requires reset confirmation', async ({
  page,
}) => {
  await login(page, '/settings/skills')
  await expect(
    page.getByRole('article', { name: 'change-summary' }),
  ).toBeVisible()
  await page.evaluate(() => {
    const key = Object.keys(localStorage).find((k) =>
      k.startsWith('ora-v2-settings-demo:'),
    )!
    localStorage.setItem(key, '{broken')
  })
  await page.reload()
  await expect(page.getByRole('alert')).toContainText('演示数据无法读取')
  await page.getByRole('button', { name: '重试', exact: true }).click()
  await expect(page.getByRole('alert')).toContainText('演示数据无法读取')
  await page.getByRole('button', { name: '重置演示', exact: true }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '取消' })
    .click()
  await expect(page.getByRole('article')).toHaveCount(0)
  await page.getByRole('button', { name: '重置演示', exact: true }).click()
  await page
    .getByRole('alertdialog')
    .getByRole('button', { name: '重置演示', exact: true })
    .click()
  await expect(
    page.getByRole('article', { name: 'change-summary' }),
  ).toBeVisible()
})

test('theme survives reload and follows system changes across existing pages', async ({
  page,
}) => {
  await page.emulateMedia({ colorScheme: 'dark' })
  await login(page, '/settings/theme')
  await expect(page.locator('html')).not.toHaveClass(/dark/)
  await expect(page.getByRole('button', { name: '浅色', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await page.reload()
  await expect(page.getByRole('heading', { name: '主题', exact: true })).toBeVisible()
  await expect(page.locator('html')).not.toHaveClass(/dark/)
  await page.getByRole('button', { name: '深色', exact: true }).click()
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.reload()
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.goto('/schedules')
  await expect(
    page.getByRole('heading', { name: '定时任务', exact: true }),
  ).toBeVisible()
  await expect(page.locator('.schedules-feature')).toBeVisible()
  const darkSurface = await page
    .locator('.schedules-feature')
    .evaluate((el) => getComputedStyle(el).backgroundColor)
  await theme(page, '浅色')
  await page.goto('/schedules')
  await expect(page.locator('.schedules-feature')).toBeVisible()
  const lightSurface = await page
    .locator('.schedules-feature')
    .evaluate((el) => getComputedStyle(el).backgroundColor)
  expect(darkSurface).not.toBe(lightSurface)
  await theme(page, '跟随系统')
  await page.reload()
  await expect(page.getByRole('heading', { name: '主题', exact: true })).toBeVisible()
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.emulateMedia({ colorScheme: 'light' })
  await expect(page.locator('html')).not.toHaveClass(/dark/)
})

test('five desktop pages and keyboard-accessible details', async ({
  page,
}, info) => {
  const errors: string[] = []
  page.on('pageerror', (error) => errors.push(error.message))
  await page.setViewportSize({ width: 1440, height: 1000 })
  await login(page, '/settings/plugins')
  await theme(page, '浅色')
  for (const [path, title] of titles) {
    await page.goto('/settings/' + path)
    await expect(
      page.getByRole('heading', { level: 1, name: title, exact: true }),
    ).toBeVisible()
    if (path !== 'theme')
      await expect(page.getByText('正在加载…', { exact: true })).toHaveCount(0)
    if (path === 'plugins') {
      for (const name of ['Tavily', '示例 Hooks']) {
        const plugin = page.getByRole('article', { name })
        const install = plugin.getByRole('button', { name: '安装' })
        if (await install.count()) await install.click()
      }
      await expect(
        page.getByRole('article', { name: 'Tavily' }).getByText(
          '已安装（演示）',
          { exact: true },
        ),
      ).toBeVisible()
    }
    await resetScroll(page)
    await page.screenshot({
      path: info.outputPath(path + '-light.png'),
      fullPage: true,
    })
  }
  await page.goto('/settings/plugins')
  const opener = page.getByRole('button', {
    name: '查看插件 Matt Pocock Skills',
  })
  await opener.focus()
  await page.keyboard.press('Enter')
  await expect(page.getByRole('dialog')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(opener).toBeFocused()
  expect(errors).toEqual([])
})

test('dark plugin and skill screenshots', async ({ page }, info) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await login(page, '/settings/theme')
  await theme(page, '深色')
  for (const path of ['plugins', 'skills']) {
    await page.goto('/settings/' + path)
    await expect(page.getByRole('article').first()).toBeVisible()
    await resetScroll(page)
    await page.screenshot({
      path: info.outputPath(path + '-dark.png'),
      fullPage: true,
    })
  }
})

test('Agent plugin screenshots', async ({ page }, info) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await login(page, '/settings/plugins')
  await theme(page, '浅色')
  await page.goto('/settings/plugins')
  await expect(page.getByRole('heading', { name: 'Agent' })).toBeVisible()
  await page.screenshot({
    path: info.outputPath('plugins-agent-light-1440.png'),
  })

  await theme(page, '深色')
  await page.goto('/settings/plugins')
  await expect(page.getByRole('heading', { name: 'Agent' })).toBeVisible()
  await page.screenshot({
    path: info.outputPath('plugins-agent-dark-1440.png'),
  })

  await page.setViewportSize({ width: 375, height: 900 })
  await theme(page, '浅色')
  await page.goto('/settings/plugins')
  await expect(page.getByRole('heading', { name: 'Agent' })).toBeVisible()
  await page.screenshot({
    path: info.outputPath('plugins-agent-light-375.png'),
    fullPage: true,
  })
})

for (const width of [768, 375]) {
  test(`five settings pages at ${width}px keep the active link visible`, async ({
    page,
  }, info) => {
    await page.setViewportSize({ width: 1440, height: 900 })
    await page.emulateMedia({ colorScheme: 'light' })
    await login(page, '/settings/skills')
    await page.setViewportSize({ width, height: 900 })
    await expect(
      page
        .getByRole('navigation', { name: '设置导航' })
        .getByRole('link', { name: 'Skills', exact: true }),
    ).toBeInViewport({ ratio: 1 })
    for (const [path, title] of titles) {
      await page.goto('/settings/' + path)
      await expect(
        page.getByRole('heading', { level: 1, name: title, exact: true }),
      ).toBeVisible()
      expect(
        await page.evaluate(
          () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
      ).toBe(true)
      const nav = page.getByRole('navigation', { name: '设置导航' })
      const active = nav.getByRole('link', { name: title, exact: true })
      await expect(active).toBeInViewport({ ratio: 1 })
      if (width === 375 && ['plugins', 'skills'].includes(path)) {
        await expect(page.getByRole('article').first()).toBeVisible()
        await resetScroll(page)
        await page.screenshot({
          path: info.outputPath(path + '-mobile.png'),
          fullPage: true,
        })
      }
    }
  })
}

test('the roster comes from the server and a member sees no invitation entry', async ({
  page,
}) => {
  await login(page, '/settings/members')
  // The preset owner is the only guaranteed row; the badge names the real role.
  const row = page.getByRole('row', { name: /演示 Owner/ })
  await expect(row.getByText('owner', { exact: true })).toBeVisible()
  await expect(page.getByRole('form', { name: '邀请成员' })).toBeVisible()

  // A preset member reads the same roster and is told why there is nothing to invite with.
  await page.getByRole('button', { name: '退出登录' }).click()
  await login(page, '/settings/members', memberEmail)
  await expect(page.getByRole('img', { name: '爱丽丝' })).toBeVisible()
  await expect(page.getByText(/邀请入口只向 owner 开放/)).toBeVisible()
  await expect(page.getByRole('form', { name: '邀请成员' })).toHaveCount(0)
  await expect(page.getByRole('row', { name: /演示 Owner/ })).toBeVisible()
})

test('model metadata and simulated credentials never send model requests', async ({
  page,
}) => {
  const modelRequests: string[] = []
  page.on('request', (request) => {
    if (/model\.example\.com|\/api\/.*model-/.test(request.url()))
      modelRequests.push(request.url())
  })
  await login(page, '/settings/model-connections')
  await page.getByRole('button', { name: '新建连接' }).click()
  await page.getByLabel('连接名称', { exact: true }).fill('浏览器验收连接')
  await page
    .getByLabel('服务地址', { exact: true })
    .fill('https://model.example.com/v1')
  await page.getByLabel('模型标识', { exact: true }).fill('org/model')
  await page.getByLabel('显示名称', { exact: true }).fill('验收模型')
  await page.getByRole('button', { name: '保存连接', exact: true }).click()
  const row = page.getByRole('article', { name: '浏览器验收连接' })
  await row.getByRole('button', { name: '模拟配置凭据' }).click()
  await expect(row.getByText('演示凭据已配置', { exact: true })).toBeVisible()
  await page
    .getByLabel('默认模型', { exact: true })
    .selectOption({ label: '浏览器验收连接 / 验收模型' })
  await page.getByRole('button', { name: '保存默认模型' }).click()
  await expect(
    page.getByRole('button', { name: '保存默认模型' }),
  ).toBeDisabled()
  await page.reload()
  await expect(page.getByLabel('默认模型', { exact: true })).toHaveValue(
    /org\/model/,
  )
  expect(modelRequests).toEqual([])
  await expect(page.locator('input[type="password"]')).toHaveCount(0)
})
