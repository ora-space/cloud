import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronRight, FlaskConical, Plus, Users } from 'lucide-react'
import { useGetApiV1MeTenants } from '@/api/me/me'
import type { MemberListItem, SpacePlugin } from '@/api/generated.schemas'
import { useUser } from '@/app/auth/user'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { codeOf, statusOf } from '@/lib/api-client'
import { archiveSquad, addSquadMember, createSquad, getSquad, listSquads, removeSquadMember, updateSquad, updateSquadMember, type SquadDetail, type SquadMember } from './api/generated'
import { MemberPicker } from './member-picker'
import { SquadCard } from './squad-card'
import { SquadDetailView } from './squad-detail'
import { PanelDialog } from './panel-dialog'
import { SquadForm, type SquadDraft } from './squad-form'
import { canMaintain, isLeader, squadTotals } from './squad-model'
import { TaskBoard } from './task-board'
import './collaboration.css'

type Panel = { kind: 'create' } | { kind: 'detail'; id: string } | { kind: 'edit'; id: string } | { kind: 'members'; id: string } | null

/**
 * Squads in the current workspace: the list, one squad's detail, and the maintainer's edits.
 * Membership changes belong to the member operations; nothing here grants runtime permissions.
 */
export default function Page() {
  const me = useUser()
  const tenants = useGetApiV1MeTenants()
  const tenantId = tenants.data?.items[0]?.id ?? ''
  const workspaceRole = tenants.data?.items[0]?.role
  const client = useQueryClient()
  const [panel, setPanel] = useState<Panel>(null)
  const [error, setError] = useState('')
  const [toast, setToast] = useState('')
  const squadId = panel && 'id' in panel ? panel.id : ''
  const list = useQuery({ queryKey: ['squads', tenantId], queryFn: () => listSquads(tenantId), enabled: !!tenantId })
  const detail = useQuery({ queryKey: ['squad', tenantId, squadId], queryFn: () => getSquad(tenantId, squadId), enabled: !!tenantId && !!squadId })
  const squad = detail.data
  const items = list.data?.items ?? []
  const totals = squadTotals(items)
  const close = () => { setPanel(null); setError('') }
  const refreshList = () => client.invalidateQueries({ queryKey: ['squads', tenantId] })
  const settle = async (updated: SquadDetail, message: string) => {
    await refreshList()
    client.setQueryData(['squad', tenantId, updated.id], updated)
    setError('')
    setToast(message)
  }
  const create = useMutation({
    mutationFn: (draft: SquadDraft) => createSquad(tenantId, draft),
    onSuccess: async created => { await settle(created, '小队已创建'); setPanel({ kind: 'detail', id: created.id }) },
    onError: e => setError(messageFor(e)),
  })
  const save = useMutation({
    mutationFn: ({ draft, target }: { draft: SquadDraft; target: SquadDetail }) =>
      updateSquad(tenantId, target.id, { ...draft, leaderAgentId: target.leaderAgentId, version: target.version }),
    onSuccess: async updated => { await settle(updated, '修改已保存'); setPanel({ kind: 'detail', id: updated.id }) },
    onError: e => setError(messageFor(e)),
  })
  const archive = useMutation({
    mutationFn: (target: SquadDetail) => archiveSquad(tenantId, target.id, { version: target.version }),
    onSuccess: updated => settle(updated, '小队已归档'),
    onError: e => setError(messageFor(e)),
  })
  const add = useMutation({
    mutationFn: ({ member, role, target }: { member: MemberListItem; role: string; target: SquadDetail }) =>
      addSquadMember(tenantId, target.id, { type: 'human', userId: member.userId, role: role.trim(), maintainer: false, version: target.version }),
    onSuccess: (updated, v) => settle(updated, `已加入 ${v.member.displayName}`),
    onError: e => setError(messageFor(e)),
  })
  const addAgent = useMutation({
    mutationFn: ({ plugin, role, target }: { plugin: SpacePlugin; role: string; target: SquadDetail }) =>
      addSquadMember(tenantId, target.id, { type: 'agent', spaceAgentId: plugin.id, role: role.trim(), maintainer: false, version: target.version }),
    onSuccess: (updated, v) => settle(updated, `已加入 ${v.plugin.identifier}`),
    onError: e => setError(messageFor(e)),
  })
  const remove = useMutation({
    mutationFn: ({ member, target }: { member: SquadMember; target: SquadDetail }) =>
      removeSquadMember(tenantId, target.id, member.id, { version: target.version }),
    onSuccess: (updated, v) => settle(updated, v.member.userId === me.id ? `已退出 ${v.member.displayName}` : `已移除 ${v.member.displayName}`),
    onError: e => setError(messageFor(e)),
  })
  const toggleMaintainer = useMutation({
    mutationFn: ({ member, target }: { member: SquadMember; target: SquadDetail }) =>
      updateSquadMember(tenantId, target.id, member.id, { role: member.role, maintainer: !member.maintainer, version: target.version }),
    onSuccess: (updated, v) => settle(updated, v.member.maintainer ? `已取消 ${v.member.displayName} 的维护者` : `已设 ${v.member.displayName} 为维护者`),
    onError: e => setError(messageFor(e)),
  })
  const setLeader = useMutation({
    mutationFn: ({ member, target }: { member: SquadMember; target: SquadDetail }) =>
      updateSquad(tenantId, target.id, {
        name: target.name, description: target.description, charter: target.charter,
        leaderAgentId: isLeader(target, member) ? null : member.spaceAgentId, version: target.version,
      }),
    onSuccess: (updated, v) => settle(updated, isLeader(v.target, v.member) ? `已清除 leader` : `已设 ${v.member.displayName} 为 leader`),
    onError: e => setError(messageFor(e)),
  })
  const removing = remove.isPending ? remove.variables?.member.id ?? '' : ''
  const updating = (toggleMaintainer.isPending ? toggleMaintainer.variables?.member.id : '') || (setLeader.isPending ? setLeader.variables?.member.id : '')
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(''), 3500); return () => clearTimeout(timer) }, [toast])
  const openCreate = () => { setError(''); setPanel({ kind: 'create' }) }
  const openSquad = (id: string) => { setError(''); setPanel({ kind: 'detail', id }) }

  return <div className="collaboration-feature mc-page">
    <div className="mc-workspace-bar">
      <span className="mc-breadcrumb">工作空间<ChevronRight size={13} />协作</span>
      <Badge variant="outline"><FlaskConical data-icon="inline-start" />编组演示</Badge>
    </div>
    <div className="mc-content">
      <header className="mc-page-header">
        <div>
          <h1>协作</h1>
          <p>任务把活指派给人、Agent 或小队；小队把人和 Agent 编在一起承接工作。</p>
        </div>
      </header>

      <TaskBoard tenantId={tenantId} squads={items} />

      <section className="mc-list-section mc-squad-section" aria-label="小队列表">
        <header className="mc-section-head">
          <h2>小队 <Badge variant="secondary">{items.length}</Badge></h2>
          <Button onClick={openCreate} disabled={!tenantId || list.isLoading || list.isError}><Plus data-icon="inline-start" />新建小队</Button>
        </header>

        <div className="mc-summary" aria-label="编组概览">
          <div className="mc-summary-counts">
            <span><i className="mc-dot human" />人类成员 <b>{totals.humans}</b></span>
            <span><i className="mc-dot agent" />Agent 成员 <b>{totals.agents}</b></span>
            <span><i className="mc-dot maintainer" />维护者 <b>{totals.maintainers}</b></span>
          </div>
          <span className="mc-summary-note">编组不等于执行能力</span>
        </div>

        {tenants.isLoading || list.isLoading
          ? <p role="status">正在加载小队…</p>
          : tenants.isError || list.isError
            ? <p role="alert">加载失败，请刷新重试。</p>
            : !tenantId
              ? <p role="alert">没有可用的工作空间。</p>
              : items.length
                ? <ul className="mc-grid">{items.map(item => <li key={item.id}><SquadCard squad={item} onOpen={() => openSquad(item.id)} /></li>)}</ul>
                : <Empty className="min-h-64 border">
                  <EmptyHeader><EmptyMedia variant="icon"><Users /></EmptyMedia><EmptyTitle>还没有小队</EmptyTitle><EmptyDescription>创建一个人和 Agent 协作的小队，把成员编在一起。</EmptyDescription></EmptyHeader>
                  <EmptyContent><Button variant="outline" onClick={openCreate}>创建小队</Button></EmptyContent>
                </Empty>}
        <footer className="mc-list-footer"><span>{items.length} 个小队</span><span>计数只含当前有效成员</span></footer>
      </section>
    </div>

    {toast && <div className="mc-toast" role="status">{toast}</div>}

    {panel?.kind === 'create' && <PanelDialog icon={<Users size={14} />} eyebrow="小队" title="新建小队" subtitle="创建后你会是本队的人类维护者。" onClose={close}>
      <SquadForm submitLabel="创建小队" saving={create.isPending} error={error} onSave={draft => create.mutate(draft)} onCancel={close} />
    </PanelDialog>}

    {panel?.kind === 'edit' && squad && <PanelDialog icon={<Users size={14} />} eyebrow="小队" title={`编辑 ${squad.name}`} subtitle="修改名称、简介与协作说明。" onClose={close}>
      <SquadForm initial={{ name: squad.name, description: squad.description, charter: squad.charter }} submitLabel="保存修改" saving={save.isPending} error={error} onSave={draft => save.mutate({ draft, target: squad })} onCancel={close} />
    </PanelDialog>}

    {panel?.kind === 'detail' && <PanelDialog icon={<Users size={14} />} eyebrow="小队" title={squad?.name ?? '小队详情'} subtitle={squad?.archivedAt ? '已归档的小队只保留历史。' : '成员按类型分开显示；维护者可以编辑或归档。'} onClose={close}>
      {detail.isLoading
        ? <p role="status" className="mc-dialog-status">正在加载小队…</p>
        : detail.isError
          ? <p role="alert" className="mc-dialog-status">小队加载失败，请关闭后重试。</p>
          : squad && <SquadDetailView squad={squad} userId={me.id} canMaintain={canMaintain(squad, me.id, workspaceRole)} archiving={archive.isPending} removing={removing} updating={updating} error={error} onEdit={() => { setError(''); setPanel({ kind: 'edit', id: squad.id }) }} onArchive={() => archive.mutate(squad)} onAddMembers={() => { setError(''); setPanel({ kind: 'members', id: squad.id }) }} onRemove={member => { setError(''); remove.mutate({ member, target: squad }) }} onToggleMaintainer={member => { setError(''); toggleMaintainer.mutate({ member, target: squad }) }} onSetLeader={member => { setError(''); setLeader.mutate({ member, target: squad }) }} />}
    </PanelDialog>}

    {panel?.kind === 'members' && squad && <PanelDialog icon={<Users size={14} />} eyebrow="小队" title={`添加成员到 ${squad.name}`} subtitle="可以加入当前工作区的有效成员或已安装的 Agent；移出小队不影响工作区成员资格。" onClose={close}>
      <MemberPicker squad={squad} tenantId={tenantId} adding={add.isPending || addAgent.isPending} error={error}
        onAdd={(member, role) => { setError(''); add.mutate({ member, role, target: squad }) }}
        onAddAgent={(plugin, role) => { setError(''); addAgent.mutate({ plugin, role, target: squad }) }}
        onClose={() => { setError(''); setPanel({ kind: 'detail', id: squad.id }) }} />
    </PanelDialog>}
  </div>
}

/** A user-facing reason for a failed squad request; the server's codes stay in the logs. */
function messageFor(error: unknown): string {
  switch (statusOf(error)) {
    case 403: return '只有工作区管理员或本队维护者可以修改小队。'
    case 404: return '小队不存在或你已无权查看。'
    case 409: return codeOf(error) === 'squad_archived'
      ? '这个小队已归档，不能再修改。'
      : '小队已被其他人修改，请关闭后重新打开。'
    default: return '操作失败，请重试。'
  }
}
