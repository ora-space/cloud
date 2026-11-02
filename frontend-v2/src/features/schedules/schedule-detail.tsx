import { useEffect, useState } from 'react'
import { useInfiniteQuery, useMutation, useQuery } from '@tanstack/react-query'
import { Check, Clock3, Pause, Pencil, Play, RefreshCw, Square, TriangleAlert } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { getWorkflowRun } from '@/lib/workflows/api/generated'
import { listScheduleHistory, getScheduleHistoryItem, getScheduleExecutionResult } from './api/history/generated'
import { runScheduleNow, cancelScheduleRun } from './api/actions/generated'
import type { ScheduleActionReceipt } from './api/actions/generated'
import type { ScheduleResources } from './api/generated'
import { type Catalog, formatTime, isWorkflowTarget, nextTimes, projectName, ruleText, type Run, type Task } from './schedule-model'
import { compareOccurrences, compareTimestampsDesc, completeResult, errorOccurrenceId, finishManualRequest, isTerminalActionRejection, manualRequestKey, occurrenceWorkflowRun, reasonText, runLabels, runnable, scheduleError } from './runtime'

const workflowRunLabels: Record<string,string> = { pending: '未开始', running: '运行中', awaitingInput: '等待输入', succeeded: '成功', failed: '失败', cancelled: '已取消' }

export function RunStatus({run}:{run?:Run}) {
 const success=run?.status==='succeeded'||run?.status==='success'
 const Icon=success?Check:run?.status==='failed'?TriangleAlert:Clock3
 return <span className={`sp-run-status ${success?'success':run?.status??'none'}`}><Icon size={13}/>{run?runLabels[run.status]:'查看记录'}</span>
}

