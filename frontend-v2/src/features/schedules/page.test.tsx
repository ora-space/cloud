import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, expect, it, vi } from 'vitest'
import Page from './page'
import * as api from './api/generated'
import * as actions from './api/actions/generated'
import * as history from './api/history/generated'
import { compareOccurrences, compareTimestampsDesc, emptyResources } from './runtime'
import * as workflowRuns from '@/lib/workflows/api/generated'

vi.mock('@/app/auth/user',()=>({useUser:()=>({id:'user'})}))
vi.mock('@/api/me/me',()=>({useGetApiV1MeTenants:()=>({data:{items:[{id:'tenant'}]},isLoading:false,isError:false})}))
vi.mock('./api/generated',()=>({listScheduleConfigs:vi.fn(),createScheduleConfig:vi.fn(),updateScheduleConfig:vi.fn(),setScheduleConfigEnabled:vi.fn(),getScheduleResources:vi.fn()}))
vi.mock('./api/actions/generated',()=>({runScheduleNow:vi.fn(),cancelScheduleRun:vi.fn()}))
vi.mock('./api/history/generated',()=>({listScheduleHistory:vi.fn(),getScheduleHistoryItem:vi.fn(),getScheduleExecutionResult:vi.fn()}))
vi.mock('@/lib/workflows/api/generated',()=>({getWorkflowRun:vi.fn()}))
const resources={projects:[{id:'project-id',name:'真实项目',description:''}],models:[{id:'fixture',name:'固定模型'}],executors:[{id:'executor-id',name:'项目执行器',agentName:'Agent',modelId:'fixture'}],workflows:[{id:'wf-1',name:'周报流程',version:'v2',snapshotId:'snap-2'}],available:{projects:true,models:true,executors:true,workflows:true}}
const item={id:'task-1',name:'早报',projectId:'',target:{kind:'chat' as const,prompt:'摘要',agent:'',model:'fixture'},rule:{kind:'daily' as const,time:'09:00',days:[1],hours:6,timezone:'Asia/Shanghai'},enabled:true,version:1,createdAt:'2026-10-01T00:00:00Z',updatedAt:'2026-10-01T00:00:00Z',runs:[]}
const occurrence:history.Occurrence={id:'run-1',scheduleId:item.id,key:'key',kind:'manual',status:'running',reason:'',executionRef:'ref',scheduledAt:item.createdAt,recordedAt:item.createdAt,configVersion:1,snapshot:{},released:false,version:1}
function mount(client=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}})){return render(<QueryClientProvider client={client}><Page/></QueryClientProvider>)}
async function detail(){await screen.findByText('早报');await userEvent.click(screen.getByRole('button',{name:'查看早报'}));return screen.getByRole('dialog')}
beforeEach(()=>{
 sessionStorage.clear();vi.resetAllMocks()
 HTMLDialogElement.prototype.showModal=function(){this.setAttribute('open','')};HTMLDialogElement.prototype.close=function(){this.removeAttribute('open')}
 vi.mocked(api.listScheduleConfigs).mockResolvedValue({items:[item],nextCursor:''})
 vi.mocked(api.getScheduleResources).mockResolvedValue(resources)
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[],nextCursor:''})
 vi.mocked(history.getScheduleHistoryItem).mockResolvedValue({...occurrence,executionRef:''})
})

it('uses shared real IDs and persists the project/model/executor binding',async()=>{
 vi.mocked(api.createScheduleConfig).mockResolvedValue({...item,name:'项目任务',projectId:'project-id'})
 mount();await screen.findByText('早报');await userEvent.click(screen.getByRole('button',{name:'新建任务'}))
 fireEvent.change(screen.getByLabelText('任务名称'),{target:{value:'项目任务'}});fireEvent.change(screen.getByLabelText('任务指令'),{target:{value:'检查代码'}})
 await userEvent.selectOptions(screen.getByLabelText('关联项目'),'project-id');await userEvent.selectOptions(screen.getByLabelText('模型'),'fixture');await userEvent.selectOptions(screen.getByLabelText('执行器'),'executor-id')
 await userEvent.click(screen.getByRole('button',{name:'创建并启用'}))
 await waitFor(()=>expect(api.createScheduleConfig).toHaveBeenCalledWith('tenant',expect.objectContaining({projectId:'project-id',target:{kind:'chat',prompt:'检查代码',model:'fixture',agent:'Agent',executorId:'executor-id'}})))
 expect(screen.queryByRole('option',{name:'Ora 官网'})).not.toBeInTheDocument()
})

