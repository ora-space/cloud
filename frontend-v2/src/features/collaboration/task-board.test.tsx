import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it, vi } from 'vitest'
import type { MemberListItem, SpacePlugin } from '@/api/generated.schemas'
import type { Project } from '@/lib/projects-api/generated'
import type { SquadSummary } from './api/generated'
import * as projectsApi from '@/lib/projects-api/generated'
import * as api from './tasks-api/generated'
import type { Task } from './tasks-api/generated'
import { TaskBoard } from './task-board'

const roster = vi.hoisted(() => ({ items: [] as MemberListItem[] }))
const spacePlugins = vi.hoisted(() => ({ items: [] as SpacePlugin[] }))
vi.mock('@/api/members/members', () => ({ useGetApiV1TenantsTidMembers: () => ({ data: roster, isLoading: false, isError: false }) }))
vi.mock('@/api/spaces/spaces', () => ({
  useGetApiV1TenantsTidSpaces: () => ({ data: { items: [{ id: 'space-1' }], nextCursor: '' }, isLoading: false, isError: false }),
  useGetApiV1TenantsTidSpacesSpaceIdPlugins: () => ({ data: spacePlugins, isLoading: false, isError: false }),
}))
vi.mock('./tasks-api/generated', () => ({
  listTasks: vi.fn(), getTask: vi.fn(), createTask: vi.fn(), updateTask: vi.fn(), setTaskStatus: vi.fn(), setTaskAssignee: vi.fn(),
}))
vi.mock('@/lib/projects-api/generated', () => ({ getApiV2TenantsTidProjects: vi.fn() }))

const project: Project = {
  id: 'project-1', tenantId: 'tenant', ownerUserId: 'user-me', name: '演示项目',
  repositoryUrl: 'fixture://demo', defaultBranch: 'main', commitId: 'a'.repeat(40), createdAt: '2026-10-01T00:00:00Z',
}
const squads: SquadSummary[] = [{
  id: 'squad-1', name: '产品小队', description: '', charter: '', leaderAgentId: null, archivedAt: null, version: 1,
  createdBy: 'user-me', createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z',
  humanCount: 1, agentCount: 0, maintainerCount: 1,
}]
const task = (overrides: Partial<Task> = {}): Task => ({
  id: 'task-1', tenantId: 'tenant', title: '整理演示数据', description: '把三条示例任务写好', projectId: 'project-1',
  assignee: null, status: 'pending', createdBy: 'user-me', createdByName: '演示用户', version: 1,
  createdAt: '2026-10-30T00:00:00Z', updatedAt: '2026-10-30T00:00:00Z', ...overrides,
})
const agentTask = task({
  id: 'task-2', title: '写调研结论', status: 'running', version: 2,
  assignee: { type: 'agent', id: 'plugin-1', displayName: 'official/ora-space.opencode', identifier: 'official/ora-space.opencode' },
})

function renderBoard() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  return render(<QueryClientProvider client={client}><TaskBoard tenantId="tenant" squads={squads} /></QueryClientProvider>)
}

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open') }
  vi.clearAllMocks()
  roster.items = [{ id: 'user-me', tenantId: 'tenant', userId: 'user-me', displayName: '演示用户', role: 'owner', status: 'active', version: 1 }]
  spacePlugins.items = []
  vi.mocked(projectsApi.getApiV2TenantsTidProjects).mockResolvedValue({ items: [project], nextCursor: '' })
  vi.mocked(api.listTasks).mockResolvedValue({ items: [], nextCursor: '' })
})

it('shows an explicit empty state instead of sample tasks', async () => {
  renderBoard()
  expect(await screen.findByText('还没有任务')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '创建第一个任务' })).toBeInTheDocument()
})

it('lists tasks with status, project and assignee', async () => {
  vi.mocked(api.listTasks).mockResolvedValue({ items: [task(), agentTask], nextCursor: '' })
  renderBoard()
  expect(await screen.findByRole('button', { name: '查看任务 整理演示数据' })).toBeInTheDocument()
  expect(screen.getByText('待处理')).toBeInTheDocument()
  expect(screen.getByText('进行中')).toBeInTheDocument()
  expect(screen.getByText('演示项目 · 未指派')).toBeInTheDocument()
  expect(screen.getByText('演示项目 · official/ora-space.opencode')).toBeInTheDocument()
  // Assigning an agent does not start it; the list says who has to act.
  expect(screen.getByText('Agent 执行需人点开始')).toBeInTheDocument()
})

