import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('workflows: build a chain on the server, keep it across a reload, add the samples', async ({ page }) => {
  await page.goto('/workflows')
  await signIn(page)
  const library = page.getByRole('complementary', { name: '工作流列表' })
  // The e2e database outlives a run, so each run makes its own workflow.
  const name = `端到端验收 ${Date.now()}`

  // Create a workflow and add nodes from the catalog.
  await library.getByRole('button', { name: '新建或导入工作流' }).click()
  await page.getByRole('menuitem', { name: /新建工作流/ }).click()
  const create = page.getByRole('alertdialog', { name: '新建工作流' })
  await create.getByLabel('工作流名称').fill(name)
  await create.getByRole('button', { name: '新建工作流' }).click()
  await expect(create).toBeHidden()
  const canvas = page.getByLabel('工作流画布')
  await expect(page.getByRole('textbox', { name: '工作流名称' })).toHaveValue(name)
  const catalog = page.getByRole('toolbar', { name: '添加工作流节点' })
  for (const kind of ['Agent', '输出']) {
    await catalog.getByRole('button', { name: kind, exact: true }).click()
    // Adding a node opens its inspector over the catalog; close it before the next one.
    await canvas.press('Escape')
  }
  await expect(canvas).toHaveAttribute('data-workflow-node-count', '3')

  // The draft autosaves to v2-server, so a reload in a fresh page restores it.
  await expect(page.getByText('已实时保存')).toBeVisible()
  await page.reload()
  await library.getByRole('button', { name, exact: true }).click()
  await expect(page.getByRole('textbox', { name: '工作流名称' })).toHaveValue(name)
  await expect(canvas).toHaveAttribute('data-workflow-node-count', '3')

  // v2-server checks a graph before publishing it; until the workflow kernel is wired into it,
  // publishing is refused, and the page says so instead of pretending a version exists.
  await expect(page.getByText('暂未发布')).toBeVisible()
  await page.getByRole('button', { name: '发布', exact: true }).click()
  const publish = page.getByRole('alertdialog', { name: '发布工作流' })
  await publish.getByRole('textbox').fill('v1')
  await publish.getByRole('button', { name: '发布' }).click()
  // The library shows the refusal as desktop's editor does, and the workflow stays unpublished.
  await expect(page.getByRole('alert').filter({ hasText: '服务端暂时不能检查和发布工作流，草稿已保存。' })).toBeVisible()
  await expect(page.getByText('暂未发布')).toBeVisible()

  // The samples join the tenant's library only when asked for.
  await library.getByRole('button', { name: '添加样例工作流' }).click()
  await expect(library.getByRole('button', { name: '示例：代码审查', exact: true })).toBeVisible()
  await expect(library.getByRole('button', { name: '循环综合测试：分支与退出', exact: true })).toBeVisible()

  // Every toast can be dismissed.
  const toast = page.locator('[data-sonner-toast]').filter({ hasText: '样例' })
  await expect(toast).toBeVisible()
  await toast.getByRole('button', { name: 'Close toast' }).click()
  await expect(toast).toBeHidden()
})
