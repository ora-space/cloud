/// <reference lib="dom" />
import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('create a task, assign it, and walk it through the lifecycle', async ({ page }) => {
  const title = `e2e 任务-${crypto.randomUUID().slice(0, 8)}`
  await page.goto('/collaboration')
  await signIn(page)
  await expect(page.getByRole('heading', { level: 1, name: '协作' })).toBeVisible()

  // A task always belongs to a project; the form waits for the demo project rather than inventing one.
  await page.getByRole('button', { name: '新建任务' }).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByLabel('任务标题').fill(title)
  await dialog.getByLabel('描述（可选）').fill('由 e2e 创建的任务')
  await expect(dialog.getByLabel('所属项目')).not.toHaveValue('')
  await page.screenshot({ path: test.info().outputPath('task-create.png'), fullPage: true })
  await dialog.getByRole('button', { name: '创建任务' }).click()
  await expect(page.getByText('任务已创建')).toBeVisible()
  await expect(dialog.getByText('待处理', { exact: true })).toBeVisible()

  // Assignment is one typed target; a person can start the task by hand.
  await dialog.getByLabel('指派给').selectOption({ label: '演示 Owner' })
  await expect(page.getByText('已指派给 演示 Owner')).toBeVisible()
  await expect(dialog.getByText(/指派给小队只表示这个队负责，不会自动分派给成员/)).toBeVisible()
  await page.screenshot({ path: test.info().outputPath('task-detail.png'), fullPage: true })

  // The lifecycle moves one step at a time and ends terminal.
  await dialog.getByRole('button', { name: '开始处理' }).click()
  await expect(dialog.getByText('进行中', { exact: true })).toBeVisible()
  await dialog.getByRole('button', { name: '提交验收' }).click()
  await expect(dialog.getByText('待验收', { exact: true })).toBeVisible()
  await dialog.getByRole('button', { name: '确认完成' }).click()
  await expect(dialog.getByText('已完成', { exact: true })).toBeVisible()
  await expect(dialog.getByText('任务已结束，不能再改状态。')).toBeVisible()
  await page.keyboard.press('Escape')

  // The task, its assignee and its final status survive a reload.
  await page.reload()
  const row = page.getByRole('button', { name: `查看任务 ${title}` })
  await expect(row).toBeVisible()
  await expect(row).toContainText('已完成')
  await expect(row).toContainText('演示 Owner')
})
