import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('workflow runs: start a published workflow from chat and follow it to the result', async ({ page }) => {
  // The demo engine takes 1.5 s a node, so the sample needs about ten seconds to finish.
  test.setTimeout(90_000)
  // v2-server has no project or execution runtime wired yet, so the chat page is given one project
  // and executor to hang the run on; the run itself plays on the in-browser demo engine.
  await page.route('**/api/v2/tenants/*/projects?*', (route) =>
    route.fulfill({ json: { items: [{ id: '00000000-0000-4000-8000-0000000000c1', tenantId: '00000000-0000-4000-8000-0000000000aa', ownerUserId: '00000000-0000-4000-8000-0000000000bb', name: 'ora-space/cloud', repositoryUrl: 'https://github.com/ora-space/cloud', defaultBranch: 'main', commitId: '0'.repeat(40), createdAt: '2026-10-30T09:00:00Z' }], nextCursor: '' } }),
  )
  await page.route('**/api/v2/tenants/*/chat/executors', (route) =>
    route.fulfill({ json: { items: [{ id: 'oc-fixture', name: 'OpenCode · Fixture', agentName: 'OpenCode', modelName: 'Fixture' }] } }),
  )
  // One conversation in the project, not started yet, for a session to take the main area back.
  await page.route(/\/api\/v2\/tenants\/[^/]+\/chat\/conversations(\?.*)?$/, (route) =>
    route.request().method() === 'GET'
      ? route.fulfill({ json: { items: [{ id: '00000000-0000-4000-8000-0000000000d1', projectId: '00000000-0000-4000-8000-0000000000c1', executorId: 'oc-fixture', title: '梳理 v2 接口缺口', createdAt: '2026-10-30T09:00:00Z', updatedAt: '2026-10-30T09:00:00Z', session: null }], nextCursor: '' } })
      : route.fallback(),
  )
  // The review sample comes from v2-server's library. Publishing it needs the workflow kernel that
  // v2-server does not wire in yet, so its stored draft stands in for the published v1 below.
  await page.goto('/workflows')
  await signIn(page)
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name: '添加样例工作流' }).click()
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const workflows = `/api/v2/tenants/${tenantId}/workflows`
  let review: { id: string } | undefined
  await expect(async () => {
    const list = (await (await page.request.get(workflows)).json()) as { workflows: { id: string; name: string }[] }
    review = list.workflows.find((w) => w.name === '示例：代码审查')
    expect(review).toBeDefined()
  }).toPass()
  const detail = (await (await page.request.get(`${workflows}/${review!.id}`)).json()) as {
    workflow: Record<string, unknown>
    draft: Record<string, unknown>
  }
  const published = { ...detail.draft, id: '00000000-0000-4000-8000-0000000000e1', version: 'v1', updatedAt: null }
  await page.route(new RegExp(`${workflows}$`), async (route) => {
    const list = (await (await route.fetch()).json()) as { workflows: { id: string }[] }
    await route.fulfill({ json: { workflows: list.workflows.map((w) => (w.id === review!.id ? { ...w, publishedVersion: 'v1' } : w)) } })
  })
  await page.route(`**${workflows}/${review!.id}`, (route) =>
    route.fulfill({ json: { ...detail, workflow: { ...detail.workflow, publishedSnapshotId: published.id }, published } }),
  )

  await page.goto('/chat')
  const cloud = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region').filter({ hasText: 'ora-space/cloud' })

  // A project's + offers the published workflows; the run opens pending in the main area.
  await cloud.getByRole('button', { name: '在 ora-space/cloud 新建对话' }).click()
  await page.getByRole('menuitem', { name: '运行工作流' }).hover()
  await page.getByRole('menuitem', { name: /示例：代码审查/ }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill('审查 e2e')
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: '审查 e2e' })).toBeVisible()
  await expect(pane.getByText('演示运行')).toBeVisible()
  await expect(cloud.getByRole('button', { name: '工作流运行 审查 e2e（未运行）' })).toBeVisible()

  // Start asks for the run's input, then the Theater plays the nodes one after another to the result act.
  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  await start.getByLabel('初始 Prompt').fill('审查 cloud#210 的改动')
  await start.getByRole('button', { name: '启动' }).click()
  await expect(pane.getByRole('button', { name: '舞台', exact: true })).toHaveAttribute('aria-pressed', 'true')
  await expect(cloud.getByRole('button', { name: '工作流运行 审查 e2e（运行中）' })).toBeVisible()
  await expect(cloud.getByRole('button', { name: '工作流运行 审查 e2e（成功）' })).toBeVisible({ timeout: 60_000 })
  await expect(pane.getByText('运行完成')).toBeVisible()

  // Overview shows the whole graph; a node opens its act, which keeps what the Agent was sent and answered.
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await pane.getByLabel('工作流全图俯瞰').getByText('Agent：审查改动', { exact: true }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect(pane.getByRole('heading', { name: 'Mock 结论' })).toBeVisible()

  // A session takes the main area back; the run stays in the list.
  await cloud.getByRole('button', { name: '梳理 v2 接口缺口', exact: true }).click()
  await expect(pane).toBeHidden()
  await expect(cloud.getByRole('button', { name: '工作流运行 审查 e2e（成功）' })).toBeVisible()
})
