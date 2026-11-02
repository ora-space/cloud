import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it, vi } from 'vitest'
import { UserContext } from '@/app/auth/user'
import type { MemberListItem, SpacePlugin, User } from '@/api/generated.schemas'
import Page from './page'
import * as api from './api/generated'
import type { SquadDetail, SquadSummary } from './api/generated'

const roster = vi.hoisted(() => ({ items: [] as MemberListItem[] }))
const spacePlugins = vi.hoisted(() => ({ items: [] as SpacePlugin[] }))
vi.mock('@/api/me/me', () => ({ useGetApiV1MeTenants: () => ({ data: { items: [{ id: 'tenant' }] }, isLoading: false, isError: false }) }))
vi.mock('@/api/members/members', () => ({ useGetApiV1TenantsTidMembers: () => ({ data: roster, isLoading: false, isError: false }) }))
vi.mock('@/api/spaces/spaces', () => ({
  useGetApiV1TenantsTidSpaces: () => ({ data: { items: [{ id: 'space-1' }], nextCursor: '' }, isLoading: false, isError: false }),
  useGetApiV1TenantsTidSpacesSpaceIdPlugins: () => ({ data: spacePlugins, isLoading: false, isError: false }),
}))
// The task board has its own tests; the page tests stay on the squad section.
vi.mock('./task-board', () => ({ TaskBoard: () => null }))
vi.mock('./api/generated', () => ({
  listSquads: vi.fn(), getSquad: vi.fn(), createSquad: vi.fn(), updateSquad: vi.fn(), archiveSquad: vi.fn(),
  addSquadMember: vi.fn(), removeSquadMember: vi.fn(), updateSquadMember: vi.fn(),
}))

const me: User = { id: 'user-me', displayName: '演示用户', status: 'active', version: 1, createdAt: '2026-10-01T00:00:00Z', deletedAt: null }
const summary: SquadSummary = {
  id: 'squad-1', name: '产品小队', description: '一起做产品', charter: '上午评审', leaderAgentId: null, archivedAt: null, version: 1,
  createdBy: me.id, createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z', humanCount: 2, agentCount: 1, maintainerCount: 1,
}
const detail: SquadDetail = {
  ...summary, leaderAgentId: 'agent-1', members: [
    { id: 'm1', type: 'human', userId: me.id, spaceAgentId: null, displayName: '演示用户', role: '负责人', maintainer: true, joinedAt: '2026-10-01T00:00:00Z', endedAt: null },
    { id: 'm2', type: 'human', userId: 'user-2', spaceAgentId: null, displayName: '第二人', role: '评审', maintainer: false, joinedAt: '2026-10-02T00:00:00Z', endedAt: null },
    { id: 'm3', type: 'agent', userId: null, spaceAgentId: 'agent-1', displayName: '研究员', role: '调研', maintainer: false, joinedAt: '2026-10-03T00:00:00Z', endedAt: null },
  ],
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } })
  return render(<QueryClientProvider client={client}><UserContext.Provider value={me}><Page /></UserContext.Provider></QueryClientProvider>)
}

const third: MemberListItem = {
  id: 'user-3', tenantId: 'tenant', userId: 'user-3', displayName: '第三人', role: 'member', status: 'active', version: 1,
}

const opencode: SpacePlugin = {
  id: 'plugin-1', spaceId: 'space-1', tenantId: 'tenant', sourceNamespace: 'official', identifier: 'ora-space.opencode',
  kind: 'agent', desiredState: 'installed', desiredVersion: '0.6.11', observedState: 'installed', observedVersion: '0.6.11',
  requestedByUserId: null, installError: null, version: 1, createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z',
}

beforeEach(() => {
  HTMLDialogElement.prototype.showModal = function () { this.setAttribute('open', '') }
  HTMLDialogElement.prototype.close = function () { this.removeAttribute('open') }
  vi.clearAllMocks()
  roster.items = [third]
  spacePlugins.items = []
  vi.mocked(api.listSquads).mockResolvedValue({ items: [summary], nextCursor: '' })
  vi.mocked(api.getSquad).mockResolvedValue(detail)
})

