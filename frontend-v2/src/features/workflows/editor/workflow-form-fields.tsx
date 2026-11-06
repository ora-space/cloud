import { useGetApiV1TenantsTidMembers } from '@/api/members/members'
import { useContractsClient } from '@/lib/workflows/data/contracts-client'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import type { Node } from '@xyflow/react'
import type { WorkflowNodeData, WorkflowVariableCatalogEntry } from '@/lib/workflows/mock'
import { Field, FieldGroup, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'
import { Plus, Trash2 } from 'lucide-react'

/** Configuration shared by newly authored inspection reviews. */
type ReviewField = {name:string;label:string;type:string;required:boolean;options?:string[]}
const defaultReviewConfig = {
  title: '巡检问题确认', submitLabel: '确认并生成任务', reviewerId: '',
  reportSelector: ['report', 'structured_output', 'reportMarkdown'],
  itemsSelector: ['report', 'structured_output', 'findings'],
  fields: [{ name: 'notes', label: '备注', type: 'textarea', required: false }] as ReviewField[],
}

/** Edits frozen review bindings; task effects remain a separate successor node. */
export function WorkflowFormFields({ node, onUpdate, onClose, variableCatalog, graphNodes = [] }: { variableCatalog: WorkflowVariableCatalogEntry[]; graphNodes?: Node<WorkflowNodeData, 'workflow'>[]; node: Node<WorkflowNodeData, 'workflow'>; onUpdate: (node: Node<WorkflowNodeData, 'workflow'>) => void; onClose: () => void }) {
  const tenantId=useContractsClient().tenantId
  const members=useGetApiV1TenantsTidMembers(tenantId,undefined,{query:{enabled:!!tenantId}})
  const isForm = node.data.kind === 'form'
  const config = { ...defaultReviewConfig, ...(node.data.formConfig as Partial<typeof defaultReviewConfig> | undefined) }
  const task = (node.data.createTasksConfig ?? { formNodeId: '' }) as { formNodeId: string }
  const update = (patch: Partial<typeof defaultReviewConfig>) => onUpdate({ ...node, data: { ...node.data, formConfig: { ...config, ...patch } } })
  const patchField = (index: number, patch: Partial<ReviewField>) => update({ fields: config.fields.map((field, at) => at === index ? { ...field, ...patch } : field) })
  return <section className="flex h-full flex-col gap-4 overflow-auto p-4" aria-label={isForm ? '动态表单配置' : '创建任务配置'}>
    <header>
      <h2 className="font-semibold">{isForm ? '动态表单' : '创建任务'}</h2>
      <p className="mt-1 text-xs text-muted-foreground">{isForm ? '改动即时保存；发布后这份配置随快照冻结。' : '只处理确认的问题；新建任务沿用本次运行的项目。'}</p>
    </header>

    <FieldGroup>
      <Field><FieldLabel htmlFor="review-node-title">节点名称</FieldLabel><Input id="review-node-title" value={node.data.title} onChange={e => onUpdate({ ...node, data: { ...node.data, title: e.target.value } })} /></Field>
      {isForm ? <>
        <Field><FieldLabel htmlFor="review-title">表单标题</FieldLabel><Input id="review-title" value={config.title} onChange={e => update({ title: e.target.value })} /></Field>
        <Field>
          <FieldLabel htmlFor="review-report">报告来源变量</FieldLabel>
          <SourceSelect id="review-report" value={config.reportSelector} entries={variableCatalog.filter(v=>v.valueType==='string'||v.valueType==='any')} onChange={reportSelector=>update({reportSelector})} />
          <p className="text-xs text-muted-foreground">一段 Markdown 文本，展示在确认页左侧。</p>
        </Field>
        <Field>
          <FieldLabel htmlFor="review-items">问题列表来源变量</FieldLabel>
          <SourceSelect id="review-items" value={config.itemsSelector} entries={variableCatalog.filter(v=>v.valueType.startsWith('array')||v.valueType==='any')} onChange={itemsSelector=>update({itemsSelector})} />
          <p className="text-xs text-muted-foreground">结构化数组，每项至少包含 <code className="font-mono">key</code> 和 <code className="font-mono">title</code>；<code className="font-mono">key</code> 用于跨天复用同一个任务。</p>
        </Field>
        <Field><FieldLabel htmlFor="review-user">确认人</FieldLabel><NativeSelect id="review-user" value={config.reviewerId} onChange={e => update({ reviewerId: e.target.value })}><NativeSelectOption value="">运行发起人</NativeSelectOption>{(members.data?.items??[]).filter(m=>m.status==='active').map(m=><NativeSelectOption key={m.userId} value={m.userId}>{m.displayName}</NativeSelectOption>)}</NativeSelect></Field>
        <Field><FieldLabel htmlFor="review-submit">提交按钮文案</FieldLabel><Input id="review-submit" value={config.submitLabel} onChange={e => update({ submitLabel: e.target.value })} /></Field>

        <div className="flex items-center justify-between gap-2 pt-1">
          <div>
            <h3 className="text-sm font-medium">附加字段</h3>
            <p className="mt-0.5 text-xs text-muted-foreground">每行问题都会出现这些输入；只读字段展示上游同名字段。</p>
          </div>
          <Button variant="outline" size="sm" disabled={config.fields.length>=20} onClick={()=>update({fields:[...config.fields,{name:`field${config.fields.length+1}`,label:'新字段',type:'text',required:false}]})}><Plus />添加</Button>
        </div>

        {config.fields.length === 0 && <p className="rounded-lg border border-dashed bg-muted/20 px-3 py-4 text-center text-xs text-muted-foreground">还没有附加字段，确认结果只记录判定和负责人。</p>}

        {config.fields.map((field,index)=><fieldset key={index} className="rounded-lg border bg-muted/15 p-3">
          <legend className="sr-only">附加字段 {index+1}</legend>
          <div className="flex items-center justify-between gap-2">
            <span className="text-xs font-medium text-muted-foreground">字段 {index+1}</span>
            <Button size="icon-xs" variant="ghost" aria-label={`移除附加字段 ${index+1}`} onClick={()=>update({fields:config.fields.filter((_,at)=>at!==index)})}><Trash2 /></Button>
          </div>
          <div className="mt-2 grid gap-2">
            <label className="grid gap-1">
              <span className="text-[11px] text-muted-foreground">标识（下游按它取值）</span>
              <Input aria-label={`字段 ${index+1} 标识`} value={field.name} onChange={e=>patchField(index,{name:e.target.value})}/>
            </label>
            <label className="grid gap-1">
              <span className="text-[11px] text-muted-foreground">显示名</span>
              <Input aria-label={`字段 ${index+1} 名称`} value={field.label} onChange={e=>patchField(index,{label:e.target.value})}/>
            </label>
            <label className="grid gap-1">
              <span className="text-[11px] text-muted-foreground">类型</span>
              <NativeSelect aria-label={`字段 ${index+1} 类型`} value={field.type} onChange={e=>patchField(index,{type:e.target.value,options:e.target.value==='select'?['选项一','选项二']:undefined})}>
                {[['text','单行文本'],['textarea','多行文本'],['select','下拉选择'],['readonly','只读文本']].map(([v,label])=><NativeSelectOption key={v} value={v}>{label}</NativeSelectOption>)}
              </NativeSelect>
            </label>
            {field.type==='select'&&<label className="grid gap-1">
              <span className="text-[11px] text-muted-foreground">选项（用逗号分隔）</span>
              <Input aria-label={`字段 ${index+1} 选项`} value={(field.options??[]).join('，')} onChange={e=>patchField(index,{options:e.target.value.split(/[，,]/)})}/>
            </label>}
            <label className="flex items-center gap-2 text-xs">
              <input type="checkbox" className="size-3.5 accent-[var(--primary)]" checked={field.required} onChange={e=>patchField(index,{required:e.target.checked})}/>
              提交前必须填写
            </label>
          </div>
        </fieldset>)}
      </> : <>
        <Field><FieldLabel htmlFor="review-source">上游表单</FieldLabel><NativeSelect id="review-source" value={task.formNodeId} onChange={e => onUpdate({ ...node, data: { ...node.data, createTasksConfig: { formNodeId: e.target.value } } })}><NativeSelectOption value="">请选择表单节点</NativeSelectOption>{task.formNodeId&&!graphNodes.some(n=>n.id===task.formNodeId)&&<NativeSelectOption value={task.formNodeId}>原绑定（节点不可用）</NativeSelectOption>}{graphNodes.filter(n=>n.data.kind==='form').map(n=><NativeSelectOption key={n.id} value={n.id}>{n.data.title}</NativeSelectOption>)}</NativeSelect></Field>
        <p className="text-sm text-muted-foreground">仅处理判定为问题的条目：已存在未关闭任务时关联它并沿用负责人，否则按本次选定的负责人新建。</p>
      </>}
    </FieldGroup>

    <Button variant="outline" onClick={onClose}>关闭配置</Button>
  </section>
}

function SourceSelect({id,value,entries,onChange}:{id:string;value:string[];entries:WorkflowVariableCatalogEntry[];onChange:(selector:string[])=>void}){
 const selected=JSON.stringify(value)
 return <NativeSelect id={id} value={selected} onChange={e=>onChange(entries.find(v=>JSON.stringify(v.selector)===e.target.value)?.selector??[])}>
  <NativeSelectOption value="[]">请选择上游变量</NativeSelectOption>
  {value.length>0&&!entries.some(v=>JSON.stringify(v.selector)===selected)&&<NativeSelectOption value={selected}>原绑定：{value.join('.')}（请检查连线）</NativeSelectOption>}
  {entries.map(v=><NativeSelectOption key={JSON.stringify(v.selector)} value={JSON.stringify(v.selector)}>{v.sourceNodeTitle||v.sourceNodeId} · {v.variableName}</NativeSelectOption>)}
 </NativeSelect>
}
