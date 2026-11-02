/// <reference lib="dom" />
import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'
import { containerWorkflowGraph, type ContainerDetail } from './workflow-container-graph.ts'

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

for (const containers of [false, true]) {
  test(containers ? 'a scheduled container workflow preserves variables and per-round session history' : 'a scheduled workflow starts its own run of the published version and reads it back in history', async ({ page }, testInfo) => {
    test.skip(containers && !realAgents, 'needs the prepared Node fixture executor for container history')
    test.setTimeout(realAgents ? 240_000 : 90_000)
    const subscribed: string[] = []
    page.on('request', (request) => {
      const match = /\/workflow-runs\/[^/]+\/nodes\/([^/]+)\/events/.exec(request.url())
      if (match) subscribed.push(match[1]!)
    })
    await page.goto('/schedules')
    await signIn(page)

    // Publish a workflow on v2-server; names are fresh so earlier runs of this spec do not interfere.
    const tenantId = ((await (await page.request.get('/api/v1/me/tenants')).json()) as { items: { id: string }[] }).items[0]!.id
    const suffix = Date.now().toString(36)
    const workflowName = `${containers ? '定时容器' : '定时起草'} ${suffix}`
    const taskName = `${containers ? '定时容器验收' : '定时工作流'}-${suffix}`
    const created = await page.request.post(`/api/v2/tenants/${tenantId}/workflows`, { data: { name: workflowName, graph: containers ? containerWorkflowGraph : graph } })
    expect(created.status()).toBe(201)
    const workflowId = ((await created.json()) as { workflow: { id: string } }).workflow.id
    expect((await page.request.post(`/api/v2/tenants/${tenantId}/workflows/${workflowId}/publish`, { data: { version: 'v1' } })).ok()).toBe(true)
    const frozenSnapshotId = (await (await page.request.get(`/api/v2/tenants/${tenantId}/workflows/${workflowId}`)).json()).published.id as string
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
    const runRead = page.waitForResponse((response) => /\/workflow-runs\/[^/?]+$/.test(new URL(response.url()).pathname) && response.request().method() === 'GET')
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
    const detail = await (await page.request.get((await runRead).url())).json() as ContainerDetail
    expect(detail.run.snapshotId).toBe(frozenSnapshotId)
    expect(detail.run.input).toBe('给支付模块加一个对账接口')
    expect(detail.failedAttempts).toEqual([])
    await page.screenshot({ path: testInfo.outputPath('schedule-workflow-history.png'), fullPage: true })

    // Pause it, so no later trigger of this spec's task runs on the shared e2e database.
    await dialog.getByRole('button', { name: '暂停任务' }).click()
    await expect(dialog.getByText('已暂停')).toBeVisible()

    // The history opens the occurrence's own run page directly.
    await dialog.getByRole('link', { name: '打开运行页' }).click()
    await expect(page).toHaveURL(/\/chat$/)
    const pane = page.getByRole('region', { name: '工作流运行' })
    await expect(pane.getByRole('heading', { name: taskName })).toBeVisible()
    await page.screenshot({ path: testInfo.outputPath('schedule-workflow-run-page.png'), fullPage: true })

    // The run is an ordinary run of the project: the chat page lists it under Demo.
    await page.goto('/chat')
    const demo = page.getByRole('complementary', { name: '聊天记录' }).getByRole('region', { name: '项目 Demo' })
    const runRow = demo.getByRole('button', { name: `工作流运行 ${taskName}（${realAgents ? '成功' : '失败'}）` })
    await expect(runRow).toBeVisible()
    if (!containers) return

    // The merged detail keeps both the root variable projection and the container histories.
    expect(detail.scopes.map((scope) => [scope.roundIndex, scope.status])).toEqual([[1, 'succeeded'], [2, 'succeeded']])
    expect(detail.variables).toContainEqual(expect.objectContaining({ selector: ['start', 'items'], value: ['第一段', '第二段'] }))
    expect(detail.variables.some((variable) => variable.selector.join('.') === 'loop.draft')).toBe(false)
    const writers = detail.nodes.filter((node) => node.nodeId === 'writer')
    const workers = detail.nodes.filter((node) => node.nodeId === 'worker')
    const task = (node: ContainerDetail['nodes'][number]) => (JSON.parse(node.payload!) as { task: string }).task
    expect(task(writers[0]!)).toBe('改进：初稿')
    expect(task(writers[1]!)).toMatch(/^改进：V2 fixture complete/)
    expect(workers.map((node) => [node.iteration, task(node)])).toEqual([[0, '处理第 0 段：第一段'], [1, '处理第 1 段：第二段']])
    expect(detail.iterationConditionDecisions).toEqual({})
    expect(JSON.parse(detail.run.output!)).toMatchObject({ draft: expect.stringContaining('V2 fixture complete'), paragraphs: [expect.any(String), expect.any(String)] })

    await runRow.click()
    await pane.getByRole('button', { name: '全图', exact: true }).click()
    await pane.getByLabel('工作流全图俯瞰').getByTestId('rf__node-start').click()
    await pane.getByRole('button', { name: '打开节点详情', exact: true }).click()
    await expect(pane.getByLabel('阶段详情').getByRole('textbox', { name: '初始 Prompt', exact: true })).toHaveValue('给支付模块加一个对账接口')
    await expect(pane.getByLabel('阶段详情').getByRole('textbox', { name: 'items (array[string])', exact: true })).toHaveValue('["第一段","第二段"]')
    await page.screenshot({ path: testInfo.outputPath('schedule-container-start.png'), fullPage: true })

    // Refresh and reopen from the project history, then read the exact attempts' session streams.
    await page.reload()
    await runRow.click()
    await pane.getByRole('button', { name: '舞台', exact: true }).click()
    await pane.getByRole('button', { name: '反馈循环: 成功', exact: true }).click()
    await pane.getByRole('button', { name: /第 2 轮 · 达到最大轮次/ }).click()
    await pane.getByRole('region', { name: '本轮概览' }).getByRole('button', { name: /改进初稿/ }).click()
    await pane.getByRole('button', { name: '查看节点会话' }).click()
    await expect.poll(() => subscribed).toContain(writers[1]!.id)
    await expect(pane.getByText(/改进：V2 fixture complete/)).toBeVisible()
    await pane.getByRole('button', { name: '逐段处理: 成功', exact: true }).click()
    await pane.getByRole('button', { name: '选择迭代轮次' }).click()
    await page.getByRole('option', { name: '第 1 轮', exact: true }).click()
    await pane.getByRole('button', { name: '处理段落: 成功', exact: true }).click()
    await pane.getByRole('button', { name: '查看节点会话' }).click()
    await expect.poll(() => subscribed).toContain(workers[0]!.id)
    await expect(pane.getByText(/处理第 0 段：第一段/)).toBeVisible()
    await pane.getByRole('button', { name: '选择迭代轮次' }).click()
    await page.getByRole('option', { name: '第 2 轮', exact: true }).click()
    await expect.poll(() => subscribed).toContain(workers[1]!.id)
    await expect(pane.getByText(/处理第 1 段：第二段/)).toBeVisible()
    await expect(pane.getByText(/处理第 0 段：第一段/)).toBeHidden()
    const toolCall = pane.getByText(/v2 fixture tool executed/).first()
    await toolCall.scrollIntoViewIfNeeded()
    await expect(toolCall).toBeInViewport()
    await page.screenshot({ path: testInfo.outputPath('schedule-container-session.png'), fullPage: true })
  })
}
