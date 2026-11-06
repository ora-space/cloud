import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useParams, useSearchParams } from 'react-router'
import { ChevronLeft, ClipboardList, ListTree, Pencil } from 'lucide-react'
import { useUser } from '@/app/auth/user'
import { Button } from '@/components/ui/button'
import { getApiV2TenantsTidProjects } from '@/lib/projects-api/generated'
import { listSquads } from '@/lib/squads-api/generated'
import { PanelDialog } from '@/components/panel/panel-dialog'
import { RunPane } from './run-pane'
import { TaskComposer, type CommentDraft } from './task-composer'
import { DecomposeDialog, TaskFamily } from './task-decomposition'
import { TaskDetailView } from './task-detail'
import { TaskDiscussion } from './task-discussion'
import { TaskForm, type TaskDraft } from './task-form'
import { TaskWorkflows } from './task-workflows'
import { projectLabel, taskMessageFor, taskMoveMessageFor, taskRunMessageFor, taskStatusLabel } from './task-model'
import {
  createTaskComment, getTask, getTaskTimeline, listTaskExecutors, setTaskAssignee, setTaskStatus, startTaskRun, stopTaskRun, updateTask,
  type Task, type TaskAssigneeRef, type TaskStatusProperty,
} from './tasks-api/generated'
import { canCloseTask, canDecompose, runsAgent } from './task-views'
import { useDecompositions, useMySquadTasks, useWorkspace } from './workspace'

/** Fresh idempotency key for one submission: a retry of the same click reuses it. */
const keyed = () => ({ headers: { 'Idempotency-Key': crypto.randomUUID() } })

/**
 * One task as the team works on it: the Agent's runs, live, beside the team's discussion, one
 * input for both, and the task's owner, execution and status on the side. The page reads the task
 * and its timeline again every few seconds while a run is live, since nothing pushes changes.
 */
