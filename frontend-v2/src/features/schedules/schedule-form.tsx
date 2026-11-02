import { useEffect, useState, type FormEvent, type ReactNode } from 'react'
import { Check, Clock3, GitBranch, MessageSquare, ListTodo } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Field, FieldDescription, FieldError, FieldGroup, FieldLabel, FieldLegend, FieldSet } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { catalog, editableDraft, emptyDraft, formatTime, nextTimes, summary, taskDraft, validateDraft, weekdayNames, type Catalog, type Draft, type Task } from './schedule-model'

export function ScheduleForm({task,onSave,onCancel,data=catalog,saving=false,blocked=false,saveError,onCloseReady,resourcesLoading=false,resourcesError,onRetryResources}:{task?:Task;onSave:(draft:Draft,data:Catalog)=>void;onCancel:()=>void;data?:Catalog;saving?:boolean;blocked?:boolean;saveError?:string;onCloseReady?:(close:()=>void)=>void;resourcesLoading?:boolean;resourcesError?:string;onRetryResources?:()=>void}) {
 const [draft,setDraft]=useState(()=>task?taskDraft(task):emptyDraft())
 const [errors,setErrors]=useState<Record<string,string>>({})
 const [discard,setDiscard]=useState(false)
 const [dirty,setDirty]=useState(false)
 const [previewNow]=useState(()=>new Date())
 useEffect(()=>{onCloseReady?.(()=>dirty?setDiscard(true):onCancel())},[onCloseReady,dirty,onCancel])
 const patch=(changes:Partial<Draft>)=>{setDraft(previous=>({...previous,...changes}));setDirty(true);setErrors({})}
 const rule=(changes:Partial<Draft['rule']>)=>patch({rule:{...draft.rule,...changes}})
 const times=nextTimes(draft.rule,previewNow,task?new Date(task.createdAt):previewNow)
 const control=(name:string)=>({id:`schedule-${name}`,'aria-invalid':!!errors[name],'aria-describedby':errors[name]?`error-${name}`:undefined})
 const models=data.models??[]
 const executors=(data.executors??[]).filter(e=>!draft.model||e.modelId===draft.model)
 const unavailable=resourcesLoading||(!!resourcesError&&!data.available?.models&&!data.available?.projects)
 const editable=editableDraft(draft,task)
 const workflows=data.workflows??[]
 const chosen=workflows.find(w=>w.id===draft.workflowId)
 // The version a save freezes is the one published now, which may differ from the one frozen before.
 const workflowHint=!chosen?'只列出已发布的工作流。':draft.snapshotId&&draft.snapshotId!==chosen.snapshotId?`当前固定的是旧版本，保存后改为生效版本 ${chosen.version}。`:`保存时固定生效版本 ${chosen.version}；之后发布的新版本要重新保存任务才会使用。`
 function submit(event:FormEvent<HTMLFormElement>){
  event.preventDefault()
  if(saving||blocked||resourcesLoading||!editable)return
  const next=validateDraft(draft,data)
  if(draft.kind==='chat'){
   if(data.available?.models&&!models.some(m=>m.id===draft.model))next.model='请选择当前可用的模型'
   if(!data.available?.models&&draft.model)next.model='模型目录尚未就绪，请刷新后再保存'
   if(draft.projectId&&(!data.available?.executors||!executors.some(e=>e.id===draft.executorId&&e.modelId===draft.model)))next.executorId='请选择当前可用的执行器'
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
     <ToggleGroup className="sp-kind-picker w-full" aria-label="执行方式" value={[draft.kind]} onValueChange={values=>{const kind=values.at(-1);if(kind==='chat'||kind==='workflow')patch({kind})}} variant="outline" spacing={3}>
      <ToggleGroupItem disabled={draft.kind==='task'} className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="chat"><MessageSquare/><strong>直接对话</strong><small>用一段指令完成工作</small>{draft.kind==='chat'&&<Check className="sp-selected-check"/>}</ToggleGroupItem>
      <ToggleGroupItem disabled={draft.kind==='task'||!data.available?.workflows} className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="workflow"><GitBranch/><strong>运行工作流</strong><small>{data.available?.workflows?'按已发布的版本运行':'工作流服务尚未就绪'}</small>{draft.kind==='workflow'&&<Check className="sp-selected-check"/>}</ToggleGroupItem>
      <ToggleGroupItem disabled className="sp-choice h-auto min-h-24 flex-1 flex-col items-start gap-2 p-4" value="task"><ListTodo/><strong>已有任务</strong><small>任务目录待接入</small></ToggleGroupItem>
     </ToggleGroup>
     {draft.kind==='chat'?<ScheduleField name="prompt" label="任务指令" error={errors.prompt} hint="每次运行新建独立对话，不携带上次聊天记录。"><Textarea className="min-h-28" rows={4} placeholder="描述任务目标与期望的输出格式…" value={draft.prompt} onChange={e=>patch({prompt:e.target.value})} {...control('prompt')}/></ScheduleField>:editable?<>
      <ScheduleField name="workflowId" label="工作流" error={errors.workflowId} hint={workflowHint}>
       <NativeSelect className="w-full" disabled={unavailable||!data.available?.workflows} value={draft.workflowId} onChange={e=>patch({workflowId:e.target.value})} {...control('workflowId')}>
        <NativeSelectOption value="">{data.available?.workflows?(workflows.length?'请选择已发布的工作流':'暂无已发布的工作流'):'工作流服务尚未就绪'}</NativeSelectOption>
        {draft.workflowId&&!chosen&&<NativeSelectOption value={draft.workflowId} disabled>原工作流暂不可用</NativeSelectOption>}
        {workflows.map(w=><NativeSelectOption key={w.id} value={w.id}>{w.name}（{w.version}）</NativeSelectOption>)}
       </NativeSelect>
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
     <ScheduleField name="projectId" label="关联项目" error={errors.projectId} hint={draft.kind==='workflow'?'必选。每次运行在这个项目的独立目录中执行工作流。':'可选。不关联项目时直接调用模型；关联项目时使用该项目独立的执行环境。'}>
      <NativeSelect className="w-full" disabled={unavailable||!data.available?.projects} value={draft.projectId} onChange={e=>patch({projectId:e.target.value,executorId:'',agent:''})} {...control('projectId')}>
       <NativeSelectOption value="">不关联项目</NativeSelectOption>
       {draft.projectId&&!data.projects.some(p=>p.id===draft.projectId)&&<NativeSelectOption value={draft.projectId} disabled>原项目暂不可用</NativeSelectOption>}
       {data.projects.map(p=><NativeSelectOption key={p.id} value={p.id}>{p.name}</NativeSelectOption>)}
      </NativeSelect>
     </ScheduleField>
     {draft.kind==='chat'&&<FieldGroup className="grid grid-cols-2 gap-4">
      <ScheduleField name="model" label="模型" error={errors.model}>
       <NativeSelect className="w-full" disabled={unavailable||!data.available?.models} value={draft.model} onChange={e=>patch({model:e.target.value,executorId:'',agent:''})} {...control('model')}>
        <NativeSelectOption value="">{data.available?.models?(models.length?'请选择模型':'暂无可用模型'):'模型服务尚未就绪'}</NativeSelectOption>
        {draft.model&&!models.some(m=>m.id===draft.model)&&<NativeSelectOption value={draft.model} disabled>原模型暂不可用</NativeSelectOption>}
        {models.map(m=><NativeSelectOption key={m.id} value={m.id}>{m.name}</NativeSelectOption>)}
       </NativeSelect>
      </ScheduleField>
      <ScheduleField name="executorId" label="执行器" error={errors.executorId}>
       <NativeSelect className="w-full" disabled={unavailable||!draft.projectId||!draft.model||!data.available?.executors} value={draft.executorId??''} onChange={e=>{const item=executors.find(v=>v.id===e.target.value);patch({executorId:e.target.value,agent:item?.agentName??''})}} {...control('executorId')}>
        <NativeSelectOption value="">{!draft.projectId?'不关联项目时无需执行器':!data.available?.executors?'执行器服务尚未就绪':executors.length?'请选择执行器':'暂无匹配的执行器'}</NativeSelectOption>
        {draft.executorId&&!executors.some(e=>e.id===draft.executorId)&&<NativeSelectOption value={draft.executorId} disabled>原执行器暂不可用</NativeSelectOption>}
        {executors.map(e=><NativeSelectOption key={e.id} value={e.id}>{e.name}</NativeSelectOption>)}
       </NativeSelect>
      </ScheduleField>
     </FieldGroup>}
     {draft.kind==='workflow'&&<FieldDescription>Agent 和模型由工作流里各个节点的配置决定。</FieldDescription>}
     {draft.kind==='chat'&&!resourcesLoading&&!data.available?.models&&<FieldDescription>模型服务尚未就绪。可以先保存无项目、未选模型的配置，接入后再选择模型；当前不会执行。</FieldDescription>}
     {!resourcesLoading&&data.available?.projects&&!data.projects.length&&<FieldDescription>暂无可用项目，可先配置无项目对话。</FieldDescription>}
    </FieldGroup>
   </FieldSet>
      <FieldSet className="sp-section">
        <FieldLegend variant="label"><span className="sp-section-number">03</span>时间安排</FieldLegend>
        <FieldGroup>
          <FieldGroup className="grid grid-cols-2 gap-4">
            <ScheduleField name="frequency" label="重复频率"><NativeSelect className="w-full" id="schedule-frequency" value={draft.rule.kind} onChange={e => rule({ kind: e.target.value as Draft['rule']['kind'] })}><NativeSelectOption value="daily">每天</NativeSelectOption><NativeSelectOption value="weekdays">工作日（周一至周五）</NativeSelectOption><NativeSelectOption value="weekly">每周</NativeSelectOption><NativeSelectOption value="hours">每隔若干小时</NativeSelectOption></NativeSelect></ScheduleField>
            {draft.rule.kind === 'hours' ? <ScheduleField name="hours" label="间隔小时数" error={errors.hours}><Input type="number" min={1} max={24} value={draft.rule.hours} onChange={e => rule({ hours: Number(e.target.value) })} {...control('hours')} /></ScheduleField> : <ScheduleField name="time" label="执行时间" error={errors.time}><Input type="time" value={draft.rule.time} onChange={e => rule({ time: e.target.value })} {...control('time')} /></ScheduleField>}
          </FieldGroup>
          {draft.rule.kind === 'weekly' && <Field data-invalid={!!errors.days}><ToggleGroup className="sp-weekdays w-full" aria-label="执行星期" multiple variant="outline" value={draft.rule.days.map(String)} onValueChange={values => rule({ days: values.map(Number) })} spacing={2}>{[1, 2, 3, 4, 5, 6, 0].map(day => <ToggleGroupItem className="flex-1" key={day} value={String(day)} aria-label={`星期${weekdayNames[day]}`}>{weekdayNames[day]}</ToggleGroupItem>)}</ToggleGroup><FieldError id="error-days">{errors.days}</FieldError></Field>}
          <ScheduleField name="timezone" label="时区" error={errors.timezone}><NativeSelect className="w-full" value={draft.rule.timezone} onChange={e => rule({ timezone: e.target.value })} {...control('timezone')}><NativeSelectOption value="Asia/Shanghai">Asia/Shanghai · 北京时间</NativeSelectOption><NativeSelectOption value="Asia/Tokyo">Asia/Tokyo · 东京</NativeSelectOption><NativeSelectOption value="Europe/London">Europe/London · 伦敦</NativeSelectOption><NativeSelectOption value="America/New_York">America/New_York · 纽约</NativeSelectOption><NativeSelectOption value="UTC">UTC · 协调世界时</NativeSelectOption></NativeSelect></ScheduleField>
          <div className="sp-time-preview"><div><Clock3 size={15} /><strong>接下来三次</strong><span>{draft.rule.timezone}</span></div>{times.length ? times.map((time, i) => <p key={time.toISOString()}><span>{String(i + 1).padStart(2, '0')}</span>{formatTime(time, draft.rule.timezone)}</p>) : <p>填写完整时间规则后预览</p>}<small>{draft.rule.kind === 'hours' ? '从创建时间起，按固定小时数间隔计算。' : '按所选时区计算，跳过夏令时不存在的时刻。'}</small></div>
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

function ScheduleField({name,label,error,hint,children}:{name:string;label:string;error?:string;hint?:string;children:ReactNode}){
 return <Field data-invalid={!!error}><FieldLabel htmlFor={`schedule-${name}`}>{label}</FieldLabel>{children}{hint&&<FieldDescription>{hint}</FieldDescription>}<FieldError id={`error-${name}`}>{error}</FieldError></Field>
}