it('creates a workflow schedule in a project, freezing the version published now',async()=>{
 vi.mocked(api.createScheduleConfig).mockResolvedValue({...item,name:'周报',projectId:'project-id',target:{kind:'workflow',workflowId:'wf-1',snapshotId:'snap-2',input:'整理本周'}})
 mount();await screen.findByText('早报');await userEvent.click(screen.getByRole('button',{name:'新建任务'}))
 fireEvent.change(screen.getByLabelText('任务名称'),{target:{value:'周报'}})
 await userEvent.click(screen.getByRole('button',{name:/运行工作流/}))
 // A workflow runs its own Agents: no model or executor to pick.
 expect(screen.queryByLabelText('模型')).not.toBeInTheDocument();expect(screen.queryByLabelText('执行器')).not.toBeInTheDocument()
 await userEvent.click(screen.getByRole('button',{name:'创建并启用'}))
 expect(await screen.findByText('请选择要运行的工作流')).toBeInTheDocument();expect(screen.getByText('工作流要在项目中运行，请选择项目')).toBeInTheDocument()
 await userEvent.selectOptions(screen.getByLabelText('工作流'),'wf-1');await userEvent.selectOptions(screen.getByLabelText('关联项目'),'project-id')
 expect(screen.getByText(/保存时固定生效版本 v2/)).toBeInTheDocument()
 fireEvent.change(screen.getByLabelText('初始输入'),{target:{value:'整理本周'}})
 await userEvent.click(screen.getByRole('button',{name:'创建并启用'}))
 await waitFor(()=>expect(api.createScheduleConfig).toHaveBeenCalledWith('tenant',expect.objectContaining({projectId:'project-id',target:{kind:'workflow',workflowId:'wf-1',snapshotId:'snap-2',input:'整理本周'}})))
})

it("says when a workflow schedule still runs an older version, and shows an occurrence's run",async()=>{
 const scheduled={...item,projectId:'project-id',target:{kind:'workflow' as const,workflowId:'wf-1',snapshotId:'snap-1',input:'整理本周'}}
 vi.mocked(api.listScheduleConfigs).mockResolvedValue({items:[scheduled],nextCursor:''})
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[{...occurrence,status:'succeeded',released:true,executionRef:'{"workflowRunId":"run-9"}'}],nextCursor:''})
 vi.mocked(workflowRuns.getWorkflowRun).mockResolvedValue({run:{id:'run-9',workflowId:'wf-1',snapshotId:'snap-1',projectId:'project-id',name:'早报',status:'succeeded',state:null,input:'整理本周',output:'{"summary":"本周合并了 3 个改动"}',error:null,startedAt:1,finishedAt:2,createdAt:1,updatedAt:2},nodes:[],scopes:[],variables:[],failedAttempts:[]} as never)
 mount();const dialog=await detail()
 expect(within(dialog).getByText('运行工作流')).toBeInTheDocument()
 expect(within(dialog).getByText('周报流程（固定的旧版本；生效版本已是 v2，编辑保存后改用）')).toBeInTheDocument()
 expect(within(dialog).getByRole('button',{name:'立即运行'})).toBeEnabled()
 await userEvent.click(within(dialog).getByRole('button',{name:'运行历史'}))
 expect(await within(dialog).findByText('工作流运行 · 成功')).toBeInTheDocument()
 expect(within(dialog).getByText('{"summary":"本周合并了 3 个改动"}')).toBeInTheDocument()
 expect(workflowRuns.getWorkflowRun).toHaveBeenCalledWith('tenant','run-9',undefined,expect.anything())
 // The schedule's own result route is for conversations; a workflow's result is its run.
 expect(history.getScheduleExecutionResult).not.toHaveBeenCalled()
})

it('shows unavailable resources, keeps reference modes disabled and allows unbound text configuration',async()=>{
 vi.mocked(api.getScheduleResources).mockResolvedValue(emptyResources)
 vi.mocked(api.createScheduleConfig).mockResolvedValue({...item,target:{...item.target,model:''}})
 mount();const dialog=await detail();expect(within(dialog).getByRole('button',{name:'立即运行'})).toBeDisabled()
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}));await userEvent.click(screen.getByRole('button',{name:'新建任务'}))
 expect(screen.getByRole('button',{name:/运行工作流/})).toBeDisabled();expect(screen.getByRole('button',{name:/已有任务/})).toBeDisabled()
 expect(screen.getByLabelText('模型')).toBeDisabled();expect(screen.getByLabelText('执行器')).toBeDisabled()
 fireEvent.change(screen.getByLabelText('任务名称'),{target:{value:'稍后配置'}});fireEvent.change(screen.getByLabelText('任务指令'),{target:{value:'摘要'}})
 await userEvent.click(screen.getByRole('button',{name:'创建并启用'}));await waitFor(()=>expect(api.createScheduleConfig).toHaveBeenCalledWith('tenant',expect.objectContaining({projectId:'',target:expect.objectContaining({model:''})})))
 expect(actions.runScheduleNow).not.toHaveBeenCalled()
})

