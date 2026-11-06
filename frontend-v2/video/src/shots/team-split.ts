import type { Shot } from '../record/shot.ts'

const title = '看板支持按负责人筛选和批量指派'
const description = [
  '小队人多以后，看板上很难找到自己的任务，Tech Lead 分派也要一张张点。',
  '验收：看板顶部可以按负责人筛选；可以多选卡片一次指派给同一个人或 Agent；筛选条件刷新后保留。',
].join('\n')

/**
 * The third part of the film opens here: 陈默 splits a big requirement with the 需求拆分 workflow,
 * starts an Agent on one subtask and hands another to 苏芮. Her shot and the review pick up from
 * this one in the same environment. Subtasks are taken by position, since the model writes them.
 */
export const teamSplit: Shot = {
  name: 'team-split',
  scene: 'lead',
  account: 'chen.mo@demo.ora-space.dev',
  start: '/collaboration',
  async ready(page) {
    await page.getByRole('list', { name: '任务看板' }).waitFor()
  },
  async play(d, page) {
    d.slide({
      eyebrow: '03 / 团队协作',
      title: '团队协同，人机共创',
      subtitle: '任务拆分、分工执行、验收闭环',
      narration: '团队协同，人机共创：复杂需求由人与 Agent 分工完成。',
    })

    d.say({ narration: '陈默创建需求，记录目标与验收标准。' })
    await d.focus('page', 1.3)
    await d.click(page.getByRole('button', { name: '新建任务' }).first())
    const dialog = page.getByRole('dialog')
    await d.type(dialog.getByLabel('任务标题'), title, { rate: 5 })
    await d.select(dialog.getByLabel('所属项目'), '协作看板服务')
    await d.type(dialog.getByLabel('描述（可选）'), description, { rate: 20 })
    await d.click(dialog.getByRole('button', { name: '创建任务' }))

    d.say({ narration: '通过「拆分」，调用团队的「需求拆分」工作流。' })
    await page.getByRole('heading', { level: 2, name: title }).waitFor()
    await d.click(page.getByRole('button', { name: '拆分', exact: true }))
    const split = page.getByRole('dialog')
    await d.hold(300)
    await d.click(split.getByRole('button', { name: '开始拆分' }))

    const run = page.getByRole('region', { name: '工作流运行' })
    // The window freezes for the explanation, so it has to show the run, not its loading line.
    await run.getByText('执行路径').waitFor()
    await page.waitForTimeout(600) // The screencast lags the page, and the frozen frame is the last one it sent.
    await d.explain({
      title: '需求拆分流程',
      narration: '工作流自动完成三个步骤：',
      items: [
        { title: '解析需求', body: '提取目标、范围与验收标准', narration: '解析需求，' },
        { title: '拆分任务', body: '按可独立交付的粒度划分', narration: '拆分任务，' },
        { title: '生成子任务', body: '自动写入任务看板', narration: '生成子任务。' },
      ],
    })

    // Starting opens the run with the subtasks folded; they are the payoff, so the shot opens them.
    const expand = page.getByRole('button', { name: '展开子任务' })
    await d.wait(expand, { rate: 20, timeout: 120_000 })
    await d.click(expand)
    const subtasks = page.getByRole('list', { name: '子任务' })
    await subtasks.waitFor()
    await d.mark(subtasks, { label: '子任务', narration: '子任务已生成。' })
    await d.hold(1400)

    d.say({ narration: '可直接执行的任务，指派给 Agent，即刻启动。' })
    await d.click(subtasks.getByRole('link').nth(0))
    const assignee = page.getByLabel('指派给')
    await assignee.waitFor()
    await d.select(assignee, 'OpenCode')
    await page.getByText('当前：OpenCode').waitFor()
    await d.click(page.getByRole('button', { name: '开始处理', exact: true }))
    await d.hold(600)

    d.say({ narration: '需要人工把关的任务，指派给苏芮。' })
    await d.click(page.getByRole('link', { name: title }))
    await subtasks.waitFor()
    await d.click(subtasks.getByRole('link').nth(1))
    await assignee.waitFor()
    await d.select(assignee, '苏芮')
    await page.getByText('当前：苏芮').waitFor()
    await d.hold(800)
  },
}