it('shows an explicit empty state instead of sample squads', async () => {
  vi.mocked(api.listSquads).mockResolvedValue({ items: [], nextCursor: '' })
  renderPage()
  expect(await screen.findByText('还没有小队')).toBeInTheDocument()
  expect(screen.getByRole('button', { name: '创建小队' })).toBeInTheDocument()
  expect(screen.queryByText('产品小队')).not.toBeInTheDocument()
})

it('lists live counts and shows typed members with their roles', async () => {
  renderPage()
  expect(await screen.findByRole('button', { name: '查看产品小队' })).toBeInTheDocument()
  expect(screen.getByText(/人类成员/)).toHaveTextContent('人类成员 2')
  expect(screen.getByText(/Agent 成员/)).toHaveTextContent('Agent 成员 1')

  await userEvent.click(screen.getByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  expect(api.getSquad).toHaveBeenCalledWith('tenant', 'squad-1')
  expect(within(dialog).getByText('演示用户')).toBeInTheDocument()
  expect(within(dialog).getByText('第二人')).toBeInTheDocument()
  expect(within(dialog).getByText('研究员')).toBeInTheDocument()
  expect(within(dialog).getByText('负责人')).toBeInTheDocument()
  expect(within(dialog).getByText('调研')).toBeInTheDocument()
  expect(within(dialog).getByText('leader：研究员')).toBeInTheDocument()
  expect(within(dialog).getByText(/不授予私人模型、Git 凭据或运行时权限/)).toBeInTheDocument()
})

it('creates a squad with a trimmed name and opens it', async () => {
  const created = { ...detail, id: 'squad-2', name: '增长小队', description: '', charter: '' }
  vi.mocked(api.createSquad).mockResolvedValue(created)
  vi.mocked(api.getSquad).mockResolvedValue(created)
  renderPage()
  await screen.findByText('产品小队')
  await userEvent.click(screen.getByRole('button', { name: '新建小队' }))
  fireEvent.change(screen.getByLabelText('小队名称'), { target: { value: '  增长小队  ' } })
  fireEvent.change(screen.getByLabelText('简介'), { target: { value: '拉新与留存' } })
  await userEvent.click(screen.getByRole('button', { name: '创建小队' }))
  await waitFor(() => expect(api.createSquad).toHaveBeenCalledWith('tenant', { name: '增长小队', description: '拉新与留存', charter: '' }))
  expect(await screen.findByRole('heading', { name: '增长小队' })).toBeInTheDocument()
})

it('keeps a non-maintainer read-only with a reason', async () => {
  vi.mocked(api.getSquad).mockResolvedValue({ ...detail, members: detail.members.map(m => ({ ...m, maintainer: false })) })
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  expect(within(dialog).getByRole('button', { name: '编辑' })).toBeDisabled()
  expect(within(dialog).getByRole('button', { name: '添加成员' })).toBeDisabled()
  expect(within(dialog).queryByRole('button', { name: '移除演示用户' })).not.toBeInTheDocument()
  expect(within(dialog).getByText('只有本队人类维护者或工作区管理员可以修改。')).toBeInTheDocument()
})

it('adds a workspace member who is not in the squad yet', async () => {
  const joined: SquadDetail = {
    ...detail, version: 2, humanCount: 3, maintainerCount: 1,
    members: [...detail.members, { id: 'm4', type: 'human', userId: 'user-3', spaceAgentId: null, displayName: '第三人', role: '评审', maintainer: false, joinedAt: '2026-10-04T00:00:00Z', endedAt: null }],
  }
  vi.mocked(api.addSquadMember).mockResolvedValue(joined)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '添加成员' }))

  // Only accounts outside the live squad are offered, and the role field travels with the add.
  const picker = await screen.findByRole('dialog')
  const people = picker.querySelector<HTMLElement>('[aria-label="可加入的工作区成员"]')!
  expect(within(people).getByText('第三人')).toBeInTheDocument()
  expect(within(people).queryByText('演示用户')).not.toBeInTheDocument()
  fireEvent.change(within(picker).getByLabelText('职责（可选）'), { target: { value: '  评审  ' } })
  await userEvent.click(within(people).getByRole('button', { name: '加入' }))
  await waitFor(() => expect(api.addSquadMember).toHaveBeenCalledWith('tenant', 'squad-1', { type: 'human', userId: 'user-3', role: '评审', maintainer: false, version: 1 }))
  expect(await screen.findByText('已加入 第三人')).toBeInTheDocument()
  // The new member is no longer a candidate, and the detail behind the picker shows them.
  expect(within(picker).queryByRole('button', { name: '加入' })).not.toBeInTheDocument()
  await userEvent.click(within(picker).getByRole('button', { name: '完成' }))
  const after = await screen.findByRole('dialog')
  expect(within(after).getByText('第三人')).toBeInTheDocument()
})

