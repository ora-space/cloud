import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { beforeEach, expect, it, vi } from 'vitest'
import type { WorkflowForm } from '@/lib/workflows/api/generated'
import { RunForm } from './run-form'

const api = vi.hoisted(() => ({ read: vi.fn(), save: vi.fn(), submit: vi.fn(), retry: vi.fn() }))
vi.mock('@/lib/workflows/api/generated', () => ({ getWorkflowForm: api.read, saveWorkflowFormDraft: api.save, submitWorkflowForm: api.submit, retryWorkflowFormTasks: api.retry }))
vi.mock('@/components/task-assignee-picker', () => ({ TaskAssigneePicker: () => <span>负责人选择</span> }))
vi.mock('@/api/members/members', () => ({ useGetApiV1TenantsTidMembers: () => ({ data: { items: [{ userId: 'admin', displayName: '管理员', status: 'active' }] } }) }))
vi.mock('@/lib/squads-api/generated', () => ({ listSquads: async () => ({ items: [] }) }))
vi.mock('./dts-receipt', () => ({ DtsReceipt: () => null }))

let form: WorkflowForm
beforeEach(() => {
  vi.clearAllMocks()
  form = {
    id: 'form', runId: 'run', nodeId: 'review', reviewerId: 'reviewer', version: 1, status: 'open', canEdit: true,
    report: '报告正文', submittedBy: '', results: [], answers: [],
    config: { title: '确认问题', submitLabel: '提交', reportSelector: ['report', 'output'], itemsSelector: ['report', 'findings'], fields: [{ name: 'notes', label: '备注', type: 'textarea', required: false }] },
    findings: [{ key: 'disk', title: '磁盘异常', description: '检查占用', evidence: '86%', severity: '中', module: '存储' }],
  }
  api.read.mockImplementation(async () => structuredClone(form))
})

function mount(runStatus = 'awaiting_input') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  const view = render(<QueryClientProvider client={client}><MemoryRouter><RunForm tenantId="tenant" runId="run" runStatus={runStatus} /></MemoryRouter></QueryClientProvider>)
  return { client, view }
}

/** The decision control reads as one labelled group of two buttons. */
function decide(label: '是问题' | '不是问题') {
  const group = screen.getAllByRole('group', { name: '是否为问题' })[0]!
  fireEvent.click(within(group).getByRole('button', { name: label }))
}

it('renders authoritative submitted answers without losing an unsaved local draft', async () => {
  const { client } = mount()
  await screen.findByText('磁盘异常')
  decide('是问题')
  form = { ...form, version: 2, status: 'submitted', canEdit: false, submittedBy: 'admin', answers: [{ key: 'disk', decision: 'rejected', values: { notes: '管理员判定无需处理' } }] }
  await act(async () => { await client.invalidateQueries({ queryKey: ['workflow-form', 'tenant', 'run'] }) })
  const card = await screen.findByRole('article', { name: '磁盘异常' })
  await waitFor(() => expect(within(card).getByText('不是问题')).toBeInTheDocument())
  expect(screen.getByText('管理员判定无需处理')).toBeInTheDocument()
  expect(screen.getByText('查看未保存的本地修改')).toBeInTheDocument()
  expect(screen.getByText(/磁盘异常：是问题/)).toBeInTheDocument()
  expect(api.submit).not.toHaveBeenCalled()
})

it('refreshes a clean draft when another reviewer saves a newer version', async () => {
  const { client } = mount()
  await screen.findByText('磁盘异常')
  form = { ...form, version: 2, answers: [{ key: 'disk', decision: 'rejected', values: { notes: '最新草稿' } }] }
  await act(async () => { await client.invalidateQueries({ queryKey: ['workflow-form', 'tenant', 'run'] }) })
  await waitFor(() => expect(screen.getByText('最新草稿')).toBeInTheDocument())
  expect(screen.queryByText('查看未保存的本地修改')).not.toBeInTheDocument()
})

