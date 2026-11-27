import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { expect, it, vi } from 'vitest'
import * as workflowsApi from '@/lib/workflows/api/generated'
import { isTerminalActionRejection, reasonText, scheduleError } from './runtime'
import { ScheduleForm } from './schedule-form'

vi.mock('@/lib/workflows/api/generated', () => ({ getWorkflow: vi.fn().mockResolvedValue({ published: null }), getWorkflowSnapshot: vi.fn().mockResolvedValue({ snapshot: null }) }))

it('validates weekly scheduling and accepts unscoped chat', async () => {
  const user = userEvent.setup()
  const onSave = vi.fn()
  render(<QueryClientProvider client={new QueryClient()}><ScheduleForm onSave={onSave} onCancel={() => {}} data={{ projects: [] }} /></QueryClientProvider>)
  fireEvent.change(screen.getByLabelText('任务名称'), { target: { value: '每日简报' } })
  fireEvent.change(screen.getByLabelText('任务指令'), { target: { value: '总结资讯' } })
  expect(screen.getByRole('combobox', { name: '关联项目' })).toHaveValue('')
  await user.selectOptions(screen.getByLabelText('重复频率'), 'weekly')
  await user.click(within(screen.getByRole('group', { name: '执行星期' })).getByRole('button', { name: '星期一' }))
  await user.click(screen.getByRole('button', { name: '创建并启用' }))
  expect(onSave).not.toHaveBeenCalled()
  expect(screen.getByText('请至少选择一天')).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: '星期五' }))
  await user.click(screen.getByRole('button', { name: '创建并启用' }))
  expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ kind: 'chat', projectId: '', rule: expect.objectContaining({ days: [5] }) }), expect.any(Object))
})

it('selects an Agent and one of its models independently', async () => {
  const user = userEvent.setup()
  const onSave = vi.fn()
  const data = {
    projects: [{ id: 'project', name: 'Demo', description: '' }],
    models: [{ id: 'direct', name: 'Direct model' }],
    agents: [
      { id: 'official/opencode', name: 'OpenCode', version: '1.0.0', models: [{ id: 'openai', name: 'OpenAI model' }] },
      { id: 'official/claude', name: 'Claude Code', version: '1.0.0', models: [{ id: 'messages', name: 'Anthropic model' }] },
    ],
    available: { projects: true, models: true, agents: true, workflows: false },
  }
  render(<QueryClientProvider client={new QueryClient()}><ScheduleForm onSave={onSave} onCancel={() => {}} data={data} /></QueryClientProvider>)
  fireEvent.change(screen.getByLabelText('任务名称'), { target: { value: '项目任务' } })
  fireEvent.change(screen.getByLabelText('任务指令'), { target: { value: '检查代码' } })
  expect(screen.queryByLabelText('执行器')).not.toBeInTheDocument()
  expect(screen.getByLabelText('Agent')).toBeDisabled()
  await user.selectOptions(screen.getByLabelText('关联项目'), 'project')
  await user.selectOptions(screen.getByLabelText('Agent'), 'official/claude')
  expect(screen.getByRole('option', { name: 'Anthropic model' })).toBeInTheDocument()
  expect(screen.queryByRole('option', { name: 'OpenAI model' })).not.toBeInTheDocument()
  await user.selectOptions(screen.getByLabelText('模型'), 'messages')
  await user.click(screen.getByRole('button', { name: '创建并启用' }))
  expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ agentId: 'official/claude', agent: 'Claude Code', model: 'messages' }), data)
})

it('requires a project for a workflow while keeping projectless chat', async () => {
  const user = userEvent.setup()
  const onSave = vi.fn()
  const data = {
    projects: [{ id: 'project', name: 'Demo', description: '' }],
    workflows: [{ id: 'workflow', name: '周报流程', version: 'v1', snapshotId: 'snapshot' }],
    available: { projects: true, models: true, agents: true, workflows: true },
  }
  render(<QueryClientProvider client={new QueryClient()}><ScheduleForm onSave={onSave} onCancel={() => {}} data={data} /></QueryClientProvider>)
  fireEvent.change(screen.getByLabelText('任务名称'), { target: { value: '定时周报' } })
  await user.click(screen.getByRole('button', { name: /运行工作流/ }))
  await user.selectOptions(screen.getByLabelText('工作流'), 'workflow')
  const project = screen.getByLabelText('关联项目') as HTMLSelectElement
  expect(project).toHaveValue('')
  expect(project.selectedOptions[0]?.textContent).toBe('请选择项目')
  expect(within(project).getByText('请选择项目')).toHaveAttribute('hidden')
  expect(within(project).queryByRole('option', { name: '请选择项目' })).not.toBeInTheDocument()
  expect(within(project).queryByRole('option', { name: '不关联项目' })).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: '创建并启用' }))
  expect(onSave).not.toHaveBeenCalled()
  expect(screen.getByText('工作流要在项目中运行，请选择项目')).toBeInTheDocument()

  await user.selectOptions(project, 'project')
  await user.click(screen.getByRole('button', { name: '创建并启用' }))
  expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ kind: 'workflow', projectId: 'project' }), data)

  await user.click(screen.getByRole('button', { name: /直接对话/ }))
  expect(within(project).getByRole('option', { name: '不关联项目' })).toBeEnabled()
  expect(within(project).getByRole('option', { name: '不关联项目' })).not.toHaveAttribute('hidden')
})