it('preserves the request identity across a network failure and reopening the drawer',async()=>{
 vi.mocked(actions.runScheduleNow).mockRejectedValueOnce({isAxiosError:true}).mockResolvedValueOnce({id:'run-1',scheduleId:item.id,kind:'manual',status:'running',reason:'',released:false,version:1,scheduledAt:item.createdAt})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'立即运行'}));expect(await screen.findByRole('alert')).toHaveTextContent('网络连接中断')
 const key=vi.mocked(actions.runScheduleNow).mock.calls[0][2].requestId
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}));await detail();await userEvent.click(screen.getByRole('button',{name:'立即运行'}))
 await waitFor(()=>expect(actions.runScheduleNow).toHaveBeenCalledTimes(2))
 expect(vi.mocked(actions.runScheduleNow).mock.calls[1][2].requestId).toBe(key)
 await waitFor(()=>expect(sessionStorage.getItem('ora.schedule.request:user:tenant:task-1')).toBeNull())
})

it('does not claim cancellation when stopping is unconfirmed and shows incomplete output honestly',async()=>{
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[occurrence],nextCursor:''})
 vi.mocked(history.getScheduleExecutionResult).mockResolvedValue({sessionId:'s',turnId:'t',mode:'model',workspaceId:'',sessionEndedAt:null,startedAt:item.createdAt,endedAt:null,result:{status:'failed',answer:'只有部分内容',answerComplete:false,stopReason:'',failureCode:'result_incomplete',usage:null}})
 vi.mocked(actions.cancelScheduleRun).mockRejectedValue({isAxiosError:true,response:{status:503,data:{code:'stop_unconfirmed'}}})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'运行历史'}))
 expect(await screen.findByText('部分结果或未完成结果')).toBeInTheDocument();expect(screen.getByText('只有部分内容')).toBeInTheDocument()
 await userEvent.click(screen.getByRole('button',{name:'取消此次运行'}));expect(await screen.findByRole('alert')).toHaveTextContent('停止尚未确认')
 expect(screen.getByRole('button',{name:'取消此次运行'})).toBeEnabled();expect(screen.queryByText('已取消')).not.toBeInTheDocument();expect(screen.getByRole('button',{name:'立即运行'})).toBeDisabled()
})

it('keeps edits on version conflict and resets the conflict for a new task',async()=>{
 vi.mocked(api.updateScheduleConfig).mockRejectedValue({isAxiosError:true,response:{status:409,data:{code:'version_conflict'}}})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'编辑'}));fireEvent.change(screen.getByLabelText('任务名称'),{target:{value:'我的修改'}})
 await userEvent.click(screen.getByRole('button',{name:'保存修改'}));expect(await screen.findByRole('alert')).toHaveTextContent('已被其他操作修改');expect(screen.getByLabelText('任务名称')).toHaveValue('我的修改');expect(screen.getByRole('button',{name:'保存修改'})).toBeDisabled()
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}));await userEvent.click(screen.getByRole('button',{name:'放弃修改'}));await userEvent.click(screen.getByRole('button',{name:'新建任务'}));expect(screen.getByRole('button',{name:'创建并启用'})).toBeEnabled()
})

it('shows config and history failures instead of sample records or fake success',async()=>{
 vi.mocked(history.listScheduleHistory).mockRejectedValue({isAxiosError:true,response:{status:404}})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'运行历史'}));expect(await screen.findByRole('alert')).toHaveTextContent('资源或接口当前不可用');expect(screen.queryByText('尚无运行记录。')).not.toBeInTheDocument();expect(screen.queryByRole('button',{name:'模拟运行'})).not.toBeInTheDocument()
})

it('refreshes current status and consumes opaque history pagination',async()=>{
 const finished={...occurrence,status:'succeeded' as const,released:true,executionRef:''}
 vi.mocked(history.listScheduleHistory).mockImplementation(async(_tid,_id,params)=>params?.cursor?{items:[{...finished,id:'older'}],nextCursor:''}:{items:[finished],nextCursor:'opaque-page'})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'运行历史'}));await userEvent.click(await screen.findByRole('button',{name:'加载更多记录'}))
 await waitFor(()=>expect(history.listScheduleHistory).toHaveBeenCalledWith('tenant','task-1',{limit:20,cursor:'opaque-page'},undefined,expect.any(AbortSignal)))
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[{...finished,status:'interrupted',reason:'server_restarted',version:2}],nextCursor:''})
 await userEvent.click(screen.getByRole('button',{name:'刷新状态'}));expect(await screen.findByText('服务重启，本次不补跑')).toBeInTheDocument()
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}))
 expect(within(screen.getByRole('row',{name:/早报/})).getByText('已中断')).toBeInTheDocument()
})

