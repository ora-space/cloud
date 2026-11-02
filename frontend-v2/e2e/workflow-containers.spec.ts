import { expect, test, type Page } from '@playwright/test'
import { signIn } from './login.ts'

// This Node test reads the wire response; importing the browser client would load its bundler
// module graph into Playwright's NodeNext project.
interface ContainerDetail {
  run: { status: string; output: string | null; error: string | null }
  scopes: { id: string; roundIndex: number; status: string }[]
  nodes: { id: string; nodeId: string; scopeId: string; sessionId: string | null; iteration: number | null; payload: string | null }[]
}

const agent = (prompt: string) => ({ executor: { agentCli: 'official/ora-space.opencode', modelId: 'fixture' }, prompt })
const node = (id: string, kind: string, title: string, x: number, y: number, data: Record<string, unknown> = {}, parentId?: string) => ({ id, type: 'workflow', position: { x, y }, parentId, data: { kind, title, ...data } })
const edge = (source: string, target: string) => ({ id: `${source}-${target}`, source, target })

/** Two isolated feedback rounds, followed by two array iterations in the root scope. */
const containers = JSON.stringify({ schemaVersion: 2, nodes: [
  node('start', 'start', '开始', 0, 0, { inputVariables: [{ name: 'items', valueType: 'array[string]', value: ['第一段', '第二段'] }] }),
  node('loop', 'loop', '反馈循环', 320, 0, { loopConfig: {
    maxIterations: 2, maxIterationsBehavior: 'succeed',
    variables: [{ name: 'draft', valueType: 'string', initial: { kind: 'constant', value: '初稿' }, feedback: ['writer', 'output'] }],
    until: { logic: 'and', conditions: [] }, outputs: [{ name: 'result', variableSelector: ['writer', 'output'] }],
  } }),
  node('entry', 'start', '本轮开始', 40, 80, { containerId: 'loop' }, 'loop'),
  node('writer', 'agent', '改进初稿', 350, 80, { containerId: 'loop', agentConfig: agent('改进：{{#loop.draft#}}') }, 'loop'),
  node('iter', 'iteration', '逐段处理', 1040, 0, { iterationConfig: { iteratorSelector: ['start', 'items'], collectSelector: ['worker', 'output'], errorStrategy: 'continue', maxIterations: 50 } }),
  node('worker', 'agent', '处理段落', 40, 80, { agentConfig: agent('处理第 {{#iter.index#}} 段：{{#iter.item#}}') }, 'iter'),
  node('out', 'output', '输出', 1500, 0, { outputs: [{ name: 'draft', variableSelector: ['loop', 'result'] }, { name: 'paragraphs', variableSelector: ['iter', 'output'] }] }),
], edges: [edge('start', 'loop'), edge('entry', 'writer'), edge('loop', 'iter'), edge('iter', 'worker'), edge('iter', 'out')] })

const exit = JSON.stringify({ schemaVersion: 2, nodes: [
  node('start', 'start', '开始', 0, 0),
  node('loop', 'loop', '主动退出循环', 320, 0, { loopConfig: {
    maxIterations: 3, variables: [{ name: 'draft', valueType: 'string', initial: { kind: 'constant', value: '保留当前结果' }, feedback: ['loop', 'draft'] }],
    until: { logic: 'and', conditions: [] }, outputs: [{ name: 'result', variableSelector: ['loop', 'draft'] }],
  } }),
  node('entry', 'start', '本轮开始', 40, 80, { containerId: 'loop' }, 'loop'),
  node('exit', 'loopExit', '退出循环', 350, 80, { containerId: 'loop' }, 'loop'),
  node('out', 'output', '输出', 1100, 0, { outputs: [{ name: 'result', variableSelector: ['loop', 'result'] }] }),
], edges: [edge('start', 'loop'), edge('entry', 'exit'), edge('loop', 'out')] })

const capFailure = JSON.stringify({ schemaVersion: 2, nodes: [
  node('start', 'start', '开始', 0, 0),
  node('loop', 'loop', '轮次上限', 320, 0, { loopConfig: {
    maxIterations: 1, maxIterationsBehavior: 'fail',
    variables: [{ name: 'draft', valueType: 'string', initial: { kind: 'constant', value: '待改进' }, feedback: ['loop', 'draft'] }],
    until: { logic: 'and', conditions: [] }, outputs: [{ name: 'result', variableSelector: ['loop', 'draft'] }],
  } }),
  node('entry', 'start', '本轮开始', 40, 80, { containerId: 'loop' }, 'loop'),
  node('out', 'output', '输出', 1100, 0, { outputs: [{ name: 'result', variableSelector: ['loop', 'result'] }] }),
], edges: [edge('start', 'loop'), edge('loop', 'out')] })

