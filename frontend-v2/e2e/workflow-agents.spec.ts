import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'
type Executor = { agentId: string; agentName: string; modelId: string; modelName: string }

test('workflow agent: an empty directory retains desktop defaults for a new node', async ({ page }) => {
  await page.route('**/api/v2/tenants/*/chat/executors', (route) => route.fulfill({ json: { items: [] } }))
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}/workflows`
  const name = `空执行器目录 ${Date.now()}`
  const created = await page.request.post(base, { data: { name, graph: null } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.getByRole('toolbar', { name: '添加工作流节点' }).getByRole('button', { name: 'Agent', exact: true }).click()
  await expect(page.getByText('请选择当前可用的 Agent 和模型。')).toBeVisible()
  await expect.poll(async () => {
    const draft = (await (await page.request.get(`${base}/${wid}/draft`)).json()).snapshot
    return JSON.parse(draft.graph).nodes.find((n: { data: { kind: string } }) => n.data.kind === 'agent')?.data.agentConfig.executor
  }).toEqual({ agentCli: 'official/ora-space.codeagentcli', modelId: 'gpt-5' })
})

test('workflow agent: replace a legacy demo choice, persist real IDs, and execute the selected binding', async ({ page }, testInfo) => {
  test.setTimeout(180_000)
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}`
  const executors: Executor[] = (await (await page.request.get(`${base}/chat/executors`)).json()).items
  test.skip(executors.length === 0, 'prepared Node assets are required for real Agent selection')
  const chosen = executors.find((e) => e.agentName === 'Claude Code') ?? executors[0]!
  const name = `真实执行器 ${Date.now()}`
  const legacy = { agentCli: 'ora/claude-code', modelId: 'claude-sonnet-5-5' }
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '真实执行', agentConfig: { executor: legacy, prompt: 'Reply with a short greeting.', skills: [], mcps: [] } } },
      { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'answer', variableSelector: ['agent', 'output'] }] } },
    ],
    edges: [{ id: 'a', source: 'start', target: 'agent' }, { id: 'b', source: 'agent', target: 'out' }],
  })
  const created = await page.request.post(`${base}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.locator('.react-flow__node[data-id="agent"]').click()
  await expect(page.getByText('请选择当前可用的 Agent 和模型。')).toBeVisible()
  const draft = async () => (await (await page.request.get(`${base}/workflows/${wid}/draft`)).json()).snapshot
  const savedExecutor = async () => JSON.parse((await draft()).graph).nodes.find((n: { id: string }) => n.id === 'agent').data.agentConfig.executor
  expect(await savedExecutor()).toEqual(legacy)
  await page.getByLabel('Agent 模型', { exact: true }).click()
  await expect(page.getByRole('option', { name: 'Codex', exact: true })).toHaveCount(0)
  for (const agentName of new Set(executors.map((e) => e.agentName))) {
    await expect(page.getByRole('option', { name: agentName, exact: true })).toBeVisible()
  }
  await page.getByRole('option', { name: chosen.agentName, exact: true }).click()
  await page.getByRole('option', { name: chosen.modelName, exact: true }).click()
  const expected = { agentCli: chosen.agentId, modelId: chosen.modelId }
  await expect.poll(savedExecutor).toEqual(expected)
  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.locator('.react-flow__node[data-id="agent"]').click()
  await expect(page.getByLabel('Agent 模型', { exact: true })).toContainText(chosen.modelName)
  await expect(page.getByText('请选择当前可用的 Agent 和模型。')).toBeHidden()
  expect(await savedExecutor()).toEqual(expected)
  await page.getByLabel('Agent 模型', { exact: true }).click()
  await expect(page.getByRole('option', { name: chosen.modelName, exact: true })).toBeVisible()
  await page.screenshot({ path: testInfo.outputPath('real-agent-model-picker.png'), animations: 'disabled' })
  await page.keyboard.press('Escape')

  expect((await page.request.post(`${base}/workflows/${wid}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
  const project = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items[0]
  const createdRun = await page.request.post(`${base}/workflows/${wid}/runs`, { data: { projectId: project.id, name, input: 'hello' } })
  expect(createdRun.status()).toBe(201)
  const rid = (await createdRun.json()).run.id
  expect((await page.request.post(`${base}/workflow-runs/${rid}/start`)).ok()).toBe(true)
  const detail = async () => (await page.request.get(`${base}/workflow-runs/${rid}`)).json()
  await expect.poll(async () => (await detail()).run.status, { timeout: 120_000 }).toBe('succeeded')
  const node = (await detail()).nodes.find((n: { nodeId: string }) => n.nodeId === 'agent')
  const payload = JSON.parse(node.payload)
  expect(payload.executor.agentId).toBe(chosen.agentId)
  expect(payload.model.id).toBe(chosen.modelId)
  expect(payload.model.fallback).toBe(false)
  expect(node.sessionId).toBeTruthy()
})

test('workflow agent: roles saved in settings appear in the role picker', async ({ page }) => {
  await page.goto('/settings/roles')
  await signIn(page)
  const roleName = `发布审查 ${Date.now()}`
  await page.getByRole('button', { name: '新建角色' }).click()
  await page.getByLabel('标题').fill(roleName)
  await page.getByLabel('描述').fill('由工作流 Agent 节点选择')
  await page.getByLabel('内容（Markdown）').fill('# 发布审查\n\n核对发布说明。')
  await page.getByRole('button', { name: '保存角色' }).click()
  await expect(page.getByRole('article', { name: roleName })).toBeVisible()

  await page.goto('/workflows')
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const name = `角色选择 ${Date.now()}`
  const created = await page.request.post(`/api/v2/tenants/${tid}/workflows`, {
    data: { name, graph: null },
  })
  expect(created.status()).toBe(201)
  await page.reload()
  await page
    .getByRole('complementary', { name: '工作流列表' })
    .getByRole('button', { name, exact: true })
    .click()
  await page
    .getByRole('toolbar', { name: '添加工作流节点' })
    .getByRole('button', { name: 'Agent', exact: true })
    .click()
  await page.getByLabel('角色', { exact: true }).click()
  await expect(page.getByRole('option', { name: roleName })).toBeVisible()
})