it('does not leave a running acknowledgement after a completed response is read',async()=>{
 const finished={...occurrence,status:'succeeded' as const,released:true,version:4}
 vi.mocked(actions.runScheduleNow).mockResolvedValue({...occurrence,status:'running'})
 vi.mocked(history.getScheduleHistoryItem).mockResolvedValue(finished)
 vi.mocked(history.getScheduleExecutionResult).mockResolvedValue({sessionId:'s',turnId:'t',mode:'model',workspaceId:'',startedAt:item.createdAt,endedAt:item.createdAt,sessionEndedAt:item.createdAt,result:{status:'completed',answer:'真实结果',answerComplete:true,stopReason:'stop',failureCode:'',usage:null}})
 mount();await detail();await userEvent.click(screen.getByRole('button',{name:'立即运行'}));expect(await screen.findByText('完整结果')).toBeInTheDocument()
 expect(screen.getByRole('status')).toHaveTextContent('运行请求已记录，当前状态见运行历史。');expect(screen.getByRole('status')).not.toHaveTextContent('进行中')
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}));expect(within(screen.getByRole('row',{name:/早报/})).getByText('成功')).toBeInTheDocument()
})

it('re-reads the final answer once the occurrence is released',async()=>{
 const running={...occurrence,status:'running' as const,version:1}
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[running],nextCursor:''})
 vi.mocked(history.getScheduleExecutionResult).mockResolvedValue({sessionId:'s',turnId:'t',mode:'model',workspaceId:'',startedAt:item.createdAt,endedAt:null,sessionEndedAt:null,result:null})
 mount();const dialog=await detail();await userEvent.click(within(dialog).getByRole('button',{name:'运行历史'}))
 expect(await within(dialog).findByText('执行尚未返回最终结果。')).toBeInTheDocument()
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[{...running,status:'succeeded',released:true,version:3}],nextCursor:''})
 vi.mocked(history.getScheduleExecutionResult).mockResolvedValue({sessionId:'s',turnId:'t',mode:'model',workspaceId:'',startedAt:item.createdAt,endedAt:item.createdAt,sessionEndedAt:item.createdAt,result:{status:'completed',answer:'最终回答',answerComplete:true,stopReason:'stop',failureCode:'',usage:null}})
 await userEvent.click(within(dialog).getByRole('button',{name:'刷新状态'}))
 expect(await within(dialog).findByText('完整结果')).toBeInTheDocument()
 expect(within(dialog).getByText('最终回答')).toBeInTheDocument()
})

it('orders multi-page history by (recordedAt DESC, id DESC) including fractional RFC3339 timestamps',async()=>{
 expect(compareTimestampsDesc('2026-10-01T09:00:00.5Z','2026-10-01T09:00:00Z')).toBeLessThan(0)
 expect(compareTimestampsDesc('2026-10-01T09:00:00.000002Z','2026-10-01T09:00:00.000001Z')).toBeLessThan(0)
 expect(compareOccurrences({recordedAt:'2026-10-01T09:00:00Z',id:'00000000-0000-4000-8000-00000000000f'},{recordedAt:'2026-10-01T09:00:00Z',id:'00000000-0000-4000-8000-000000000002'})).toBeLessThan(0)
 const baseRun={...occurrence,released:true,executionRef:''}
 const page1=[
  {...baseRun,id:'00000000-0000-4000-8000-000000000001',status:'succeeded' as const,scheduledAt:'2026-10-01T09:00:00.5Z',recordedAt:'2026-10-01T09:00:00.5Z'},
  {...baseRun,id:'00000000-0000-4000-8000-00000000000f',status:'failed' as const,scheduledAt:'2026-10-01T09:00:00Z',recordedAt:'2026-10-01T09:00:00Z'},
 ]
 const page2=[
  {...baseRun,id:'00000000-0000-4000-8000-000000000002',status:'cancelled' as const,scheduledAt:'2026-10-01T09:00:00Z',recordedAt:'2026-10-01T09:00:00Z'},
 ]
 vi.mocked(history.listScheduleHistory).mockImplementation(async(_tid,_id,params)=>params?.cursor?{items:page2,nextCursor:''}:{items:page1,nextCursor:'composite-cursor'})
 mount();const dialog=await detail();await userEvent.click(within(dialog).getByRole('button',{name:'运行历史'}))
 await userEvent.click(await within(dialog).findByRole('button',{name:'加载更多记录'}))
 await waitFor(()=>{
  const labels=[...dialog.querySelectorAll('.sp-history-list button > span:first-child')].map(el=>el.textContent)
  expect(labels).toEqual(['成功','失败','已取消'])
 })
 await userEvent.click(screen.getByRole('button',{name:'关闭面板'}))
 expect(within(screen.getByRole('row',{name:/早报/})).getByText('成功')).toBeInTheDocument()
})