async function execute(page: Page, graph: string, title: string, expectedStatus: 'succeeded' | 'failed' = 'succeeded'): Promise<ContainerDetail> {
  await page.goto('/chat')
  await signIn(page)
  const tenant = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id as string
  const base = `/api/v2/tenants/${tenant}`
  const projects = (await (await page.request.get(`${base}/projects?limit=100`)).json()).items as { id: string; name: string }[]
  const projectId = projects.find((project) => project.name === 'Demo')!.id
  const name = `${title} ${Date.now().toString(36)}`
  const created = await page.request.post(`${base}/workflows`, { data: { name, graph } })
  expect(created.status()).toBe(201)
  const workflowId = (await created.json()).draft.workflowId as string
  const published = await page.request.post(`${base}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })
  expect(published.ok(), await published.text()).toBe(true)
  const response = await page.request.post(`${base}/workflows/${workflowId}/runs`, { data: { name, projectId } })
  expect(response.status()).toBe(201)
  const id = (await response.json()).run.id as string
  const started = await page.request.post(`${base}/workflow-runs/${id}/start`)
  expect(started.ok(), await started.text()).toBe(true)
  await expect.poll(async () => (await (await page.request.get(`${base}/workflow-runs/${id}`)).json()).run.status, { timeout: 150_000 }).not.toMatch(/^(pending|running)$/)
  const detail = await (await page.request.get(`${base}/workflow-runs/${id}`)).json() as ContainerDetail
  expect(detail.run.status, detail.run.error ?? '').toBe(expectedStatus)
  await page.reload()
  await page.getByRole('button', { name: `工作流运行 ${name}（${expectedStatus === 'succeeded' ? '成功' : '失败'}）` }).click()
  await expect(page.getByRole('region', { name: '工作流运行' }).getByRole('heading', { name })).toBeVisible()
  await page.getByRole('region', { name: '工作流运行' }).getByRole('button', { name: '舞台', exact: true }).click()
  return detail
}

async function screenshot(page: Page, path: string) {
  await page.waitForFunction(() => document.getAnimations().filter((animation) => animation.timeline === document.timeline && animation.effect?.getComputedTiming().endTime !== Infinity).every((animation) => animation.playState !== 'running'))
  await page.screenshot({ path })
}

test('workflow containers: a round limit shows the domain error without corrupt-snapshot advice', async ({ page }, info) => {
  await page.setViewportSize({ width: 1600, height: 1000 })
  const detail = await execute(page, capFailure, '轮次上限失败验收', 'failed')
  expect(detail.run.error).toContain('Loop did not terminate within 1 rounds')
  expect(JSON.parse(detail.nodes.find((node) => node.nodeId === 'loop')!.payload ?? '{}')).not.toHaveProperty('error_detail')
  const pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '全图', exact: true }).click()
  await pane.getByLabel('工作流全图俯瞰').locator('.react-flow__node').filter({ has: page.getByText('轮次上限', { exact: true }) }).click()
  await expect(pane.getByRole('alert').filter({ hasText: 'Loop did not terminate within 1 rounds' })).toBeVisible()
  await expect(pane.getByText('运行的冻结数据无效', { exact: true })).toBeHidden()
  await expect(pane.getByText(/该运行的快照已损坏/)).toBeHidden()
  await expect(pane.getByRole('button', { name: '再跑一次', exact: true })).toBeVisible()
  await expect(pane.getByText('可在顶部点击「再跑一次」创建新运行。', { exact: true })).toBeVisible()
  await expect(pane.getByText(/从失败处继续/)).toBeHidden()
  await screenshot(page, info.outputPath('s3-loop-domain-failure.png'))
})

test('workflow containers: LoopExit exports and displays the committed exit reason', async ({ page }, info) => {
  const detail = await execute(page, exit, '主动退出验收')
  expect(detail.scopes).toHaveLength(1)
  expect(detail.scopes[0]).toMatchObject({ roundIndex: 1, status: 'succeeded' })
  expect(JSON.parse(detail.run.output!)).toEqual({ result: '保留当前结果' })
  const pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '主动退出循环: 成功', exact: true }).click()
  await expect(pane.getByRole('button', { name: /第 1 轮 · 已完成 · 通过退出节点结束/ })).toBeVisible()
  await pane.getByRole('button', { name: '循环结果', exact: true }).click()
  await expect(pane.getByRole('region', { name: '循环结果' }).getByText('已完成 · 通过退出节点结束')).toBeVisible()
  await screenshot(page, info.outputPath('s3-loop-exit.png'))
})

test('workflow containers: real Agents retain isolated Loop and Iteration round histories', async ({ page }, info) => {
  test.skip(!process.env.V2_NODE_ASSETS_DIR, 'needs the prepared Node fixture executor')
  test.setTimeout(240_000)
  const subscribed: string[] = []
  page.on('request', (request) => {
    const match = /\/workflow-runs\/[^/]+\/nodes\/([^/]+)\/events/.exec(request.url())
    if (match) subscribed.push(match[1]!)
  })
  const detail = await execute(page, containers, '容器轮次验收')
  expect(detail.scopes.map((scope) => [scope.roundIndex, scope.status])).toEqual([[1, 'succeeded'], [2, 'succeeded']])
  const writers = detail.nodes.filter((node) => node.nodeId === 'writer')
  expect(new Set(writers.map((node) => node.scopeId)).size).toBe(2)
  expect(new Set(writers.map((node) => node.sessionId)).size).toBe(2)
  expect(writers.every((node) => node.iteration === null)).toBe(true)
  expect(detail.nodes.filter((node) => node.nodeId === 'worker').map((node) => node.iteration)).toEqual([0, 1])
  const pane = page.getByRole('region', { name: '工作流运行' })
  await pane.getByRole('button', { name: '反馈循环: 成功', exact: true }).click()
  await pane.getByRole('button', { name: /第 2 轮 · 达到最大轮次/ }).click()
  await expect(pane.getByText('已开始 2 轮 / 最大 2 轮')).toBeVisible()
  await screenshot(page, info.outputPath('s3-loop-rounds.png'))
  await pane.getByRole('region', { name: '本轮概览' }).getByRole('button', { name: /改进初稿/ }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect.poll(() => subscribed).toContain(writers[1]!.id)
  await expect(pane.getByText('<workspace_root>', { exact: false }).first()).toBeVisible()
  await pane.getByRole('button', { name: '反馈循环: 成功', exact: true }).click()
  await pane.getByRole('button', { name: /第 1 轮 ·/ }).click()
  await pane.getByRole('region', { name: '本轮概览' }).getByRole('button', { name: /改进初稿/ }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  await expect.poll(() => subscribed).toContain(writers[0]!.id)
  await expect(pane.getByText(/改进：初稿/)).toBeVisible()
  await pane.getByRole('button', { name: '逐段处理: 成功', exact: true }).click()
  await pane.getByRole('button', { name: '选择迭代轮次' }).click()
  await page.getByRole('option', { name: '第 2 轮', exact: true }).click()
  await expect(pane.getByRole('button', { name: '选择迭代轮次' })).toContainText('2')
  await pane.getByRole('button', { name: '处理段落: 成功', exact: true }).click()
  await pane.getByRole('button', { name: '查看节点会话' }).click()
  const workers = detail.nodes.filter((node) => node.nodeId === 'worker')
  await expect.poll(() => subscribed).toContain(workers.find((node) => node.iteration === 1)!.id)
  await expect(pane.getByText('<workspace_root>', { exact: false }).first()).toBeVisible()
  await expect(pane.getByText(/处理第 1 段：第二段/)).toBeVisible()
  await pane.getByRole('button', { name: '选择迭代轮次' }).click()
  await page.getByRole('option', { name: '第 1 轮', exact: true }).click()
  await expect.poll(() => subscribed).toContain(workers.find((node) => node.iteration === 0)!.id)
  await expect(pane.getByText(/处理第 0 段：第一段/)).toBeVisible()
  await expect(pane.getByText(/处理第 1 段：第二段/)).toBeHidden()
  await pane.getByRole('button', { name: '选择迭代轮次' }).click()
  await page.getByRole('option', { name: '第 2 轮', exact: true }).click()
  await expect(pane.getByText(/处理第 1 段：第二段/)).toBeVisible()
  const toEnd = pane.getByRole('button', { name: '滚动到底部' })
  if (await toEnd.isVisible()) await toEnd.click()
  await expect(pane.getByText(/v2 fixture tool executed/).first()).toBeVisible()
  await screenshot(page, info.outputPath('s3-iteration-rounds.png'))
})
