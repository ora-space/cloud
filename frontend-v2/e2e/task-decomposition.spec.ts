/// <reference lib="dom" />
import { mkdirSync } from 'node:fs'
import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

const screens = '../.tmp/screens'

function decompositionGraph() {
  return JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始', inputVariables: [
        { name: 'tasks', valueType: 'array[object]', value: [{ title: '登录页子任务', description: '由确定性输出创建' }] },
      ] } },
      { id: 'out', type: 'workflow', position: { x: 360, y: 0 }, data: { kind: 'output', title: '输出', outputs: [
        { name: 'tasks', variableSelector: ['start', 'tasks'] },
      ] } },
    ],
    edges: [{ id: 'e', type: 'workflow', source: 'start', target: 'out' }],
  })
}

function summaryGraph() {
  return JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'out', type: 'workflow', position: { x: 360, y: 0 }, data: { kind: 'output', title: '输出', outputs: [
        { name: 'summary', variableSelector: ['start', 'input'] },
      ] } },
    ],
    edges: [{ id: 'e', type: 'workflow', source: 'start', target: 'out' }],
  })
}

async function publishWorkflow(page: Page, tenantId: string, name: string, graph: string) {
  const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const workflowId = ((await created.json()) as { workflow: { id: string } }).workflow.id
  const published = await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })
  expect(published.ok()).toBeTruthy()
  return workflowId
}

/**
 * Workflows keep fixed names and are reused. A workflow with runs cannot be deleted, and deleting
 * the others races specs that have the shared dev tenant's library open, so reuse instead of
 * creating one per run keeps the tenant from piling up.
 */
async function reuseWorkflow(page: Page, tenantId: string, name: string, graph: string) {
  const list = await page.request.get(`/api/v2/tenants/${tenantId}/workflows`)
  expect(list.ok()).toBeTruthy()
  const existing = ((await list.json()) as { workflows: { id: string; name: string; publishedVersion: string | null }[] })
    .workflows.find(w => w.name === name && w.publishedVersion)
  return existing?.id ?? publishWorkflow(page, tenantId, name, graph)
}

test('decomposition picker explains blocked workflows and runs a compatible one', async ({ page }) => {
  mkdirSync(screens, { recursive: true })
  await page.goto('/collaboration?new=1')
  await signIn(page)
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const suffix = crypto.randomUUID().slice(0, 8)
  const blockedName = 'e2e 普通总结（无 tasks 输出）'
  const compatibleName = 'e2e 需求拆分（tasks 输出）'
  await reuseWorkflow(page, tenantId, blockedName, summaryGraph())
  const compatibleId = await reuseWorkflow(page, tenantId, compatibleName, decompositionGraph())

  const create = page.getByRole('dialog')
  await create.getByLabel('任务标题').fill(`账号体系 ${suffix}`)
  await create.getByRole('button', { name: '创建任务' }).click()
  await page.getByRole('button', { name: '拆分', exact: true }).click()
  const picker = page.getByRole('dialog')
  const select = picker.getByLabel('工作流')
  await expect(select).toBeEnabled()
  await select.selectOption(compatibleId)
  await expect(select).toHaveValue(compatibleId)
  // Chromium does not expose <optgroup> as an accessible group, so locate it by its label.
  const group = select.locator('optgroup[label="不可拆分（缺少 tasks 输出）"]')
  await expect(group.locator('option', { hasText: blockedName })).toBeDisabled()
  await picker.getByText(/个已发布工作流不能用于拆分/).click()
  const details = picker.locator('details')
  await expect(details.getByText('用于拆分的工作流，输出节点必须声明名为 tasks 的输出，内容是 {title, description} 的数组。', { exact: true })).toHaveCount(1)
  await expect(details.getByText(new RegExp(blockedName.replace(/[()（）]/g, '.')))).toBeVisible()
  await page.screenshot({ path: `${screens}/decomposition-picker.png`, fullPage: true })

  await picker.getByRole('button', { name: '开始拆分' }).click()
  await expect(page.getByText(new RegExp(`「${compatibleName.replace(/[()（）]/g, '.')}」拆出了 1 个子任务`))).toBeVisible({ timeout: 30_000 })
  await page.getByRole('button', { name: '展开子任务' }).click()
  await expect(page.getByRole('list', { name: '子任务' })).toContainText('登录页子任务')
})

test('workflow output editor shows the decomposition contract hint', async ({ page }) => {
  mkdirSync(screens, { recursive: true })
  await page.goto('/workflows')
  await signIn(page)
  const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
  const name = 'e2e 输出提示'
  await reuseWorkflow(page, tenantId, name, summaryGraph())
  await page.reload()
  const library = page.getByRole('complementary', { name: '工作流列表' })
  await library.getByRole('button', { name, exact: true }).click()
  const canvas = page.getByLabel('工作流画布')
  await canvas.getByText('输出', { exact: true }).click()
  await expect(page.getByText(/用于「拆分」时，输出节点需要名为 tasks 的输出/)).toBeVisible()
  await expect(page.getByText(/\{tasks: \[\{title, description\}\]\}/)).toBeVisible()
  await expect(page.getByText(/绑定到 Agent 的 structured_output\.tasks/)).toBeVisible()
  await page.screenshot({ path: `${screens}/workflow-output-hint.png`, fullPage: true })
})
