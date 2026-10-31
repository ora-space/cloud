import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ClipboardList, Plus } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from '@/components/ui/empty'
import { getApiV2TenantsTidProjects } from '@/lib/projects-api/generated'
import type { SquadSummary } from './api/generated'
import { PanelDialog } from './panel-dialog'
import { TaskDetailView } from './task-detail'
import { TaskForm, type TaskDraft } from './task-form'
import { assigneeLabel, projectLabel, taskMessageFor, taskStatusLabel } from './task-model'
import { createTask, getTask, listTasks, setTaskAssignee, setTaskStatus, updateTask, type Task, type TaskAssigneeRef, type TaskStatusProperty } from './tasks-api/generated'

type Panel = { kind: 'create' } | { kind: 'detail'; id: string } | { kind: 'edit'; id: string } | null

/**
 * The workspace's tasks: the list the whole team sees, one task's detail, and the writes that
 * create, edit, reassign and move it. Assignment records who owns the work; it dispatches nothing.
 */
export function TaskBoard({ tenantId, squads }: {
  tenantId: string
  /** Live squads of the workspace, offered as assignees. */
  squads: SquadSummary[]
}) {
  const client = useQueryClient()
  const [panel, setPanel] = useState<Panel>(null)
  const [error, setError] = useState('')
  const [toast, setToast] = useState('')
  const taskId = panel && 'id' in panel ? panel.id : ''
  const list = useQuery({ queryKey: ['tasks', tenantId], queryFn: () => listTasks(tenantId), enabled: !!tenantId })
  const projects = useQuery({ queryKey: ['projects', tenantId], queryFn: () => getApiV2TenantsTidProjects(tenantId, { limit: 100 }), enabled: !!tenantId })
  const detail = useQuery({ queryKey: ['task', tenantId, taskId], queryFn: () => getTask(tenantId, taskId), enabled: !!tenantId && !!taskId })
  const items = list.data?.items ?? []
  const projectList = projects.data?.items ?? []
  const task = detail.data
  const close = () => { setPanel(null); setError('') }
  const refresh = () => client.invalidateQueries({ queryKey: ['tasks', tenantId] })
  const settle = async (updated: Task, message: string) => {
    await refresh()
    client.setQueryData(['task', tenantId, updated.id], updated)
    setError('')
    setToast(message)
  }
  const create = useMutation({
    mutationFn: (draft: TaskDraft) => createTask(tenantId, draft),
    onSuccess: async created => {
      await refresh()
      client.setQueryData(['task', tenantId, created.id], created)
      setError('')
      setToast('任务已创建')
      setPanel({ kind: 'detail', id: created.id })
    },
    onError: e => setError(taskMessageFor(e)),
  })
  const save = useMutation({
    mutationFn: ({ draft, target }: { draft: TaskDraft; target: Task }) => updateTask(tenantId, target.id, { ...draft, version: target.version }),
    onSuccess: async updated => { await settle(updated, '修改已保存'); setPanel({ kind: 'detail', id: updated.id }) },
    onError: e => setError(taskMessageFor(e)),
  })
  const move = useMutation({
    mutationFn: ({ status, target }: { status: TaskStatusProperty; target: Task }) => setTaskStatus(tenantId, target.id, { status, version: target.version }),
    onSuccess: (updated, v) => settle(updated, `状态已改为「${taskStatusLabel(v.status)}」`),
    onError: e => setError(taskMessageFor(e)),
  })
  const assign = useMutation({
    mutationFn: ({ assignee, target }: { assignee: TaskAssigneeRef | null; target: Task }) => setTaskAssignee(tenantId, target.id, { assignee, version: target.version }),
    onSuccess: updated => settle(updated, updated.assignee ? `已指派给 ${updated.assignee.displayName}` : '已取消指派'),
    onError: e => setError(taskMessageFor(e)),
  })
  const busy = move.isPending || assign.isPending
  useEffect(() => { if (!toast) return; const timer = setTimeout(() => setToast(''), 3500); return () => clearTimeout(timer) }, [toast])
  const openCreate = () => { setError(''); setPanel({ kind: 'create' }) }
  const openTask = (id: string) => { setError(''); setPanel({ kind: 'detail', id }) }

  return <>
    <section className="mc-list-section mc-task-section" aria-label="任务">
      <header className="mc-section-head">
        <h2>任务 <Badge variant="secondary">{items.length}</Badge></h2>
        <Button onClick={openCreate} disabled={!tenantId || list.isLoading || list.isError}><Plus data-icon="inline-start" />新建任务</Button>
      </header>

      {list.isLoading
        ? <p role="status">正在加载任务…</p>
        : list.isError
          ? <p role="alert">任务加载失败，请刷新重试。</p>
          : items.length
            ? <ul className="mc-tasks">{items.map(item => <li key={item.id}>
              <button type="button" className="mc-task-row" aria-label={`查看任务 ${item.title}`} onClick={() => openTask(item.id)}>
                <span className={`mc-status ${item.status}`}>{taskStatusLabel(item.status)}</span>
                <span className="mc-task-main">
                  <strong>{item.title}</strong>
                  <small>{projectLabel(projectList, item.projectId)} · {assigneeLabel(item)}</small>
                </span>
                <span className="mc-task-hint">{item.assignee?.type === 'agent' ? 'Agent 执行需人点开始' : item.assignee?.type === 'squad' ? '小队负责，不自动分派' : ''}</span>
              </button>
            </li>)}</ul>
            : <Empty className="min-h-56 border">
              <EmptyHeader><EmptyMedia variant="icon"><ClipboardList /></EmptyMedia><EmptyTitle>还没有任务</EmptyTitle><EmptyDescription>建一个任务，指派给一个人、一个 Agent 或一个小队，由人推进状态。</EmptyDescription></EmptyHeader>
              <EmptyContent><Button variant="outline" onClick={openCreate} disabled={!tenantId}>创建第一个任务</Button></EmptyContent>
            </Empty>}
      <footer className="mc-list-footer"><span>{items.length} 个任务</span><span>指派给小队不等于自动分派</span></footer>
    </section>

    {toast && <div className="mc-toast" role="status">{toast}</div>}

    {panel?.kind === 'create' && <PanelDialog icon={<ClipboardList size={14} />} eyebrow="任务" title="新建任务" subtitle="任务属于一个项目；创建后指派与状态各有入口。" onClose={close}>
      <TaskForm projects={projectList} projectsLoading={projects.isLoading} submitLabel="创建任务" saving={create.isPending} error={error} onSave={draft => create.mutate(draft)} onCancel={close} />
    </PanelDialog>}

    {panel?.kind === 'edit' && task && <PanelDialog icon={<ClipboardList size={14} />} eyebrow="任务" title={`编辑 ${task.title}`} subtitle="修改标题、描述与所属项目；状态与指派有各自的写入。" onClose={close}>
      <TaskForm initial={{ title: task.title, description: task.description, projectId: task.projectId }} projects={projectList} projectsLoading={projects.isLoading} submitLabel="保存修改" saving={save.isPending} error={error} onSave={draft => save.mutate({ draft, target: task })} onCancel={() => { setError(''); setPanel({ kind: 'detail', id: task.id }) }} />
    </PanelDialog>}

    {panel?.kind === 'detail' && <PanelDialog icon={<ClipboardList size={14} />} eyebrow="任务" title={task?.title ?? '任务详情'} subtitle="状态按「待处理 → 进行中 → 待验收 → 已完成」推进；指派给小队只表示这个队负责。" onClose={close}>
      {detail.isLoading
        ? <p role="status" className="mc-dialog-status">正在加载任务…</p>
        : detail.isError
          ? <p role="alert" className="mc-dialog-status">任务加载失败，请关闭后重试。</p>
          : task && <TaskDetailView task={task} projectName={projectLabel(projectList, task.projectId)} squads={squads} busy={busy} error={error}
            onEdit={() => { setError(''); setPanel({ kind: 'edit', id: task.id }) }}
            onStatus={status => { setError(''); move.mutate({ status, target: task }) }}
            onAssign={next => { setError(''); assign.mutate({ assignee: next, target: task }) }} />}
    </PanelDialog>}
  </>
}
