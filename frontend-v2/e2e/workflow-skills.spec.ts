import { expect, test } from '@playwright/test'
import { signIn } from './login.ts'

test('workflow dependencies: select an imported Skill, persist its binding and omit demo MCPs', async ({ page }, info) => {
  await page.setViewportSize({ width: 1440, height: 1000 })
  await page.goto('/settings/skills')
  await signIn(page)
  const name = `workflow-import-${Date.now()}`
  await page.getByRole('button', { name: '导入技能' }).click()
  await page.getByLabel(/选择 SKILL.md/).setInputFiles({
    name: 'SKILL.md', mimeType: 'text/markdown',
    buffer: Buffer.from(`---\nname: ${name}\ndescription: 工作流导入验证\n---\n\n执行前核对项目 README。\n`),
  })
  await page.getByRole('dialog').getByRole('button', { name: '导入', exact: true }).click()
  await expect(page.getByRole('article', { name })).toBeVisible()
  const tid = (await (await page.request.get('/api/v1/me/tenants')).json()).items[0].id
  const base = `/api/v2/tenants/${tid}/workflows`
  const created = await page.request.post(base, { data: { name, graph: null } })
  expect(created.status()).toBe(201)
  const wid = (await created.json()).workflow.id
  await page.goto('/workflows')
  const open = async () => page.getByRole('complementary', { name: '工作流列表' }).getByRole('button', { name, exact: true }).click()
  await open()
  await page.getByRole('toolbar', { name: '添加工作流节点' }).getByRole('button', { name: 'Agent', exact: true }).click()
  await page.getByRole('button', { name: '添加 Skill', exact: true }).click()
  await page.getByRole('option', { name, exact: true }).click()
  const node = async () => {
    const draft = (await (await page.request.get(`${base}/${wid}/draft`)).json()).snapshot
    return JSON.parse(draft.graph).nodes.find((n: { data: { kind: string } }) => n.data.kind === 'agent')
  }
  await expect.poll(async () => (await node())?.data.agentConfig.skills).toEqual([{ skillId: name, enabled: true }])
  const agentID = (await node()).id
  await page.reload()
  await open()
  await page.locator(`.react-flow__node[data-id="${agentID}"]`).click()
  const toggle = page.getByRole('switch', { name: new RegExp(name) })
  await expect(toggle).toBeChecked()
  await toggle.click()
  await expect.poll(async () => (await node()).data.agentConfig.skills).toEqual([{ skillId: name, enabled: false }])
  await toggle.click()
  await expect.poll(async () => (await node()).data.agentConfig.skills).toEqual([{ skillId: name, enabled: true }])
  await expect(page.getByRole('button', { name: '添加 MCP', exact: true })).toBeDisabled()
  await expect(page.getByText('请先在设置中安装 MCP。已有绑定可保留并启用。')).toBeVisible()
  await expect(page.getByText(/^已实时保存/)).toBeVisible()
  await page.screenshot({ path: info.outputPath('workflow-imported-skill.png'), animations: 'disabled' })
})
