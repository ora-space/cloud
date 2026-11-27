import { useQuery } from '@tanstack/react-query'
import { listTaskExecutors } from '../collaboration/tasks-api/generated'
import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { Check, Clock3, GitBranch, MessageSquare, ListTodo } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel, FieldLegend, FieldSet } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { useWorkflowHasPersonalSkills, WORKFLOW_PERSONAL_SKILL_TEAM_VISIBILITY_HINT } from '@/lib/workflows/personal-skills'
import { agentChoice, agentModels, catalog, editableDraft, emptyDraft, formatTime, nextTimes, summary, taskDraft, validateDraft, weekdayNames, type Catalog, type Draft, type Task } from './schedule-model'

export function ScheduleForm({task,onSave,onCancel,data=catalog,saving=false,blocked=false,saveError,onCloseReady,resourcesLoading=false,resourcesError,onRetryResources}:{task?:Task;onSave:(draft:Draft,data:Catalog)=>void;onCancel:()=>void;data?:Catalog;saving?:boolean;blocked?:boolean;saveError?:string;onCloseReady?:(close:()=>void)=>void;resourcesLoading?:boolean;resourcesError?:string;onRetryResources?:()=>void}) {
 const [draft,setDraft]=useState(()=>task?taskDraft(task):emptyDraft())
 const [search,setSearch]=useState('')
 const [errors,setErrors]=useState<Record<string,string>>({})
 const [discard,setDiscard]=useState(false)
 const [dirty,setDirty]=useState(false)
 const [previewNow]=useState(()=>new Date())
 useEffect(()=>{onCloseReady?.(()=>dirty?setDiscard(true):onCancel())},[onCloseReady,dirty,onCancel])
 const patch=(changes:Partial<Draft>)=>{setDraft(previous=>({...previous,...changes}));setDirty(true);setErrors({})}
 const rule=(changes:Partial<Draft['rule']>)=>patch({rule:{...draft.rule,...changes}})
 const times=nextTimes(draft.rule,previewNow,task?new Date(task.createdAt):previewNow)
 const control=(name:string)=>({id:`schedule-${name}`,'aria-invalid':!!errors[name],'aria-describedby':errors[name]?`error-${name}`:undefined})
 const selectedAgent=agentChoice(draft,data)
 const models=agentModels(draft,data)
 const unavailable=resourcesLoading||(!!resourcesError&&!data.available?.models&&!data.available?.projects)
 const editable=editableDraft(draft,task)
 const workflows=data.workflows??[]
 const chosen=workflows.find(w=>w.id===draft.workflowId)
 // The hint must read the graph the trigger will run: the latest publication while following it, otherwise the snapshot the task is pinned to, which a later publication does not change.
 const pinnedSnapshotId=draft.kind==='workflow'&&draft.versionPolicy!=='latest'?draft.snapshotId:undefined
 const workflowHasPersonalSkills=useWorkflowHasPersonalSkills(draft.kind==='workflow'?data.tenantId:undefined,draft.kind==='workflow'?draft.workflowId:undefined,pinnedSnapshotId)
 // The version a save freezes is the one published now, which may differ from the one frozen before; a snapshot belongs to one workflow, so choosing another drops it.
 const workflowHint=draft.versionPolicy==='latest'?'每次触发时使用最新发布版本；历史运行始终保留当次实际使用的快照。':!chosen?'只列出已发布的工作流。':draft.snapshotId&&draft.snapshotId!==chosen.snapshotId?`当前固定的是旧版本，保存后改为生效版本 ${chosen.version}。`:`保存时固定生效版本 ${chosen.version}；之后发布的新版本要重新保存任务才会使用。`
 function submit(event:FormEvent<HTMLFormElement>){
  event.preventDefault()
  if(saving||blocked||resourcesLoading||!editable)return
  const next=validateDraft(draft,data)
  if(draft.kind==='chat'){
   if(draft.projectId){
    if(!data.available?.agents)next.agentId='Agent 目录尚未就绪，请刷新后再保存'
    else if(!selectedAgent)next.agentId='请选择当前可用的 Agent'
    else if(!draft.model||!selectedAgent.models.some(m=>m.id===draft.model))next.model='请选择该 Agent 可用的模型'
   }else{
    if(data.available?.models&&!models.some(m=>m.id===draft.model))next.model='请选择当前可用的模型'
    if(!data.available?.models&&draft.model)next.model='模型目录尚未就绪，请刷新后再保存'
   }
  }
  setErrors(next)
  if(Object.keys(next).length){const form=event.currentTarget;requestAnimationFrame(()=>form.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus());return}
  onSave(draft,data)
 }
 return <form className="sp-form" onSubmit={submit} noValidate>
  <div className="sp-form-scroll"><FieldGroup>
   <FieldSet className="sp-section">
    <FieldLegend variant="label"><span className="sp-section-number">01</span>执行内容</FieldLegend>
    <FieldGroup>
     <ScheduleField name="name" label="任务名称" error={errors.name}><Input placeholder="例如：每日 AI 简报" value={draft.name} onChange={e=>patch({name:e.target.value})} {...control('name')}/></ScheduleField>
     <ToggleGroup className="sp-kind-picker w-full" aria-label="执行方式" value={[draft.kind]} onValueChange={values=>{const kind=values.at(-1);if(kind==='task'&&draft.kind!=='task')patch({kind,projectId:'',executorId:'',rule:{...draft.rule,kind:'once',date:tomorrow(draft.rule.timezone)}});else if(kind==='chat'||kind==='workflow')patch({kind,...(draft.kind==='task'?{projectId:'',executorId:''}:{})})}} variant="outline" spacing={3}>
      <ToggleGroupItem disabled={!editable} className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="chat"><MessageSquare/><strong>直接对话</strong><small>用一段指令完成工作</small>{draft.kind==='chat'&&<Check className="sp-selected-check"/>}</ToggleGroupItem>
      <ToggleGroupItem disabled={!editable||!data.available?.workflows} className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="workflow"><GitBranch/><strong>运行工作流</strong><small>{data.available?.workflows?'按已发布的版本运行':'工作流服务尚未就绪'}</small>{draft.kind==='workflow'&&<Check className="sp-selected-check"/>}</ToggleGroupItem>
      <ToggleGroupItem disabled={!data.tenantId||!editable} className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="task"><ListTodo/><strong>处理协作任务</strong><small>在原任务中执行并查看结果</small></ToggleGroupItem>
     </ToggleGroup>
     {draft.kind==='chat'?<ScheduleField name="prompt" label="任务指令" error={errors.prompt} hint="每次运行新建独立对话，不携带上次聊天记录。"><Textarea className="min-h-28" rows={4} placeholder="描述任务目标与期望的输出格式…" value={draft.prompt} onChange={e=>patch({prompt:e.target.value})} {...control('prompt')}/></ScheduleField> :draft.kind==='task'&&editable?<>
       <ScheduleField name="search" label="搜索分配给我的任务"><Input id="schedule-search" placeholder="搜索标题或编号" value={search} onChange={e=>setSearch(e.target.value)} /></ScheduleField>
       <ScheduleField name="taskId" label="协作任务" error={errors.taskId} hint="只列出分配给你且待处理或进行中的任务。">
        <NativeSelect {...control('taskId')} value={draft.taskId} onChange={e=>{const t=data.tasks?.find(t=>t.id===e.target.value);patch({taskId:t?.id??'',projectId:t?.projectId??'',executorId:'',...(!draft.name?{name:t?.title??''}:{})})}}>
         <NativeSelectOption value="">{data.taskDirectoryError?'任务列表读取失败，请重新打开':!data.tasks?'正在读取任务…':data.tasks.length?'请选择任务':'没有可处理的任务'}</NativeSelectOption>
         {draft.taskId&&!data.tasks?.some(t=>t.id===draft.taskId)&&<NativeSelectOption value={draft.taskId} disabled>原任务已不可用</NativeSelectOption>}
         {data.tasks?.filter(t=>t.id===draft.taskId||(t.title+' '+t.id).toLowerCase().includes(search.trim().toLowerCase())).map(t=><NativeSelectOption key={t.id} value={t.id}>{t.title} · {t.id.slice(0,8)} · {data.projects.find(p=>p.id===t.projectId)?.name??'无关联项目'}</NativeSelectOption>)}
        </NativeSelect>
       </ScheduleField>
       {draft.taskId&&data.tenantId&&<TaskExecutor tenant={data.tenantId} taskId={draft.taskId} value={draft.executorId??''} onChange={executorId=>patch({executorId})} error={errors.executorId}/>}
       <ScheduleField name="note" label="补充说明" hint="可选。每次执行读取原任务的最新描述。"><Textarea {...control('note')} value={draft.note} onChange={e=>patch({note:e.target.value})}/></ScheduleField>
      </>:editable?<>
      <ScheduleField name="workflowId" label="工作流" error={errors.workflowId} hint={workflowHint}>
       <NativeSelect className="w-full" disabled={unavailable||!data.available?.workflows} value={draft.workflowId} onChange={e=>patch({workflowId:e.target.value,...(e.target.value!==draft.workflowId?{snapshotId:''}:{})})} {...control('workflowId')}>
        <NativeSelectOption value="">{data.available?.workflows?(workflows.length?'请选择已发布的工作流':'暂无已发布的工作流'):'工作流服务尚未就绪'}</NativeSelectOption>
        {draft.workflowId&&!chosen&&<NativeSelectOption value={draft.workflowId} disabled>原工作流暂不可用</NativeSelectOption>}
        {workflows.map(w=><NativeSelectOption key={w.id} value={w.id}>{w.name}（{w.version}）</NativeSelectOption>)}
       </NativeSelect>
       {workflowHasPersonalSkills&&<FieldDescription>{WORKFLOW_PERSONAL_SKILL_TEAM_VISIBILITY_HINT}</FieldDescription>}
      </ScheduleField>
      <ScheduleField name="versionPolicy" label="版本策略" hint="发布新版本不会自动修改固定版本任务。">
       <ToggleGroup id="schedule-versionPolicy" aria-label="版本策略" value={[draft.versionPolicy??'fixed']} onValueChange={values=>{const value=values.at(-1);if(value==='fixed'||value==='latest')patch({versionPolicy:value})}} variant="outline" spacing={2}>
        <ToggleGroupItem value="fixed">固定版本</ToggleGroupItem>
        <ToggleGroupItem value="latest">跟随最新发布版本</ToggleGroupItem>
       </ToggleGroup>
      </ScheduleField>
      <ScheduleField name="input" label="初始输入" hint="可选。每次运行都作为开始节点的输入；开始节点的变量使用工作流里的默认值。"><Textarea className="min-h-24" rows={3} placeholder="例如：整理本周合并的改动" value={draft.input} onChange={e=>patch({input:e.target.value})} {...control('input')}/></ScheduleField>
     </>:<p role="status">此执行方式尚未接入真实目录，已有配置保留，暂不能编辑。</p>}
    </FieldGroup>
   </FieldSet>
   <FieldSet className="sp-section" aria-busy={resourcesLoading}>
    <FieldLegend variant="label"><span className="sp-section-number">02</span>运行环境</FieldLegend>
    <FieldGroup>
     {resourcesLoading&&<p role="status">正在读取执行资源…</p>}
     {resourcesError&&<p role="alert">{resourcesError} <Button type="button" variant="outline" size="sm" onClick={onRetryResources}>重试加载</Button></p>}
     <ScheduleField name="projectId" label="关联项目" error={errors.projectId} hint={draft.kind==='task'?'沿用协作任务的项目。':draft.kind==='workflow'?'必选。每次运行在这个项目的独立目录中执行工作流。':'可选。不关联项目时直接调用模型；关联项目时使用该项目独立的执行环境。'}>
      <NativeSelect className="w-full" disabled={draft.kind==='task'||unavailable||!data.available?.projects} aria-required={draft.kind==='workflow'} value={draft.projectId} onChange={e=>patch({projectId:e.target.value,agentId:'',agent:'',model:''})} {...control('projectId')}>
       <NativeSelectOption value="" disabled={draft.kind==='workflow'} hidden={draft.kind==='workflow'}>{draft.kind==='workflow'?'请选择项目':'不关联项目'}</NativeSelectOption>
       {draft.projectId&&!data.projects.some(p=>p.id===draft.projectId)&&<NativeSelectOption value={draft.projectId} disabled>原项目暂不可用</NativeSelectOption>}
       {data.projects.map(p=><NativeSelectOption key={p.id} value={p.id}>{p.name}</NativeSelectOption>)}
      </NativeSelect>
     </ScheduleField>
     {draft.kind==='chat'&&<FieldGroup className="grid grid-cols-2 gap-4">
      <ScheduleField name="agentId" label="Agent" error={errors.agentId}>
       <NativeSelect className="w-full" disabled={unavailable||!draft.projectId||!data.available?.agents} value={selectedAgent?.id??(draft.agentId||draft.agent)} onChange={e=>{const item=(data.agents??[]).find(a=>a.id===e.target.value);patch({agentId:e.target.value,agent:item?.name??'',...(draft.model&&!item?.models.some(m=>m.id===draft.model)?{model:''}:{})})}} {...control('agentId')}>
        <NativeSelectOption value="">{!draft.projectId?'不关联项目时仅调用模型':!data.available?.agents?'Agent 服务尚未就绪':(data.agents??[]).length?'请选择 Agent':'暂无可用 Agent'}</NativeSelectOption>
        {(draft.agentId||draft.agent)&&!selectedAgent&&<NativeSelectOption value={draft.agentId||draft.agent} disabled>原 Agent 暂不可用</NativeSelectOption>}
        {(data.agents??[]).map(a=><NativeSelectOption key={a.id} value={a.id}>{a.name}</NativeSelectOption>)}
       </NativeSelect>
      </ScheduleField>
      <ScheduleField name="model" label="模型" error={errors.model}>
       <NativeSelect className="w-full" disabled={unavailable||(draft.projectId?(!data.available?.agents||!selectedAgent):!data.available?.models)} value={draft.model} onChange={e=>patch({model:e.target.value})} {...control('model')}>
        <NativeSelectOption value="">{draft.projectId?(!selectedAgent?'请先选择 Agent':models.length?'请选择模型':'该 Agent 暂无可用模型'):data.available?.models?(models.length?'请选择模型':'暂无可用模型'):'模型服务尚未就绪'}</NativeSelectOption>
        {draft.model&&!models.some(m=>m.id===draft.model)&&<NativeSelectOption value={draft.model} disabled>原模型暂不可用</NativeSelectOption>}
        {models.map(m=><NativeSelectOption key={m.id} value={m.id}>{m.name}</NativeSelectOption>)}
       </NativeSelect>
      </ScheduleField>
     </FieldGroup>}
     {draft.kind==='workflow'&&<FieldDescription>Agent 和模型由工作流里各个节点的配置决定。</FieldDescription>}
     {draft.kind==='chat'&&draft.projectId&&data.available?.agents&&<FieldDescription>Agent 和模型分开选择；模型只列出该 Agent 当前可运行的选项。</FieldDescription>}
     {draft.kind==='chat'&&!draft.projectId&&<FieldDescription>不关联项目时只直接调用模型，不启动 Agent。</FieldDescription>}
     {draft.kind==='chat'&&!resourcesLoading&&!data.available?.models&&<FieldDescription>模型服务尚未就绪。可以先保存无项目、未选模型的配置，接入后再选择模型；当前不会执行。</FieldDescription>}
     {!resourcesLoading&&data.available?.projects&&!data.projects.length&&<FieldDescription>暂无可用项目，可先配置无项目对话。</FieldDescription>}
    </FieldGroup>
   </FieldSet>
      <FieldSet className="sp-section">
        <FieldLegend variant="label"><span className="sp-section-number">03</span>时间安排</FieldLegend>
        <FieldGroup>
          <FieldGroup className="grid grid-cols-2 gap-4">
            <ScheduleField name="frequency" label="重复频率"><NativeSelect className="w-full" id="schedule-frequency" value={draft.rule.kind} onChange={e => rule({ kind: e.target.value as Draft['rule']['kind'],date:e.target.value==='once'?(draft.rule.date||tomorrow(draft.rule.timezone)):undefined })}><NativeSelectOption value="once">仅一次</NativeSelectOption><NativeSelectOption value="daily">每天</NativeSelectOption><NativeSelectOption value="weekdays">工作日（周一至周五）</NativeSelectOption><NativeSelectOption value="weekly">每周</NativeSelectOption><NativeSelectOption value="hours">每隔若干小时</NativeSelectOption></NativeSelect></ScheduleField>
            {draft.rule.kind === 'hours' ? <ScheduleField name="hours" label="间隔小时数" error={errors.hours}><Input type="number" min={1} max={24} value={draft.rule.hours} onChange={e => rule({ hours: Number(e.target.value) })} {...control('hours')} /></ScheduleField> : <ScheduleField name="time" label="执行时间" error={errors.time}><Input type="time" value={draft.rule.time} onChange={e => rule({ time: e.target.value })} {...control('time')} /></ScheduleField>}
          </FieldGroup>
          {draft.rule.kind==='once'&&<ScheduleField name="date" label="执行日期" error={errors.date}><Input {...control('date')} type="date" value={draft.rule.date??''} onChange={e=>rule({date:e.target.value})}/></ScheduleField>}
          {draft.rule.kind === 'weekly' && <Field data-invalid={!!errors.days}><ToggleGroup className="sp-weekdays w-full" aria-label="执行星期" multiple variant="outline" value={draft.rule.days.map(String)} onValueChange={values => rule({ days: values.map(Number) })} spacing={2}>{[1, 2, 3, 4, 5, 6, 0].map(day => <ToggleGroupItem className="flex-1" key={day} value={String(day)} aria-label={`星期${weekdayNames[day]}`}>{weekdayNames[day]}</ToggleGroupItem>)}</ToggleGroup><FieldError id="error-days">{errors.days}</FieldError></Field>}
          <ScheduleField name="timezone" label="时区" error={errors.timezone}><NativeSelect className="w-full" value={draft.rule.timezone} onChange={e => rule({ timezone: e.target.value })} {...control('timezone')}><NativeSelectOption value="Asia/Shanghai">Asia/Shanghai · 北京时间</NativeSelectOption><NativeSelectOption value="Asia/Tokyo">Asia/Tokyo · 东京</NativeSelectOption><NativeSelectOption value="Europe/London">Europe/London · 伦敦</NativeSelectOption><NativeSelectOption value="America/New_York">America/New_York · 纽约</NativeSelectOption><NativeSelectOption value="UTC">UTC · 协调世界时</NativeSelectOption></NativeSelect></ScheduleField>
          <div className="sp-time-preview"><div><Clock3 size={15} /><strong>{draft.rule.kind==='once'?'预计执行时间':'接下来三次'}</strong><span>{draft.rule.timezone}</span></div>{times.length ? times.map((time, i) => <p key={time.toISOString()}><span>{String(i + 1).padStart(2, '0')}</span>{formatTime(time, draft.rule.timezone)}</p>) : <p>填写完整时间规则后预览</p>}<small>{draft.rule.kind === 'hours' ? '从创建时间起，按固定小时数间隔计算。' : '按所选时区计算，跳过夏令时不存在的时刻。'}</small></div>
        </FieldGroup>
      </FieldSet>

   <div className="sp-summary"><Clock3 size={17}/><div><strong>任务预览</strong><p>{summary(draft,data)}</p><small>时间为预计触发时间；执行状态以运行记录为准。</small></div></div>
  </FieldGroup></div>
  {saveError&&<p role="alert" className="sp-helper">{saveError}</p>}
  <footer className="sp-form-footer">
   {discard?<><span>放弃未保存的修改？</span><Button type="button" variant="outline" onClick={()=>setDiscard(false)}>继续编辑</Button><Button type="button" onClick={onCancel}>放弃修改</Button></>:<><Button type="button" variant="outline" onClick={()=>dirty?setDiscard(true):onCancel()}>取消</Button><span className="sp-footer-spacer"/><Button type="submit" disabled={saving||blocked||resourcesLoading||!editable}>{saving?'保存中…':task?'保存修改':'创建并启用'}</Button></>}
  </footer>
 </form>
}