it('lets a workflow explicitly follow the latest release while explaining frozen history', async () => {
 const user=userEvent.setup()
 const onSave=vi.fn()
 const data={projects:[{id:'p',name:'Demo',description:''}],workflows:[{id:'wf',name:'流程',version:'v1',snapshotId:'s1'}],available:{projects:true,models:true,agents:true,workflows:true}}
 render(<QueryClientProvider client={new QueryClient()}><ScheduleForm onSave={onSave} onCancel={()=>{}} data={data}/></QueryClientProvider>)
 await user.type(screen.getByLabelText('任务名称'),'跟随计划')
 await user.click(screen.getByRole('button',{name:/运行工作流/}))
 await user.selectOptions(screen.getByLabelText('工作流'),'wf')
 await user.selectOptions(screen.getByLabelText('关联项目'),'p')
 await user.click(screen.getByRole('button',{name:'跟随最新发布版本'}))
 expect(screen.getByText(/历史运行始终保留当次实际使用的快照/)).toBeInTheDocument()
 await user.click(screen.getByRole('button',{name:'创建并启用'}))
 expect(onSave).toHaveBeenCalledWith(expect.objectContaining({versionPolicy:'latest',workflowId:'wf'}),data)
})

it('shows the personal Skill team visibility hint when a scheduled workflow references personal Skills', async () => {
 const user=userEvent.setup()
 // A fixed schedule freezes the snapshot the catalog lists, so the hint reads that snapshot.
 vi.mocked(workflowsApi.getWorkflowSnapshot).mockResolvedValueOnce({
  snapshot:{
   id:'s-skill',workflowId:'wf-skill',version:'v1',createdAt:0,updatedAt:0,
   graph:JSON.stringify({nodes:[{id:'a1',type:'workflow',position:{x:0,y:0},data:{kind:'agent',title:'审查',agentConfig:{skills:[{skillId:'my-personal-skill',enabled:true}]}}}],edges:[]}),
  },
 } as never)
 const data={tenantId:'tenant',projects:[{id:'p',name:'Demo',description:''}],workflows:[{id:'wf-skill',name:'带技能流程',version:'v1',snapshotId:'s-skill'}],available:{projects:true,models:true,agents:true,workflows:true}}
 render(<QueryClientProvider client={new QueryClient({defaultOptions:{queries:{retry:false}}})}><ScheduleForm onSave={()=>{}} onCancel={()=>{}} data={data}/></QueryClientProvider>)
 await user.click(screen.getByRole('button',{name:/运行工作流/}))
 await user.selectOptions(screen.getByLabelText('工作流'),'wf-skill')
 expect(await screen.findByText('运行时读取发起人同名的个人 Skill，其内容在节点会话中对团队可见。')).toBeInTheDocument()
 expect(workflowsApi.getWorkflowSnapshot).toHaveBeenCalledWith('tenant','wf-skill','s-skill',undefined,expect.anything())
 expect(workflowsApi.getWorkflow).not.toHaveBeenCalled()
 expect(reasonText('workflow_skill_not_found')).toBe('工作流需要的技能不可用，请确认发起人的个人技能已导入或市场技能已下发到项目')
 expect(reasonText('workflow_role_not_found')).toBe('工作流引用的角色在工作区角色目录里不存在，请先在设置的角色页新建同名角色')
 for (const [code, expected] of [
  ['workflow_skill_not_found', '工作流需要的技能不可用，请确认发起人的个人技能已导入或市场技能已下发到项目。'],
  ['workflow_role_not_found', '工作流引用的角色在工作区角色目录里不存在，请先在设置的角色页新建同名角色。'],
  ['workflow_run_not_startable', '工作流不能启动：图里有当前版本还不能执行的节点，或输入不满足开始节点。'],
 ] as const) {
  const err = { isAxiosError: true, response: { status: 400, data: { code, params: { occurrenceId: 'occ-1' } } } }
  expect(scheduleError(err, 'fallback')).toBe(expected)
  expect(isTerminalActionRejection(err)).toBe(true)
 }
})