export default function TaskPage() {
  const { taskId = '' } = useParams()
  const me = useUser()
  const { tenantId, role } = useWorkspace()
  const client = useQueryClient()
  const [params, setParams] = useSearchParams()
  const rawTab = params.get('tab')
  const tab = rawTab === 'discussion' || rawTab === 'workflows' ? rawTab : 'runs'
  const pickedRun=params.get("run")
  const [error, setError] = useState('')
  const [composeError, setComposeError] = useState('')
  const [editing, setEditing] = useState(false)
  const [decomposing, setDecomposing] = useState(false)
  const [pickedExecutor, setExecutorId] = useState('')
  const live = (task?: Task) => !!task?.activeRun
  const taskQuery = useQuery({
    queryKey: ['task', tenantId, taskId], queryFn: () => getTask(tenantId, taskId), enabled: !!tenantId && !!taskId,
    refetchInterval: q => (q.state.data?.activeRun?.state === 'running' ? 2000 : 10000),
  })
  const task = taskQuery.data
  const timeline = useQuery({
    queryKey: ['task-timeline', tenantId, taskId], queryFn: () => getTaskTimeline(tenantId, taskId), enabled: !!tenantId && !!taskId,
    refetchInterval: live(task) ? 3000 : 10000,
  })
  const projects = useQuery({ queryKey: ['projects', tenantId], queryFn: () => getApiV2TenantsTidProjects(tenantId, { limit: 100 }), enabled: !!tenantId })
  const squads = useQuery({ queryKey: ['squads', tenantId], queryFn: () => listSquads(tenantId), enabled: !!tenantId })
  const inSquad = useMySquadTasks(tenantId).has(taskId)
  const runnable = !!task && runsAgent(task, me.id, inSquad)
  const executors = useQuery({
    queryKey: ['task-executors', tenantId, taskId, task?.assignee?.id], queryFn: () => listTaskExecutors(tenantId, taskId), enabled: !!tenantId && runnable,
  })
  const executorList = executors.data?.items ?? []
  // Until someone picks, the Agent's configured model, as the server defaults, before its fixture.
  const executorId = executorList.some(x => x.id === pickedExecutor)
    ? pickedExecutor
    : (executorList.find(x => !x.id.endsWith(':fixture')) ?? executorList[0])?.id ?? ''

  // The task is read most often; when its run changes state, the timeline and the sidebar's counts
  // follow at once instead of on their own slower beat.
  const runState = task ? `${task.activeRun?.id ?? ''}:${task.activeRun?.state ?? ''}:${task.status}` : ''
  useEffect(() => {
    if (!runState) return
    void client.invalidateQueries({ queryKey: ['task-timeline', tenantId, taskId] })
    void client.invalidateQueries({ queryKey: ['tasks', tenantId] })
  }, [runState, client, tenantId, taskId])
  const decompositions = useDecompositions(tenantId, taskId)
  const splitting = decompositions.latest?.status === 'running'
  const runs = timeline.data?.runs ?? []
  const comments = timeline.data?.comments ?? []
  const selected = pickedRun ? runs.find(r=>r.id===pickedRun) : runs.at(-1)
  // A link to a run the task no longer lists falls back to the list.
  const openedWorkflow = tab === 'workflows' ? decompositions.items.find(d => d.workflowRunId && d.workflowRunId === params.get('workflow')) : undefined
  const refresh = async () => {
    await Promise.all([
      client.invalidateQueries({ queryKey: ['task', tenantId, taskId] }),
      client.invalidateQueries({ queryKey: ['task-timeline', tenantId, taskId] }),
      client.invalidateQueries({ queryKey: ['tasks', tenantId] }),
      client.invalidateQueries({ queryKey: ['my-squad-tasks', tenantId] }),
    ])
  }
  const showTab = (next: 'runs' | 'discussion', id?:string) => {
    const p = new URLSearchParams(params)
    p.delete('workflow')
    if(id)p.set('run',id)
    if (next === 'runs') p.delete('tab')
    else p.set('tab', next)
    setParams(p, { replace: true })
  }
  const showRun = (id: string) => showTab('runs',id)
  const showWorkflows = (runId?: string) => {
    const p = new URLSearchParams(params)
    p.set('tab', 'workflows')
    if (runId) p.set('workflow', runId)
    else p.delete('workflow')
    setParams(p, { replace: true })
  }

  const settle = (updated: Task) => {
    client.setQueryData(['task', tenantId, updated.id], updated)
    setError('')
    void client.invalidateQueries({ queryKey: ['tasks', tenantId] })
  }
  const save = useMutation({
    mutationFn: ({ draft, target }: { draft: TaskDraft; target: Task }) => updateTask(tenantId, target.id, { ...draft, version: target.version }),
    onSuccess: updated => { settle(updated); setEditing(false) },
    onError: e => setError(taskMessageFor(e)),
  })
  const move = useMutation({
    mutationFn: ({ status, target }: { status: TaskStatusProperty; target: Task }) => setTaskStatus(tenantId, target.id, { status, version: target.version }),
    onSuccess: updated => { settle(updated); void refresh() },
    onError: e => setError(taskMoveMessageFor(e)),
  })
  const assign = useMutation({
    mutationFn: ({ assignee, target }: { assignee: TaskAssigneeRef | null; target: Task }) => setTaskAssignee(tenantId, target.id, { assignee, version: target.version }),
    onSuccess: updated => { settle(updated); void client.invalidateQueries({ queryKey: ['my-squad-tasks', tenantId] }) },
    onError: e => setError(taskMessageFor(e)),
  })
  const start = useMutation({
    mutationFn: () => startTaskRun(tenantId, taskId, { executorId }, keyed()),
    onSuccess: async run => { setError(''); showRun(run.id); await refresh() },
    onError: e => setError(taskRunMessageFor(e)),
  })
  const stop = useMutation({
    mutationFn: (runId: string) => stopTaskRun(tenantId, taskId, runId),
    onSuccess: async () => { setError(''); await refresh() },
    onError: e => setError(taskRunMessageFor(e)),
  })
  const comment = useMutation({
    mutationFn: (draft: CommentDraft) => createTaskComment(tenantId, taskId, { ...draft, executorId: draft.toAgent ? executorId : undefined }, keyed()),
    onSuccess: async (created, draft) => {
      setComposeError('')
      // Replying answers any @ waiting on the caller here.
      void client.invalidateQueries({ queryKey: ['task-mentions', tenantId] })
      if (draft.toAgent && created.runId) showRun(created.runId)
      else showTab('discussion')
      await refresh()
    },
    onError: e => setComposeError(taskRunMessageFor(e)),
  })

  if (taskQuery.isLoading || !tenantId) return <div className="mc-content"><p role="status">正在加载任务…</p></div>
  if (taskQuery.isError || !task) {
    return <div className="mc-content">
      <p role="alert">任务不存在或你已无权查看。</p>
      <Link to="/collaboration" className="mc-link">返回任务列表</Link>
    </div>
  }
  const projectList = projects.data?.items ?? []
  return <div className="mc-task-page">
    <header className="mc-task-bar">
      <Link to="/collaboration" className="mc-back" aria-label="返回任务列表"><ChevronLeft size={16} /></Link>
      <span className={`mc-status ${task.status}`}>{taskStatusLabel(task.status)}</span>
      <h2>{task.title}</h2>
      {canDecompose(task) && <Button type="button" variant="outline" size="sm" onClick={() => setDecomposing(true)} disabled={splitting}
        title={splitting ? '正在拆分，跑完后才能再拆' : '用工作流把这个任务拆成子任务'}><ListTree data-icon="inline-start" />{splitting ? '拆分中…' : '拆分'}</Button>}
      <Button type="button" variant="outline" size="sm" onClick={() => { setError(''); setEditing(true) }} disabled={save.isPending}><Pencil data-icon="inline-start" />编辑</Button>
    </header>
    <div className="mc-task-body">
      <section className="mc-task-main-col" aria-label="运行与讨论">
        {task.description && <p className="mc-description">{task.description}</p>}
        {task.workflowSources?.map(source=><p key={source.runId}><Link className="underline" to={`/chat?run=${source.runId}`}>查看来源巡检报告 · {source.name}{source.submittedAt ? ` · ${new Date(source.submittedAt).toLocaleString()}` : ''}</Link></p>)}
        <TaskFamily tenantId={tenantId} task={task} latest={decompositions.latest} folded={!!openedWorkflow} onRetry={() => setDecomposing(true)} />
        <div className="mc-tabs" role="tablist" aria-label="任务内容">
          <button type="button" role="tab" aria-selected={tab === 'runs'} onClick={() => showTab('runs')}>Agent 运行 <b>{runs.length}</b></button>
          {(decompositions.items.length > 0 || tab === 'workflows') && <button type="button" role="tab" aria-selected={tab === 'workflows'} onClick={() => showWorkflows()}>工作流 <b>{decompositions.items.length}</b></button>}
          <button type="button" role="tab" aria-selected={tab === 'discussion'} onClick={() => showTab('discussion')}>讨论 <b>{comments.length}</b></button>
        </div>
        <div className="mc-tab-body">
          {tab === 'workflows'
            ? <TaskWorkflows tenantId={tenantId} decompositions={decompositions.items} selected={openedWorkflow} onSelect={showWorkflows} />
            : timeline.isError
            ? <p role="alert">运行与讨论加载失败，请刷新重试。</p>
            : tab === 'runs'
              ? pickedRun&&!selected&&!timeline.isPending ? <p role="status">这次运行已不存在或不可见。</p> : <RunPane tenantId={tenantId} taskId={task.id} runs={runs} selected={selected} onSelect={showRun} />
              : <TaskDiscussion comments={comments} runs={runs} onRun={showRun} />}
        </div>
        {!openedWorkflow && <TaskComposer tenantId={tenantId} task={task} me={me.id} inSquad={inSquad} sending={comment.isPending} error={composeError}
          onSend={async draft => { try { await comment.mutateAsync(draft); return true } catch { return false } }} />}
      </section>
      <aside className="mc-task-side-col" aria-label="任务属性">
        <TaskDetailView task={task} me={me.id} inSquad={inSquad} canClose={canCloseTask(task, me.id, role)} projectName={projectLabel(projectList, task.projectId)} squads={squads.data?.items ?? []}
          busy={move.isPending || assign.isPending} error={error}
          run={{
            executors: executorList, executorId, onExecutor: setExecutorId, hasRuns: runs.length > 0,
            starting: start.isPending, stopping: stop.isPending,
            onStart: () => { setError(''); start.mutate() },
            onStop: () => { if (task.activeRun) { setError(''); stop.mutate(task.activeRun.id) } },
          }}
          onStatus={status => { setError(''); move.mutate({ status, target: task }) }}
          onAssign={next => { setError(''); assign.mutate({ assignee: next, target: task }) }} />
      </aside>
    </div>

    {decomposing && <DecomposeDialog tenantId={tenantId} task={task} onClose={() => setDecomposing(false)} onStarted={created => { setDecomposing(false); showWorkflows(created.status === 'running' ? created.workflowRunId : undefined) }} />}

    {editing && <PanelDialog icon={<ClipboardList size={14} />} eyebrow="任务" title={`编辑 ${task.title}`} subtitle="修改标题、描述与所属项目；状态与指派有各自的入口。" onClose={() => setEditing(false)}>
      <TaskForm initial={{ title: task.title, description: task.description, projectId: task.projectId }} projects={projectList} projectsLoading={projects.isLoading} submitLabel="保存修改" saving={save.isPending} error={error} onSave={draft => save.mutate({ draft, target: task })} onCancel={() => setEditing(false)} />
    </PanelDialog>}
  </div>
}