function TaskExecutor({tenant,taskId,value,onChange,error}:{tenant:string;taskId:string;value:string;onChange:(id:string)=>void;error?:string}){
 const q=useQuery({queryKey:['schedule-task-executors',tenant,taskId],queryFn:({signal})=>listTaskExecutors(tenant,taskId,undefined,signal),retry:false})
 return <ScheduleField name="executorId" label="Agent 与模型" error={error}>
  <NativeSelect id="schedule-executorId" aria-invalid={!!error} disabled={!q.data} value={value} onChange={e=>onChange(e.target.value)}>
    <NativeSelectOption value="">{q.isError?'执行器读取失败':q.isPending?'正在读取…':q.data?.items.length?'请选择 Agent 与模型':'没有可用执行器'}</NativeSelectOption>
    {value&&!q.data?.items.some(x=>x.id===value)&&<NativeSelectOption disabled value={value}>原执行器暂不可用</NativeSelectOption>}
    {q.data?.items.map(x=><NativeSelectOption key={x.id} value={x.id}>{x.name}</NativeSelectOption>)}
  </NativeSelect>{q.isError&&<Button variant="outline" type="button" onClick={()=>void q.refetch()}>重试</Button>}
 </ScheduleField>
}

function tomorrow(zone:string) {
 return new Intl.DateTimeFormat('sv-SE',{timeZone:zone,year:'numeric',month:'2-digit',day:'2-digit'}).format(new Date(Date.now()+86400000))
}

function ScheduleField({name,label,error,hint,children}:{name:string;label:string;error?:string;hint?:string;children:ReactNode}){
 return <Field data-invalid={!!error}><FieldLabel htmlFor={`schedule-${name}`}>{label}</FieldLabel>{children}{hint&&<FieldDescription>{hint}</FieldDescription>}<FieldError id={`error-${name}`}>{error}</FieldError></Field>
}