const personalSkillHint='运行时读取发起人同名的个人 Skill，其内容在节点会话中对团队可见。'
const agentGraph=(skillId?:string,enabled=true)=>JSON.stringify({nodes:[{id:'start',type:'workflow',position:{x:0,y:0},data:{kind:'start'}},{id:'a1',type:'workflow',position:{x:0,y:0},data:{kind:'agent',title:'审查',agentConfig:{skills:skillId?[{skillId,enabled}]:[]}}}],edges:[{source:'start',target:'a1'}]})
const publication=(id:string,workflowId:string,version:string,graph:string)=>({id,workflowId,version,createdAt:0,updatedAt:0,graph})
const pinnedTask=(workflowId:string,snapshotId:string)=>({id:'schedule',name:'固定版本',projectId:'p',target:{kind:'workflow' as const,workflowId,snapshotId,input:'整理'},rule:{kind:'daily' as const,time:'09:00',days:[1],hours:6,timezone:'Asia/Shanghai'},enabled:true,createdAt:'2026-11-01T00:00:00Z',runs:[]})
const catalogWith=(...workflows:{id:string;name:string;version:string;snapshotId:string}[])=>({tenantId:'tenant',projects:[{id:'p',name:'Demo',description:''}],workflows,available:{projects:true,models:true,agents:true,workflows:true}})
/** Renders the form and waits until every query it issued has settled, so an absent hint is a verdict and not a race. */
async function renderSettled(task:ReturnType<typeof pinnedTask>,data:ReturnType<typeof catalogWith>){
 const client=new QueryClient({defaultOptions:{queries:{retry:false}}})
 const view=render(<QueryClientProvider client={client}><ScheduleForm task={task} onSave={()=>{}} onCancel={()=>{}} data={data}/></QueryClientProvider>)
 await waitFor(()=>expect(client.getQueryCache().getAll().map(query=>query.state.status)).not.toContain('pending'))
 return view
}

it('hides the personal Skill hint when the version a save freezes dropped the Skill, though the task is still pinned to one with it', async () => {
 // Editing re-pins a fixed task to the publication the catalog lists, so that graph decides the hint.
 vi.mocked(workflowsApi.getWorkflowSnapshot).mockReset().mockResolvedValue({snapshot:publication('snap-2','wf','v2',agentGraph('my-personal-skill',false))} as never)
 vi.mocked(workflowsApi.getWorkflow).mockReset()
 await renderSettled(pinnedTask('wf','snap-1'),catalogWith({id:'wf',name:'带技能流程',version:'v2',snapshotId:'snap-2'}))
 expect(workflowsApi.getWorkflowSnapshot).toHaveBeenCalledExactlyOnceWith('tenant','wf','snap-2',undefined,expect.anything())
 expect(screen.queryByText(personalSkillHint)).not.toBeInTheDocument()
 expect(screen.getByText(/当前固定的是旧版本，保存后改为生效版本 v2/)).toBeInTheDocument()
})

it('shows the personal Skill hint when the version a save freezes adds the Skill, though the task is pinned to one without it', async () => {
 const user=userEvent.setup()
 vi.mocked(workflowsApi.getWorkflowSnapshot).mockReset().mockResolvedValue({snapshot:publication('snap-2','wf','v2',agentGraph('my-personal-skill'))} as never)
 vi.mocked(workflowsApi.getWorkflow).mockReset().mockResolvedValue({published:publication('snap-2','wf','v2',agentGraph('my-personal-skill'))} as never)
 await renderSettled(pinnedTask('wf','snap-1'),catalogWith({id:'wf',name:'带技能流程',version:'v2',snapshotId:'snap-2'}))
 expect(screen.getByText(personalSkillHint)).toBeInTheDocument()
 expect(workflowsApi.getWorkflowSnapshot).toHaveBeenCalledExactlyOnceWith('tenant','wf','snap-2',undefined,expect.anything())
 // A task following the latest release runs whatever is published at each trigger: the publication decides.
 await user.click(screen.getByRole('button',{name:'跟随最新发布版本'}))
 await waitFor(()=>expect(workflowsApi.getWorkflow).toHaveBeenCalledExactlyOnceWith('tenant','wf',undefined,expect.anything()))
 expect(workflowsApi.getWorkflowSnapshot).toHaveBeenCalledTimes(1)
 expect(screen.getByText(personalSkillHint)).toBeInTheDocument()
})

it('drops the pinned snapshot when another workflow is chosen, so the field names the version a save freezes', async () => {
 const user=userEvent.setup()
 vi.mocked(workflowsApi.getWorkflowSnapshot).mockReset()
  .mockResolvedValueOnce({snapshot:publication('snap-1','wf-a','v1',agentGraph())} as never)
  .mockResolvedValueOnce({snapshot:publication('snap-b2','wf-b','v2',agentGraph('my-personal-skill'))} as never)
 vi.mocked(workflowsApi.getWorkflow).mockReset()
 await renderSettled(pinnedTask('wf-a','snap-1'),catalogWith({id:'wf-a',name:'甲',version:'v1',snapshotId:'snap-1'},{id:'wf-b',name:'乙',version:'v2',snapshotId:'snap-b2'}))
 expect(screen.queryByText(personalSkillHint)).not.toBeInTheDocument()
 expect(screen.getByText(/保存时固定生效版本 v1/)).toBeInTheDocument()
 await user.selectOptions(screen.getByLabelText('工作流'),'wf-b')
 expect(await screen.findByText(personalSkillHint)).toBeInTheDocument()
 expect(workflowsApi.getWorkflowSnapshot).toHaveBeenLastCalledWith('tenant','wf-b','snap-b2',undefined,expect.anything())
 // The snapshot of the workflow left behind is gone from the draft: a save freezes wf-b's publication, and the field says so.
 expect(screen.getByText(/保存时固定生效版本 v2/)).toBeInTheDocument()
 expect(screen.queryByText(/当前固定的是旧版本/)).not.toBeInTheDocument()
})