export function ScheduleDetail({data,resources,tenantId,scope,task,initialTab='config',onEdit,onToggle,toggling=false,onObserved}:{data:Catalog;resources:ScheduleResources;tenantId:string;scope:string;task:Task;initialTab?:'config'|'history';onEdit:()=>void;onToggle:()=>void;toggling?:boolean;onObserved?:(record:ScheduleActionReceipt)=>void}) {
 const [tab,setTab]=useState(initialTab)
 const [runId,setRunId]=useState('')
 const [operationError,setOperationError]=useState('')
 const [notice,setNotice]=useState('')
 const [now]=useState(()=>new Date())
 const history=useInfiniteQuery({queryKey:['schedule-history',scope,task.id],initialPageParam:'',queryFn:({pageParam,signal})=>listScheduleHistory(tenantId,task.id,{limit:20,...(pageParam?{cursor:pageParam}:{})},undefined,signal),getNextPageParam:page=>page.nextCursor||undefined,retry:false,refetchInterval:3000})
 const hasHistory=history.data!==undefined
 const rows=[...new Map((history.data?.pages.flatMap(p=>p.items)??[]).map(r=>[r.id,r])).values()].sort(compareOccurrences)
 const exact=useQuery({queryKey:['schedule-history-item',scope,task.id,runId],queryFn:({signal})=>getScheduleHistoryItem(tenantId,task.id,runId,undefined,signal),enabled:!!runId&&!rows.some(r=>r.id===runId),retry:false,refetchInterval:3000})
 const selected=runId?(rows.find(r=>r.id===runId)??(exact.isError?undefined:exact.data)):rows[0]
 const freshExact=!exact.isError&&exact.data&&!rows.some(r=>r.id===exact.data!.id)?exact.data:undefined
 const observed=[rows[0],freshExact].filter(r=>!!r).sort((a,b)=>compareTimestampsDesc(a.scheduledAt,b.scheduledAt)||compareOccurrences(a,b))[0]
 useEffect(()=>{if(observed)onObserved?.(observed)},[observed,onObserved])
 // The occurrence version is part of the key: a read taken while the run was still active must not
 // survive the release, otherwise a first "no final result yet" answer would never be refreshed.
 // A workflow occurrence's result is the run it started, read like the run page reads it.
 const workflowRunId=occurrenceWorkflowRun(selected)
 const output=useQuery({queryKey:['schedule-result',scope,task.id,selected?.id,selected?.version],queryFn:({signal})=>getScheduleExecutionResult(tenantId,task.id,selected!.id,undefined,signal),enabled:tab==='history'&&!!selected?.executionRef&&!workflowRunId,retry:false,refetchInterval:selected&&!selected.released?2000:false})
 const workflowRun=useQuery({queryKey:['schedule-workflow-run',scope,workflowRunId,selected?.version],queryFn:({signal})=>getWorkflowRun(tenantId,workflowRunId,undefined,signal),enabled:tab==='history'&&!!workflowRunId,retry:false,refetchInterval:selected&&!selected.released?2000:false})
 const run=useMutation({mutationFn:()=>runScheduleNow(tenantId,task.id,{requestId:manualRequestKey(scope,task.id)}),retry:false,onSuccess:receipt=>{finishManualRequest(scope,task.id);setRunId(receipt.id);onObserved?.(receipt);setOperationError('');setNotice('运行请求已记录，当前状态见运行历史。');setTab('history');void history.refetch()},onError:error=>{const oid=errorOccurrenceId(error);if(oid)setRunId(oid);if(isTerminalActionRejection(error))finishManualRequest(scope,task.id);setOperationError(scheduleError(error,'运行请求失败，请重试同一请求。'));setTab('history');void history.refetch()}})
 const cancel=useMutation({mutationFn:(id:string)=>cancelScheduleRun(tenantId,task.id,id,{}),retry:false,onSuccess:receipt=>{onObserved?.(receipt);setOperationError('');setNotice('取消请求已处理，当前状态见运行历史。');void history.refetch();if(selected?.executionRef&&!workflowRunId)void output.refetch()},onError:error=>{setOperationError(scheduleError(error,'取消失败，运行状态未确认，请稍后重试。'));void history.refetch()}})
 const busy=run.isPending||cancel.isPending
 const occupied=rows.some(r=>!r.released)||!!selected&&!selected.released
 const available=runnable(task,resources)
 const target=task.target
 const workflow=isWorkflowTarget(target)?resources.workflows.find(w=>w.id===target.workflowId):undefined
 const canCancel=!!selected&&!selected.released&&['running','cancelling','succeeded','failed','cancelled','interrupted'].includes(selected.status)
 const cleanup=!!selected&&['succeeded','failed','cancelled','interrupted'].includes(selected.status)
 return <div className="sp-detail">
  <div className="sp-detail-toolbar"><span className={`sp-enabled ${task.enabled?'':'paused'}`}>{task.enabled?'已启用':'已暂停'}</span><span className="sp-footer-spacer"/><Button variant="outline" size="sm" disabled={busy} onClick={onEdit}><Pencil data-icon="inline-start"/>编辑</Button></div>
  <div className="sp-tabs" aria-label="详情视图"><button aria-pressed={tab==='config'} onClick={()=>setTab('config')}>任务配置</button><button aria-pressed={tab==='history'} onClick={()=>setTab('history')}>运行历史</button></div>
  <div className="sp-detail-scroll">
   {tab==='config'?<>
    <div className="sp-config-heading">执行内容</div><strong>{target.kind==='chat'?'直接对话':isWorkflowTarget(target)?'运行工作流':target.kind==='workflow'?'工作流（旧引用）':'已有任务（待接入）'}</strong>
    {target.kind==='chat'?<p className="sp-prompt-preview">{target.prompt}</p>:isWorkflowTarget(target)?<p className="sp-prompt-preview">{target.input||'没有初始输入'}</p>:<p className="sp-helper">此类型的真实目录和执行入口尚未就绪，已有配置已保留。</p>}
    <div className="sp-config-heading">运行环境</div><dl className="sp-key-values">
     <div><dt>关联项目</dt><dd>{projectName(task.projectId,data)}</dd></div>
     {isWorkflowTarget(target)&&<div><dt>工作流</dt><dd>{!workflow?'原工作流暂不可用':workflow.snapshotId===target.snapshotId?`${workflow.name}（${workflow.version}）`:`${workflow.name}（固定的旧版本；生效版本已是 ${workflow.version}，编辑保存后改用）`}</dd></div>}
     {target.kind==='chat'&&<><div><dt>模型</dt><dd>{resources.models.find(m=>m.id===target.model)?.name??(target.model?'原模型暂不可用':'尚未选择模型')}</dd></div><div><dt>执行器</dt><dd>{!task.projectId?'无需执行器':resources.executors.find(e=>e.id===target.executorId)?.name??'原执行器暂不可用'}</dd></div></>}
     <div><dt>执行频率</dt><dd>{ruleText(task.rule)}</dd></div><div><dt>时区</dt><dd>{task.rule.timezone}</dd></div>
    </dl>
    <div className="sp-time-preview"><div><Clock3 size={15}/><strong>预计触发时间</strong></div>{nextTimes(task.rule,now,new Date(task.createdAt)).map((date,i)=><p key={date.toISOString()}><span>{i+1}</span>{formatTime(date,task.rule.timezone)}</p>)}</div>
    <p className="sp-helper">每次独立执行，结果保存在本任务的运行历史。计划暂停不停止已开始的运行。</p>
   </>:<>
    <div className="sp-detail-toolbar"><strong>运行记录</strong><span className="sp-footer-spacer"/><Button variant="outline" size="sm" disabled={history.isFetching} onClick={()=>void history.refetch()}><RefreshCw data-icon="inline-start"/>刷新状态</Button></div>
    {history.isError&&hasHistory&&<p role="alert">{scheduleError(history.error,'运行记录读取失败，请重试。')} 既有记录未被清除。</p>}
    {history.isPending?<p role="status">正在读取运行记录…</p>:history.isError&&!hasHistory?<p role="alert">{scheduleError(history.error,'运行记录读取失败，请重试。')}</p>:!rows.length&&!selected?<p className="sp-empty-small">{runId?'正在核对本次运行记录…':'尚无运行记录。'}</p>:<>
     <div className="sp-history-list">{rows.map(r=><button key={r.id} aria-pressed={selected?.id===r.id} onClick={()=>setRunId(r.id)}><span>{runLabels[r.status]}</span><span>{formatTime(r.scheduledAt,task.rule.timezone)}</span><small>{r.kind==='manual'?'手动':'定时'}{!r.released?' · 尚未结束':''}</small></button>)}</div>
     {history.hasNextPage&&<Button variant="outline" disabled={history.isFetchingNextPage} onClick={()=>void history.fetchNextPage()}>{history.isFetchingNextPage?'加载中…':'加载更多记录'}</Button>}
     <p className="sp-helper">记录按记录时间倒序分页读取。停留在详情时自动刷新。</p>
     {selected&&<section aria-label="选中运行的结果">
      <div className="sp-config-heading">{runLabels[selected.status]}{!selected.released?' · 等待结束确认':''}</div>
      {selected.reason&&<p className="sp-helper">{reasonText(selected.reason)}</p>}
      {workflowRunId?workflowRun.isPending?<p role="status">正在读取工作流运行…</p>:workflowRun.isError?<p role="alert">{scheduleError(workflowRun.error,'工作流运行暂时无法读取。')} <Button variant="outline" size="sm" onClick={()=>void workflowRun.refetch()}>重试读取运行</Button></p>:<>
       <div className="sp-config-heading">工作流运行 · {workflowRunLabels[workflowRun.data.run.status]??workflowRun.data.run.status}</div>
       <pre className="sp-prompt-preview" style={{whiteSpace:'pre-wrap',overflowWrap:'anywhere'}}>{workflowRun.data.run.output??workflowRun.data.run.error??'运行尚未产生输出'}</pre>
       <p className="sp-helper">这次运行也列在对话页「{projectName(task.projectId,data)}」项目下，可以打开查看每个节点。</p>
      </>:!selected.executionRef?<p className="sp-helper">此次运行尚无可读取的执行结果。</p>:output.isPending?<p role="status">正在读取结果…</p>:output.isError?<p role="alert">{scheduleError(output.error,'完整结果暂时无法读取。')} <Button variant="outline" size="sm" onClick={()=>void output.refetch()}>重试读取结果</Button></p>:output.data?.result?<>
       <div className="sp-config-heading">{completeResult(output.data)?'完整结果':'部分结果或未完成结果'}</div>
       <pre className="sp-prompt-preview" style={{whiteSpace:'pre-wrap',overflowWrap:'anywhere'}}>{output.data.result.answer||'未返回文本回答'}</pre>
       {output.data.result.failureCode&&<p>{reasonText(output.data.result.failureCode)}</p>}
      </>:<p role="status">执行尚未返回最终结果。</p>}
      {canCancel&&<Button variant="outline" disabled={busy} onClick={()=>cancel.mutate(selected.id)}><Square data-icon="inline-start"/>{cancel.isPending?'正在请求…':cleanup?'重试收尾':'取消此次运行'}</Button>}
     </section>}
    </>}
   </>}
   {!available&&<p className="sp-helper">执行资源尚未就绪或已失效，请刷新目录并编辑配置后再运行。</p>}
   {exact.isError&&runId&&<p role="alert">{scheduleError(exact.error,'本次运行记录暂时无法读取。')} <Button variant="outline" size="sm" onClick={()=>void exact.refetch()}>核对本次运行</Button></p>}
   {operationError&&<p role="alert">{operationError}</p>}
   {notice&&!operationError&&<p role="status">{notice}</p>}
  </div>
  <footer className="sp-detail-footer"><Button size="sm" disabled={!available||busy||occupied} onClick={()=>{setNotice('');run.mutate()}}><Play data-icon="inline-start"/>{run.isPending?'正在提交…':run.isError&&!isTerminalActionRejection(run.error)?'重试运行请求':'立即运行'}</Button><span className="sp-footer-spacer"/><Button variant="outline" size="sm" disabled={busy||toggling} onClick={onToggle}>{task.enabled?<Pause data-icon="inline-start"/>:<Play data-icon="inline-start"/>}{task.enabled?'暂停任务':'恢复任务'}</Button></footer>
 </div>
}