it('offers task retry from the submitted answers despite a conflicting local rejection', async () => {
  const { client } = mount('failed')
  await screen.findByText('磁盘异常')
  decide('不是问题')
  form = { ...form, version: 2, status: 'submitted', canEdit: false, submittedBy: 'admin', answers: [{ key: 'disk', decision: 'confirmed', assignee: { type: 'human', id: 'admin' }, values: {} }] }
  await act(async () => { await client.invalidateQueries({ queryKey: ['workflow-form', 'tenant', 'run'] }) })
  await waitFor(() => expect(screen.getByRole('button', { name: '重试任务创建' })).toBeEnabled())
  expect(screen.queryByText(/无需处理，运行已完成/)).not.toBeInTheDocument()
  expect(within(screen.getByRole('article', { name: '磁盘异常' })).getByText('是问题')).toBeInTheDocument()
})

it('sends only the assignee identity when reusing an existing task', async () => {
  form = {
    ...form,
    existingTasks: [{ key: 'disk', taskId: 'task-1', status: 'pending', assignee: { type: 'human', id: 'admin', displayName: '管理员' } }],
  }
  api.submit.mockResolvedValueOnce({ version: 2 })
  mount()
  await screen.findByText('磁盘异常')
  decide('是问题')
  fireEvent.click(screen.getByRole('button', { name: '提交' }))
  await waitFor(() => expect(api.submit).toHaveBeenCalled())
  expect(api.submit.mock.calls[0]![2].answers).toEqual([
    { key: 'disk', decision: 'confirmed', assignee: { type: 'human', id: 'admin' }, values: {} },
  ])
})

it('prefills the confirming member as owner so one click completes a row', async () => {
  form = { ...form, reviewerId: 'admin' }
  api.submit.mockResolvedValueOnce({ version: 2 })
  mount()
  await screen.findByText('磁盘异常')
  decide('是问题')
  expect(screen.getByText('将新建 1 个任务')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '提交' }))
  await waitFor(() => expect(api.submit).toHaveBeenCalled())
  expect(api.submit.mock.calls[0]![2].answers[0].assignee).toEqual({ type: 'human', id: 'admin' })
})

it('marks every finding at once and can undo the batch', async () => {
  form = {
    ...form,
    reviewerId: 'admin',
    findings: [
      { key: 'disk', title: '磁盘异常', description: '', evidence: '', severity: '高', module: '存储' },
      { key: 'api', title: '接口错误率', description: '', evidence: '', severity: '中', module: 'API' },
    ],
  }
  mount()
  await screen.findByText('磁盘异常')
  fireEvent.click(screen.getByRole('button', { name: '全部标为问题' }))
  expect(screen.getByText('将新建 2 个任务')).toBeInTheDocument()
  const cards = screen.getAllByRole('article')
  expect(cards).toHaveLength(2)
  for (const card of cards) expect(within(card).getByText('是问题')).toBeInTheDocument()
  fireEvent.click(screen.getByRole('button', { name: '撤销' }))
  expect(screen.getByText('还有 2 项待确认')).toBeInTheDocument()
})

it('asks for one confirmation when the run found nothing', async () => {
  form = { ...form, findings: [] }
  api.submit.mockResolvedValueOnce({ version: 2 })
  mount()
  expect(await screen.findByText('本次未发现问题')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '确认无问题并继续' })).toBeEnabled()
  expect(screen.queryByRole('button', { name: '全部标为问题' })).not.toBeInTheDocument()
})

it('explains a missing form instead of asking for a refresh when the node failed', async () => {
  api.read.mockRejectedValueOnce(new Error('not found'))
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } })
  render(<QueryClientProvider client={client}><MemoryRouter><RunForm tenantId="tenant" runId="run" runStatus="failed" nodeError="表单处理失败，请核对数据与负责人后重试" /></MemoryRouter></QueryClientProvider>)
  expect(await screen.findByText('确认表单没有生成')).toBeInTheDocument()
  expect(screen.getByText('表单处理失败，请核对数据与负责人后重试')).toBeInTheDocument()
})