it('offers an installed space agent and joins it as an agent member', async () => {
  spacePlugins.items = [opencode]
  const joined: SquadDetail = {
    ...detail, version: 2, agentCount: 2,
    members: [...detail.members, { id: 'm4', type: 'agent', userId: null, spaceAgentId: 'plugin-1', displayName: 'official/ora-space.opencode', role: '实现', maintainer: false, joinedAt: '2026-10-04T00:00:00Z', endedAt: null }],
  }
  vi.mocked(api.addSquadMember).mockResolvedValue(joined)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '添加成员' }))

  const picker = await screen.findByRole('dialog')
  const agents = picker.querySelector<HTMLElement>('[aria-label="可加入的空间 Agent"]')!
  expect(within(agents).getByText('official/ora-space.opencode')).toBeInTheDocument()
  expect(within(agents).getByText('已安装 0.6.11')).toBeInTheDocument()
  fireEvent.change(within(picker).getByLabelText('职责（可选）'), { target: { value: '实现' } })
  await userEvent.click(within(agents).getByRole('button', { name: '加入' }))
  await waitFor(() => expect(api.addSquadMember).toHaveBeenCalledWith('tenant', 'squad-1', { type: 'agent', spaceAgentId: 'plugin-1', role: '实现', maintainer: false, version: 1 }))
  expect(await screen.findByText('已加入 ora-space.opencode')).toBeInTheDocument()
  // The joined agent leaves the candidate list just like a person does.
  expect(within(agents).queryByRole('button', { name: '加入' })).not.toBeInTheDocument()
})

it('removes a live member only after confirmation and keeps the history', async () => {
  const removed: SquadDetail = {
    ...detail, version: 2, humanCount: 1,
    members: detail.members.map(m => (m.id === 'm2' ? { ...m, maintainer: false, endedAt: '2026-10-30T00:00:00Z' } : m)),
  }
  vi.mocked(api.removeSquadMember).mockResolvedValue(removed)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '移除第二人' }))
  expect(api.removeSquadMember).not.toHaveBeenCalled()
  await userEvent.click(within(dialog).getByRole('button', { name: '确认移除' }))
  await waitFor(() => expect(api.removeSquadMember).toHaveBeenCalledWith('tenant', 'squad-1', 'm2', { version: 1 }))
  expect(await screen.findByText('已移除 第二人')).toBeInTheDocument()
  expect(within(dialog).getByText('已退出 1')).toBeInTheDocument()
})

it('lets a member exit their own squad without being a maintainer', async () => {
  const meMember = { id: 'm1', type: 'human' as const, userId: me.id, spaceAgentId: null, displayName: '演示用户', role: '', maintainer: false, joinedAt: '2026-10-01T00:00:00Z', endedAt: null }
  const otherMaintainer = { id: 'm2', type: 'human' as const, userId: 'user-2', spaceAgentId: null, displayName: '第二人', role: '', maintainer: true, joinedAt: '2026-10-02T00:00:00Z', endedAt: null }
  const nonMaintainer: SquadDetail = { ...detail, leaderAgentId: null, humanCount: 2, maintainerCount: 1, members: [meMember, otherMaintainer] }
  const exited: SquadDetail = { ...nonMaintainer, version: 2, humanCount: 1, members: [otherMaintainer, { ...meMember, endedAt: '2026-10-30T00:00:00Z' }] }
  vi.mocked(api.getSquad).mockResolvedValue(nonMaintainer)
  vi.mocked(api.removeSquadMember).mockResolvedValue(exited)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  // A non-maintainer cannot edit but can still leave their own membership.
  expect(within(dialog).getByRole('button', { name: '编辑' })).toBeDisabled()
  await userEvent.click(within(dialog).getByRole('button', { name: '退出演示用户' }))
  await userEvent.click(within(dialog).getByRole('button', { name: '确认退出' }))
  await waitFor(() => expect(api.removeSquadMember).toHaveBeenCalledWith('tenant', 'squad-1', 'm1', { version: 1 }))
  expect(await screen.findByText('已退出 演示用户')).toBeInTheDocument()
})

