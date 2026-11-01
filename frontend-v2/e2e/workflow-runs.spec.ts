import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

// With prepared Node assets (`task v2:node:prepare`, then V2_NODE_ASSETS_DIR) v2-server runs real
// Agents on the fixture model; without them, as in CI, it has no Agent to run.
const realAgents = !!process.env.V2_NODE_ASSETS_DIR

/** Start → 起草 → 审阅 → 输出: 审阅 reads 起草's answer through the variable pool. */
const chain = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'draft', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '起草', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '根据需求起草方案：{{#start.input#}}' } } },
    { id: 'review', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'agent', title: '审阅', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '审阅上一步的方案：{{#draft.output#}}' } } },
    { id: 'out', type: 'workflow', position: { x: 960, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'draft', variableSelector: ['draft', 'output'] }, { name: 'review', variableSelector: ['review', 'output'] }] } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'draft' },
    { id: 'e2', source: 'draft', target: 'review' },
    { id: 'e3', source: 'review', target: 'out' },
  ],
})

/**
 * Screenshots wait for the theater's enter and expand animations, or they catch a half-open card.
 * Endless ones (status pulses) and scroll-driven ones (scroll fades) never settle and are skipped.
 */
async function screenshot(page: Page, path: string) {
  await page.waitForFunction(() =>
    document
      .getAnimations()
      .filter((a) => a.timeline === document.timeline && a.effect?.getComputedTiming().endTime !== Infinity)
      .every((a) => a.playState !== 'running'),
  )
  await page.screenshot({ path })
}

test('workflow runs: v2-server executes a published workflow started from chat', async ({ page }, testInfo) => {
  test.setTimeout(180_000)
  await page.goto('/chat')
  await signIn(page)

  // Publish the chain on v2-server; the workflow and its run take names no earlier run of this spec used.
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const name = `两段 Agent ${suffix}`
  const runName = `对账接口方案 ${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph: chain } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { draft: { workflowId: string } }).draft.workflowId
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  // A project's + offers the published workflows; the run opens pending in the main area.
  await page.reload()
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await demo.getByRole('button', { name: '在 Demo 新建对话' }).click()
  await page.getByRole('menuitem', { name: '运行工作流' }).hover()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const dialog = page.getByRole('dialog', { name: '运行工作流' })
  await dialog.getByLabel('运行名称').fill(runName)
  await dialog.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()
  await expect(pane.getByText('演示运行')).toBeHidden()
  const row = (status: string) => demo.getByRole('button', { name: `工作流运行 ${runName}（${status}）` })
  await expect(row('未运行')).toBeVisible()

  // Start sends the kickoff input to v2-server, which executes the run; the page follows it.
  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  await start.getByLabel('初始 Prompt').fill('给支付模块加一个对账接口')
  await start.getByRole('button', { name: '启动' }).click()
  await expect(pane.getByRole('button', { name: '舞台', exact: true })).toHaveAttribute('aria-pressed', 'true')

  if (!realAgents) {
    // No Agent is installed, so the first Agent node fails and the run with it, as v2-server reports.
    await expect(row('失败')).toBeVisible({ timeout: 30_000 })
    await pane.getByRole('button', { name: '全图', exact: true }).click()
    await pane.getByLabel('工作流全图俯瞰').getByText('起草', { exact: true }).click()
    await pane.getByRole('button', { name: '打开节点详情' }).click()
    await expect(pane.getByText(/not available in v2/).filter({ visible: true }).first()).toBeVisible()
    return
  }

  await expect(row('成功')).toBeVisible({ timeout: 150_000 })
  await expect(pane.getByText('运行完成')).toBeVisible()
  await screenshot(page, testInfo.outputPath('1-result.png'))

  // 审阅 was asked with 起草's answer filled in, and answered in turn.
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await screenshot(page, testInfo.outputPath('2-overview.png'))
  await pane.getByLabel('工作流全图俯瞰').getByText('审阅', { exact: true }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect(pane.getByText(/审阅上一步的方案：V2 fixture complete\./)).toBeVisible()
  await screenshot(page, testInfo.outputPath('3-review-session.png'))
})
