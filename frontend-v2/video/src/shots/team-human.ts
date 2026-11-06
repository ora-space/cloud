import type { Shot } from '../record/shot.ts'

/**
 * 苏芮 takes the subtask 陈默 assigned and chooses how to do it, this time with a workflow. Recorded after
 * `team-split` in the same environment, while the Agent's subtask is still running.
 */
export const teamHuman: Shot = {
  name: 'team-human',
  scene: 'lead',
  account: 'su.rui@demo.ora-space.dev',
  start: '/collaboration?view=mine',
  async ready(page) {
    await page.getByRole('listitem', { name: '待处理列' }).getByRole('button', { name: /^查看任务/ }).first().waitFor()
  },
  async play(d, page) {
    d.slide({
      eyebrow: '苏芮',
      title: '灵活的任务执行方式',
      narration: '执行方式由成员选择：',
      items: [
        { title: '人工开发', body: '直接修改代码，把控实现细节', narration: '人工开发，' },
        { title: 'Agent 协作', body: '调整提示词，由 Agent 执行', narration: '与 Agent 协作，' },
        { title: '工作流执行', body: '复用团队沉淀的标准流程', narration: '或运行工作流。' },
      ],
    })

    d.say({ narration: '本次，苏芮选择开发工作流。' })
    await d.focus('page', 1.3)
    await d.click(page.getByRole('listitem', { name: '待处理列' }).getByRole('button', { name: /^查看任务/ }).first())
    await d.hold(500)
    await d.click(page.getByRole('button', { name: '用工作流处理' }))
    const dialog = page.getByRole('dialog')
    const prompt = dialog.getByLabel('需求')
    await prompt.waitFor()
    await d.hold(600)

    d.say({ narration: '补充需求细节与约束条件，明确验收标准。' })
    await d.focus(prompt, 1.6)
    await d.click(prompt)
    await page.keyboard.press('Control+End')
    await page.keyboard.type('沿用现有技术栈，不新增依赖；改动要带测试。', { delay: 45 })
    await d.hold(1200)
    await d.focus('page', 1.3)
    await d.click(dialog.getByRole('button', { name: '启动工作流' }))

    const run = page.getByRole('region', { name: '工作流运行' })
    // The window freezes for the explanation, so it has to show the run, not its loading line.
    await run.getByText('执行路径').waitFor()
    await page.waitForTimeout(600) // The screencast lags the page, and the frozen frame is the last one it sent.
    await d.explain({
      title: '开发工作流',
      narration: '工作流按需求自动执行：',
      items: [
        { title: '制定计划', body: '分析代码库，明确实现方案与验收清单', narration: '制定计划，' },
        { title: '实现需求', body: '完成开发并补充测试', narration: '实现需求，' },
        { title: '验证交付', body: '运行测试并生成交付说明', narration: '验证交付。' },
      ],
    })

    await d.wait(run.getByText('成功').first(), { rate: 20 })
    await d.hold(600)

    d.say({ narration: '执行完成后，苏芮在工作区审查变更，提交验收。' })
    await d.click(run.getByRole('group', { name: '运行视图' }).getByRole('button', { name: '全图' }))
    await d.hold(200)
    await d.click(page.getByRole('button', { name: '工作区', exact: true }))
    const panel = page.getByRole('complementary', { name: '工作区' })
    const file = panel.getByRole('button').filter({ hasText: /\.mjs/ }).first()
    await file.waitFor()
    await d.focus(panel, 1.5)
    await d.hold(300)
    await d.click(file)
    await d.hold(1000)
    await d.focus('page', 1.3)
    await d.click(page.getByRole('button', { name: '工作区', exact: true }))
    await d.hold(200)
    await d.click(page.getByRole('button', { name: '提交验收' }))
    await page.locator('.mc-task-bar .mc-status', { hasText: '待验收' }).waitFor()
    await d.hold(400)
  },
}