it('lets a maintainer toggle another human maintainer mark', async () => {
  const promoted: SquadDetail = { ...detail, version: 2, maintainerCount: 2, members: detail.members.map(m => (m.id === 'm2' ? { ...m, maintainer: true } : m)) }
  vi.mocked(api.updateSquadMember).mockResolvedValue(promoted)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '设为维护者' }))
  await waitFor(() => expect(api.updateSquadMember).toHaveBeenCalledWith('tenant', 'squad-1', 'm2', { role: '评审', maintainer: true, version: 1 }))
  expect(await screen.findByText('已设 第二人 为维护者')).toBeInTheDocument()
})

it('lets a maintainer set an agent as leader', async () => {
  const noLeader: SquadDetail = { ...detail, leaderAgentId: null }
  const led: SquadDetail = { ...noLeader, version: 2, leaderAgentId: 'agent-1' }
  vi.mocked(api.getSquad).mockResolvedValue(noLeader)
  vi.mocked(api.updateSquad).mockResolvedValue(led)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '设为 leader' }))
  await waitFor(() => expect(api.updateSquad).toHaveBeenCalledWith('tenant', 'squad-1', { name: '产品小队', description: '一起做产品', charter: '上午评审', leaderAgentId: 'agent-1', version: 1 }))
  expect(await screen.findByText('已设 研究员 为 leader')).toBeInTheDocument()
})

it('archives only after confirmation and then shows read-only history', async () => {
  const archived: SquadDetail = {
    ...detail, archivedAt: '2026-10-30T00:00:00Z', humanCount: 0, agentCount: 0, maintainerCount: 0,
    members: detail.members.map(m => ({ ...m, maintainer: false, endedAt: '2026-10-30T00:00:00Z' })),
  }
  vi.mocked(api.archiveSquad).mockResolvedValue(archived)
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '归档' }))
  expect(within(dialog).getByText('归档后不能恢复，全部有效编组会结束。')).toBeInTheDocument()
  expect(api.archiveSquad).not.toHaveBeenCalled()
  await userEvent.click(within(dialog).getByRole('button', { name: '确认归档' }))
  await waitFor(() => expect(api.archiveSquad).toHaveBeenCalledWith('tenant', 'squad-1', { version: 1 }))
  expect(await within(dialog).findByText('已归档 · 只读')).toBeInTheDocument()
  expect(within(dialog).queryByRole('button', { name: '编辑' })).not.toBeInTheDocument()
})

it('reports a version conflict when another change lands first', async () => {
  vi.mocked(api.updateSquad).mockRejectedValue({ isAxiosError: true, response: { status: 409 } })
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '编辑' }))
  // The detail dialog is replaced by the edit dialog, so query the form in the new one.
  const edit = await screen.findByRole('dialog')
  fireEvent.change(within(edit).getByLabelText('小队名称'), { target: { value: '我的修改' } })
  await userEvent.click(within(edit).getByRole('button', { name: '保存修改' }))
  expect(await within(edit).findByRole('alert')).toHaveTextContent('小队已被其他人修改，请关闭后重新打开。')
  expect(within(edit).getByLabelText('小队名称')).toHaveValue('我的修改')
})

it('tells an archived squad apart from a version conflict', async () => {
  vi.mocked(api.archiveSquad).mockRejectedValue({ isAxiosError: true, response: { status: 409, data: { code: 'squad_archived' } } })
  renderPage()
  await userEvent.click(await screen.findByRole('button', { name: '查看产品小队' }))
  const dialog = await screen.findByRole('dialog')
  await userEvent.click(within(dialog).getByRole('button', { name: '归档' }))
  await userEvent.click(within(dialog).getByRole('button', { name: '确认归档' }))
  expect(await within(dialog).findByRole('alert')).toHaveTextContent('这个小队已归档，不能再修改。')
})