it('creates a task in the workspace project and opens it', async () => {
  const created = task({ title: '整理演示数据', description: '两行' })
  vi.mocked(api.createTask).mockResolvedValue(created)
  vi.mocked(api.getTask).mockResolvedValue(created)
  renderBoard()
  await screen.findByText('还没有任务')
  await userEvent.click(screen.getByRole('button', { name: '新建任务' }))
  const dialog = await screen.findByRole('dialog')
  // The form waits for a real project rather than inventing one.
  await waitFor(() => expect(within(dialog).getByLabelText('所属项目')).toHaveValue('project-1'))
  fireEvent.change(within(dialog).getByLabelText('任务标题'), { target: { value: '  整理演示数据  ' } })
  fireEvent.change(within(dialog).getByLabelText('描述（可选）'), { target: { value: '两行' } })
  await userEvent.click(within(dialog).getByRole('button', { name: '创建任务' }))
  await waitFor(() => expect(api.createTask).toHaveBeenCalledWith('tenant', { title: '整理演示数据', description: '两行', projectId: 'project-1' }))
  expect(await screen.findByText('任务已创建')).toBeInTheDocument()
  const detail = await screen.findByRole('dialog')
  expect(within(detail).getByText('两行')).toBeInTheDocument()
})

it('moves a task one step at a time with the version it read', async () => {
  vi.mocked(api.listTasks).mockResolvedValue({ items: [task()], nextCursor: '' })
  vi.mocked(api.getTask).mockResolvedValue(task())
  vi.mocked(api.setTaskStatus).mockResolvedValue(task({ status: 'running', version: 2 }))
  renderBoard()
  await userEvent.click(await screen.findByRole('button', { name: '查看任务 整理演示数据' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '开始处理' }))
  await waitFor(() => expect(api.setTaskStatus).toHaveBeenCalledWith('tenant', 'task-1', { status: 'running', version: 1 }))
  expect(await screen.findByText('状态已改为「进行中」')).toBeInTheDocument()
  await waitFor(() => expect(within(dialog).getByText('进行中')).toBeInTheDocument())
  expect(within(dialog).getByRole('button', { name: '提交验收' })).toBeInTheDocument()
})

it('keeps a stale write visible instead of silently retrying', async () => {
  vi.mocked(api.listTasks).mockResolvedValue({ items: [task()], nextCursor: '' })
  vi.mocked(api.getTask).mockResolvedValue(task())
  vi.mocked(api.setTaskStatus).mockRejectedValue({ isAxiosError: true, response: { status: 409, data: { code: 'version_conflict' } } })
  renderBoard()
  await userEvent.click(await screen.findByRole('button', { name: '查看任务 整理演示数据' }))
  await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '开始处理' }))
  expect(await screen.findByText('任务已被其他人修改，请关闭后重新打开。')).toBeInTheDocument()
})

it('assigns a squad without dispatching to its members', async () => {
  vi.mocked(api.listTasks).mockResolvedValue({ items: [task()], nextCursor: '' })
  vi.mocked(api.getTask).mockResolvedValue(task())
  vi.mocked(api.setTaskAssignee).mockResolvedValue(task({ assignee: { type: 'squad', id: 'squad-1', displayName: '产品小队' }, version: 2 }))
  renderBoard()
  await userEvent.click(await screen.findByRole('button', { name: '查看任务 整理演示数据' }))
  const dialog = await screen.findByRole('dialog')
  expect(within(dialog).getByText(/指派给小队只表示这个队负责，不会自动分派给成员/)).toBeInTheDocument()
  fireEvent.change(within(dialog).getByLabelText('指派给'), { target: { value: 'squad:squad-1' } })
  await waitFor(() => expect(api.setTaskAssignee).toHaveBeenCalledWith('tenant', 'task-1', { assignee: { type: 'squad', id: 'squad-1' }, version: 1 }))
  expect(await screen.findByText('已指派给 产品小队')).toBeInTheDocument()
})

it('says plainly that an agent task waits for a person and leaves room for comments', async () => {
  vi.mocked(api.listTasks).mockResolvedValue({ items: [agentTask], nextCursor: '' })
  vi.mocked(api.getTask).mockResolvedValue(agentTask)
  renderBoard()
  await userEvent.click(await screen.findByRole('button', { name: '查看任务 写调研结论' }))
  const dialog = await screen.findByRole('dialog')
  expect(within(dialog).getByText(/指派给 Agent 后需要人点「开始处理」/)).toBeInTheDocument()
  expect(within(dialog).getByText(/真正启动 Agent 执行、跑完自动进入待验收尚未接入/)).toBeInTheDocument()
  expect(within(dialog).getByText(/评论区留在这里/)).toBeInTheDocument()
  expect(within(dialog).getByRole('button', { name: '提交验收' })).toBeInTheDocument()
})
