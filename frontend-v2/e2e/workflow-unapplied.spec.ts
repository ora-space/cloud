import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

// What the editor and the start dialog say a run skips comes from v2-server's analysis of the
// graph, so these specs drive the real server: the frontend keeps no list of its own.

test('the Agent inspector marks the settings v2-server does not apply', async ({ page }, testInfo) => {
  test.setTimeout(120_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const name = `暂不生效 ${Date.now()}`
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'agent', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'agent', title: '审阅', agentConfig: {
        executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' },
        prompt: '审阅改动',
        roleId: 'reviewer',
        skills: [],
        mcps: [{ mcpId: 'official/ora-space.tavily-search', enabled: true }],
        retry: { enabled: true, maxRetries: 2, initialDelaySeconds: 10 },
      } } },
      { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出', outputs: [{ name: 'answer', variableSelector: ['agent', 'output'] }] } },
    ],
    edges: [{ id: 'a', source: 'start', target: 'agent' }, { id: 'b', source: 'agent', target: 'out' }],
  })
  const created = await page.request.post(`/api/v2/tenants/${tid}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  // Read what the server reports for this graph, so the hints follow it as settings start applying.
  const analysis = (await (await page.request.post(`/api/v2/tenants/${tid}/workflows/analyze`, { data: { graph } })).json()) as {
    unappliedSettings: { nodeId: string; settings: string[] }[]
  }
  const unapplied = analysis.unappliedSettings.find((entry) => entry.nodeId === 'agent')?.settings ?? []

  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await page.locator('.react-flow__node[data-id="agent"]').click()
  const inspector = page.locator('[data-workflow-inspector]')
  for (const setting of ['role', 'mcp', 'retry']) {
    const hint = inspector.getByTestId(`workflow-unapplied-${setting}`)
    if (unapplied.includes(setting)) await expect(hint).toContainText('暂不生效')
    else await expect(hint).toHaveCount(0)
  }
  expect(unapplied.length, 'this graph should still configure something v2 does not apply').toBeGreaterThan(0)
  await inspector.getByTestId(`workflow-unapplied-${unapplied[0]}`).scrollIntoViewIfNeeded()
  await page.screenshot({ path: testInfo.outputPath('inspector-unapplied.png'), animations: 'disabled' })
  if (unapplied.includes('retry')) {
    await inspector.getByTestId('workflow-unapplied-retry').scrollIntoViewIfNeeded()
    await page.screenshot({ path: testInfo.outputPath('inspector-retry.png'), animations: 'disabled' })
  }

  // Clearing the role makes the server stop listing it, and the hint goes with it.
  if (unapplied.includes('role')) {
    await inspector.getByRole('button', { name: '角色', exact: true }).click()
    await page.getByRole('option', { name: '无角色', exact: true }).click()
    await expect(inspector.getByTestId('workflow-unapplied-role')).toHaveCount(0)
  }
})

test('the retry hint follows the server for every retry policy form', async ({ page }) => {
  test.setTimeout(120_000)
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/workflows')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const name = `重试策略 ${Date.now()}`
  // Absent (desktop's default policy), switched off, no budget, and switched on.
  const policies: Record<string, unknown> = {
    'retry-absent': undefined,
    'retry-off': { enabled: false, maxRetries: 2, initialDelaySeconds: 10 },
    'retry-zero': { enabled: true, maxRetries: 0, initialDelaySeconds: 10 },
    'retry-on': { enabled: true, maxRetries: 2, initialDelaySeconds: 10 },
  }
  const ids = Object.keys(policies)
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      ...ids.map((id, i) => ({ id, type: 'workflow', position: { x: 320, y: i * 220 }, data: { kind: 'agent', title: id, agentConfig: {
        executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' },
        prompt: id,
        skills: [],
        mcps: [],
        ...(policies[id] === undefined ? {} : { retry: policies[id] }),
      } } })),
    ],
    edges: ids.map((id) => ({ id: `e-${id}`, source: 'start', target: id })),
  })
  expect((await page.request.post(`/api/v2/tenants/${tid}/workflows`, { data: { name, graph } })).status()).toBe(201)
  const analysis = (await (await page.request.post(`/api/v2/tenants/${tid}/workflows/analyze`, { data: { graph } })).json()) as {
    unappliedSettings: { nodeId: string; settings: string[] }[]
  }
  const listed = (id: string) => analysis.unappliedSettings.find((entry) => entry.nodeId === id)?.settings.includes('retry') ?? false
  expect(ids.some(listed) && !ids.every(listed), 'the forms should differ in whether the server lists retry').toBe(true)

  await page.reload()
  await page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  const inspector = page.locator('[data-workflow-inspector]')
  for (const id of ids) {
    await page.locator(`.react-flow__node[data-id="${id}"]`).click()
    await expect(inspector.getByText(id, { exact: true }).first()).toBeVisible()
    const hint = inspector.getByTestId('workflow-unapplied-retry')
    if (listed(id)) await expect(hint, id).toContainText('暂不生效')
    else await expect(hint, id).toHaveCount(0)
  }
})

test('the start dialog waits for analysis and names reachable prompt and tool nodes, not spares', async ({ page }, testInfo) => {
  test.setTimeout(120_000)
  let releaseAnalysis!: () => void
  const analysisGate = new Promise<void>((resolve) => { releaseAnalysis = resolve })
  await page.route('**/workflows/analyze', async (route) => {
    await analysisGate
    await route.continue()
  })
  await page.goto('/chat')
  await signIn(page)
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const suffix = Date.now().toString(36)
  const name = `不支持节点 ${suffix}`
  const runName = `不支持节点运行 ${suffix}`
  const graph = JSON.stringify({
    nodes: [
      { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始' } },
      { id: 'polish', type: 'workflow', position: { x: 320, y: 0 }, data: { kind: 'prompt', title: '润色提示词' } },
      { id: 'out', type: 'workflow', position: { x: 640, y: 0 }, data: { kind: 'output', title: '输出', outputs: [] } },
      { id: 'lookup', type: 'workflow', position: { x: 320, y: 120 }, data: { kind: 'tool', title: '检索工具' } },
      { id: 'spare', type: 'workflow', position: { x: 320, y: 240 }, data: { kind: 'tool', title: '备用工具' } },
    ],
    edges: [{ id: 'c', source: 'start', target: 'lookup' }, { id: 'a', source: 'start', target: 'polish' }, { id: 'b', source: 'polish', target: 'out' }],
  })
  const created = await page.request.post(`/api/v2/tenants/${tid}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const workflowId = (await created.json()).workflow.id
  expect((await page.request.post(`/api/v2/tenants/${tid}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)

  await page.reload()
  await page.getByRole('button', { name: '选择项目' }).click()
  await page.getByRole('option', { name: 'Demo', exact: true }).click()
  await page.getByRole('button', { name: '运行工作流', exact: true }).click()
  await page.getByRole('menuitem', { name: new RegExp(name) }).click()
  const create = page.getByRole('dialog', { name: '运行工作流' })
  await create.getByLabel('运行名称').fill(runName)
  await create.getByRole('button', { name: '创建运行' }).click()
  const pane = page.getByRole('region', { name: '工作流运行' })
  await expect(pane.getByRole('heading', { name: runName })).toBeVisible()

  await pane.getByRole('button', { name: '启动' }).click()
  const start = page.getByRole('dialog')
  try {
    await expect(start.getByRole('button', { name: '启动' })).toBeDisabled()
  } finally {
    releaseAnalysis()
  }
  const warning = start.getByRole('alert')
  await expect(warning).toContainText('有节点暂不支持运行')
  await expect(warning).toContainText('「润色提示词」（提示词节点）')
  await expect(warning).toContainText('「检索工具」（工具节点）')
  await expect(warning).not.toContainText('备用工具')
  await page.screenshot({ path: testInfo.outputPath('start-unsupported.png'), animations: 'disabled' })

  // The warning does not decide anything: start is still the server's to refuse.
  const refused = page.waitForResponse((r) => /\/workflow-runs\/[^/]+\/start$/.test(new URL(r.url()).pathname))
  await start.getByRole('button', { name: '启动' }).click()
  expect((await refused).status()).toBe(400)
})
