import type { Shot } from '../record/shot.ts'

/**
 * 陈默 comes back to the board and accepts 苏芮's work, which closes the third part. Recording
 * starts only once both subtasks of `team-split` are waiting for review.
 */
export const teamReview: Shot = {
  name: 'team-review',
  scene: 'lead',
  account: 'chen.mo@demo.ora-space.dev',
  start: '/collaboration?view=all',
  async ready(page) {
    const review = page.getByRole('listitem', { name: '待验收列' })
    await review.getByText('拆分自').nth(1).waitFor({ timeout: 300_000 })
  },
  async play(d, page) {
    const review = page.getByRole('listitem', { name: '待验收列' })

    d.slide({
      eyebrow: '陈默',
      title: '成果验收',
      narration: '成果统一进入「待验收」，由负责人审查。',
    })

    await d.focus('page', 1)
    await d.hold(400)
    await d.mark(review, { label: '待验收' })

    d.say({ narration: '陈默审查苏芮提交的变更，确认通过。' })
    await d.focus('page', 1.3)
    const filter = page.getByLabel('指派对象')
    const assignee = await filter.locator('option', { hasText: '苏芮' }).first().textContent()
    await d.select(filter, assignee!)
    await d.hold(400)
    await d.click(review.getByRole('button', { name: /^查看任务/ }).filter({ hasText: '拆分自' }).first())
    const workspace = page.getByRole('button', { name: '工作区', exact: true })
    await workspace.waitFor()
    await d.hold(300)
    await d.click(workspace)
    const panel = page.getByRole('complementary', { name: '工作区' })
    const file = panel.getByRole('button').filter({ hasText: /\.mjs/ }).first()
    await file.waitFor()
    await d.focus(panel, 1.5)
    await d.hold(300)
    await d.click(file)
    await d.hold(1000)
    await d.focus('page', 1.3)
    await d.click(workspace)
    await d.hold(200)
    await d.click(page.getByRole('button', { name: '确认完成' }))
    const done = page.locator('.mc-task-bar .mc-status', { hasText: '已完成' })
    await done.waitFor()
    await d.hold(300)

    await d.mark(done, { label: '已完成', narration: '验收通过，任务完成。' })
    await d.hold(400)

    d.slide({
      eyebrow: '协作闭环',
      title: '团队协同，人机共创',
      narration: '人把控方向，人机协同交付：',
      items: [
        { title: '陈默 · 拆分需求', body: '工作流生成子任务并完成分配', narration: '拆分需求，' },
        { title: 'OpenCode · 自主执行', body: 'Agent 独立完成开发任务', narration: '自主执行，' },
        { title: '苏芮 · 人机协作', body: '选用工作流，完成开发并提交', narration: '人机协作，' },
        { title: '陈默 · 审查验收', body: '审查变更，确认交付', narration: '审查验收。' },
      ],
    })
  },
}