it('retains loaded tasks, resources and active history occupancy across poll errors, and maps action error codes',async()=>{
 const client=new QueryClient({defaultOptions:{queries:{retry:false},mutations:{retry:false}}})
 vi.mocked(history.getScheduleExecutionResult).mockResolvedValue({sessionId:'s',turnId:'t',mode:'model',workspaceId:'',startedAt:item.createdAt,endedAt:null,sessionEndedAt:null,result:null})
 vi.mocked(history.listScheduleHistory).mockResolvedValueOnce({items:[occurrence],nextCursor:''}).mockRejectedValueOnce({isAxiosError:true,response:{status:503}})
 mount(client);const dialog=await detail();await userEvent.click(within(dialog).getByRole('button',{name:'运行历史'}))
 const activeItem=await within(dialog).findByRole('button',{name:/进行中/})
 expect(activeItem).toHaveAttribute('aria-pressed','true')
 expect(within(dialog).getByRole('button',{name:'立即运行'})).toBeDisabled()

 vi.mocked(api.listScheduleConfigs).mockRejectedValueOnce({isAxiosError:true,response:{status:503}})
 await client.invalidateQueries({queryKey:['schedule-configs','user:tenant']})
 expect(await screen.findByText('列表刷新失败，当前继续显示已有任务。')).toBeInTheDocument()
 expect(screen.getByRole('row',{name:/早报/})).toBeInTheDocument()

 vi.mocked(api.getScheduleResources).mockRejectedValueOnce({isAxiosError:true,response:{status:503}})
 await userEvent.click(screen.getByRole('button',{name:'刷新资源'}))
 expect(await screen.findByText(/当前继续使用上次读取的资源/)).toBeInTheDocument()

 await userEvent.click(within(dialog).getByRole('button',{name:'刷新状态'}))
 expect(await within(dialog).findByText(/既有记录未被清除/)).toBeInTheDocument()
 expect(within(dialog).getByRole('button',{name:/进行中/})).toBeInTheDocument()
 expect(within(dialog).getByRole('button',{name:'立即运行'})).toBeDisabled()

 const unboundActive={...occurrence,id:'run-unbound',executionRef:''}
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[unboundActive],nextCursor:''})
 vi.mocked(actions.cancelScheduleRun).mockResolvedValue({id:unboundActive.id,scheduleId:item.id,kind:'manual',status:'cancelled',reason:'',released:true,version:2,scheduledAt:item.createdAt})
 await userEvent.click(within(dialog).getByRole('button',{name:'刷新状态'}))
 await waitFor(()=>expect(within(dialog).queryByText(/既有记录未被清除/)).not.toBeInTheDocument())
 vi.mocked(history.getScheduleExecutionResult).mockClear()
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[{...unboundActive,status:'cancelled',released:true,version:2}],nextCursor:''})
 await userEvent.click(within(dialog).getByRole('button',{name:'取消此次运行'}))
 await waitFor(()=>expect(actions.cancelScheduleRun).toHaveBeenCalledWith('tenant','task-1','run-unbound',{}))
 expect(history.getScheduleExecutionResult).not.toHaveBeenCalled()
 await waitFor(()=>expect(within(dialog).getByRole('button',{name:'立即运行'})).toBeEnabled())

 const skippedTarget={...occurrence,id:'run-skipped-target',status:'skipped' as const,reason:'execution_target_unavailable',executionRef:'',released:true}
 vi.mocked(history.listScheduleHistory).mockResolvedValue({items:[skippedTarget],nextCursor:''})
 vi.mocked(actions.runScheduleNow).mockRejectedValueOnce({isAxiosError:true,response:{status:409,data:{code:'execution_target_unavailable',params:{occurrenceId:'run-skipped-target'}}}})
 await userEvent.click(within(dialog).getByRole('button',{name:'立即运行'}))
 expect(await within(dialog).findByText(/所选模型或项目当前不可用/)).toBeInTheDocument()
 expect(await within(dialog).findByText('执行目标已不可用')).toBeInTheDocument()
})
