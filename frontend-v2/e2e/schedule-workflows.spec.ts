/// <reference lib="dom" />
import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

// With prepared Node assets (`task v2:node:prepare`, then V2_NODE_ASSETS_DIR) the workflow's Agent
// runs on the fixture model; without them, as in CI, there is no Agent and the run fails.
const realAgents = !!process.env.V2_NODE_ASSETS_DIR

/** Start → 起草 → 输出: one Agent that drafts from the kickoff input. */
const graph = JSON.stringify({
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
    { id: 'draft', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '起草', agentConfig: { executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt: '根据需求起草方案：{{#start.input#}}' } } },
    { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'draft', variableSelector: ['draft', 'output'] }] } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'draft' },
    { id: 'e2', source: 'draft', target: 'out' },
  ],
})

test('a scheduled workflow starts its own run of the published version and reads it back in history', async ({ page }, testInfo) => {
  test.setTimeout(realAgents ? 240_000 : 90_000)
  await page.goto('/schedules')
  await signIn(page)

  // Publish a workflow on v2-server; names are fresh so earlier runs of this spec do not interfere.
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = Date.now().toString(36)
  const workflowName = `定时起草 ${suffix}`
  const taskName = `定时工作流-${suffix}`
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name: workflowName, graph } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { workflow: { id: string } }).workflow.id
  expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
  await page.reload()
  await expect(page.getByRole('heading', { name: '定时任务', exact: true })).toBeVisible()

  // The form offers the published workflow; the task runs it in a project with a kickoff input.
  const dialog = page.getByRole('dialog')
  await page.locator('header').getByRole('button', { name: '新建任务', exact: true }).click()
  await dialog.getByRole('textbox', { name: '任务名称' }).fill(taskName)
  await dialog.getByRole('button', { name: /运行工作流/ }).click()
  await dialog.getByRole('combobox', { name: '工作流', exact: true }).selectOption({ label: `${workflowName}（v1）` })
  await dialog.getByRole('combobox', { name: '关联项目', exact: true }).selectOption({ label: 'Demo' })
  await dialog.getByRole('textbox', { name: '初始输入' }).fill('给支付模块加一个对账接口')
  await expect(dialog.getByText('保存时固定生效版本 v1；之后发布的新版本要重新保存任务才会使用。')).toBeVisible()
  await dialog.getByRole('button', { name: '创建并启用' }).click()
  await expect(dialog.getByRole('heading', { name: taskName })).toBeVisible()
  await expect(dialog.getByText(`${workflowName}（v1）`)).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('schedule-workflow-config.png'), fullPage: true })

  // 立即运行 creates and starts one workflow run; the history follows that run to its end.
  await dialog.getByRole('button', { name: '立即运行', exact: true }).click()
  await expect(dialog.getByRole('button', { name: '运行历史' })).toHaveAttribute('aria-pressed', 'true')
  if (realAgents) {
    await expect(dialog.getByText('工作流运行 · 成功')).toBeVisible({ timeout: 200_000 })
    await expect(dialog.getByText(/V2 fixture complete/)).toBeVisible()
  } else {
    // No Agent is installed: v2-server fails the run at its Agent node, and the history says so.
    await expect(dialog.getByText('工作流运行 · 失败')).toBeVisible({ timeout: 60_000 })
    await expect(dialog.getByText('工作流运行失败')).toBeVisible()
  }
  // The occurrence settles with its run and frees the task for the next trigger.
  await expect(dialog.getByRole('button', { name: new RegExp(`^${realAgents ? '成功' : '失败'}.*手动$`) })).toBeVisible({ timeout: 30_000 })
  await page.screenshot({ path: testInfo.outputPath('schedule-workflow-history.png'), fullPage: true })

  // Pause it, so no later trigger of this spec's task runs on the shared e2e database.
  await dialog.getByRole('button', { name: '暂停任务' }).click()
  await expect(dialog.getByText('已暂停')).toBeVisible()

  // The run is an ordinary run of the project: the chat page lists it under Demo.
  await page.goto('/chat')
  const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
  await expect(demo.getByRole('button', { name: new RegExp(`工作流运行 ${taskName}（${realAgents ? '成功' : '失败'}）`) })).toBeVisible()
})
