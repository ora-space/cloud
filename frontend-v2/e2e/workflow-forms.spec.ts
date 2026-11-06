import { expect, test } from '@playwright/test'
import { signIn, ownerName } from './login.ts'

test('inspection form persists a draft, creates a real task once and survives refresh', async ({ page }) => {
  await page.goto('/chat')
  await signIn(page)
  const tenants = await (await page.request.get('/api/v1/me/tenants')).json()
  const tenant = tenants.items[0].id
  const projects = await (await page.request.get(`/api/v2/tenants/${tenant}/projects`)).json()
  const key = `disk-${crypto.randomUUID()}`
  const graph = {
    nodes: [
      { id: 'start', data: { kind: 'start', inputVariables: [{ name: 'findings', valueType: 'array[object]', value: [{ key, title: '验收磁盘异常', description: '核对并处理磁盘占用', evidence: '86% > 80%', severity: '中', module: '存储' }] }] } },
      { id: 'review', data: { kind: 'form', formConfig: { title: '巡检问题确认', submitLabel: '确认并生成任务', reportSelector: ['start', 'input'], itemsSelector: ['start', 'findings'], fields: [{ name: 'notes', label: '备注', type: 'textarea', required: false }] } } },
      { id: 'tasks', data: { kind: 'createTasks', createTasksConfig: { formNodeId: 'review' } } },
      { id: 'output', data: { kind: 'output', outputs: [{ name: 'tasks', variableSelector: ['tasks', 'structured_output', 'tasks'] }] } },
    ].map((node, index) => ({ ...node, type: 'workflow', position: { x: index * 340, y: 100 }, data: { ...node.data, title: node.id, description: '' } })),
    edges: [['start', 'review'], ['review', 'tasks'], ['tasks', 'output']].map(([source, target]) => ({ id: `${source}-${target}`, source, target })),
  }
  const created = await page.request.post(`/api/v2/tenants/${tenant}/workflows`, { data: { name: `表单验收 ${key}`, graph: JSON.stringify(graph) } })
  expect(created.ok()).toBeTruthy()
  const workflow = (await created.json()).workflow.id
  expect((await page.request.post(`/api/v2/tenants/${tenant}/workflows/${workflow}/publish`, { data: { version: 'v1' } })).ok()).toBeTruthy()
  const response = await page.request.post(`/api/v2/tenants/${tenant}/workflows/${workflow}/runs`, { data: { projectId: projects.items[0].id, input: '巡检报告：发现磁盘异常，需要人工确认。' } })
  expect(response.ok()).toBeTruthy()
  const run = (await response.json()).run.id
  expect((await page.request.post(`/api/v2/tenants/${tenant}/workflow-runs/${run}/start`, { data: {} })).ok()).toBeTruthy()
  await page.goto(`/chat?run=${run}`)
  const form = page.getByRole('region', { name: '报告与问题确认' })
  await expect(form.getByText('验收磁盘异常', { exact: true })).toBeVisible()
  // One click on the visible control, not a select that hides the two real answers.
  await form.getByRole('group', { name: '是否为问题' }).getByRole('button', { name: '是问题', exact: true }).click()
  await form.getByLabel('指派给', { exact: true }).selectOption({ label: ownerName })
  await form.getByRole('button', { name: '添加备注' }).click()
  await form.getByLabel('备注', { exact: true }).fill('请清理并核对增长来源')
  await form.getByRole('button', { name: '保存草稿' }).click()
  await expect(form.getByText('草稿已保存。')).toBeVisible()
  await page.reload()
  await expect(page).toHaveURL(new RegExp(`run=${run}`))
  await expect(form.getByLabel('备注', { exact: true })).toHaveValue('请清理并核对增长来源')
  await form.getByRole('button', { name: '确认并生成任务' }).click()
  await expect(form.getByRole('link', { name: '打开任务', exact: true })).toBeVisible()
  const taskLink = await form.getByRole('link', { name: '打开任务', exact: true }).getAttribute('href')
  await page.reload()
  await expect(form.getByRole('link', { name: '打开任务', exact: true })).toHaveAttribute('href', taskLink!)
  await page.setViewportSize({ width: 390, height: 844 })
  await expect(form.getByText('验收磁盘异常', { exact: true })).toBeVisible()
  await form.getByRole('link', { name: '打开任务', exact: true }).click()
  await expect(page.getByText('请清理并核对增长来源', { exact: false })).toBeVisible()
  await page.getByRole('link', { name: '查看来源巡检报告' }).click()
  await expect(form).toBeVisible()
})
