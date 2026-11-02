import { ChevronRight, Folder, GitBranch, ListTodo, MessageSquare } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Switch } from '@/components/ui/switch'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { cn } from '@/lib/utils'
import { catalog, type Catalog, formatTime, isWorkflowTarget, nextTimes, projectName, ruleText, type Task } from './schedule-model'
import { RunStatus } from './schedule-detail'

export function ScheduleTable({ data = catalog, tasks, now, onOpen, onToggle, toggling = false }: { data?: Catalog; tasks: Task[]; now: Date; onOpen: (task: Task, tab?: 'config' | 'history') => void; onToggle: (task: Task) => void; toggling?: boolean }) {
  return <div className="sp-table-surface"><Table className="sp-table">
    <TableHeader><TableRow><TableHead>任务名称</TableHead><TableHead>执行方式</TableHead><TableHead>关联项目</TableHead><TableHead>时间安排</TableHead><TableHead>运行记录</TableHead><TableHead>启用</TableHead><TableHead><span className="sp-sr-only">详情</span></TableHead></TableRow></TableHeader>
    <TableBody>{tasks.map(task => {
      const due = task.enabled ? nextTimes(task.rule, now, new Date(task.createdAt))[0] : undefined
      const latest = task.runs[0]
      const target = task.target
      const Icon = target.kind === 'chat' ? MessageSquare : target.kind === 'task' ? ListTodo : GitBranch
      const label = target.kind === 'chat' ? '直接对话' : target.kind === 'task' ? '已有任务' : '工作流'
      return <TableRow key={task.id} className="sp-task-row" data-paused={!task.enabled}>
        <TableCell><button className="sp-task-cell" onClick={() => onOpen(task)}>
          <span className={cn('sp-task-icon', target.kind !== 'chat' && 'sp-task-icon-workflow')}><Icon size={17} /></span>
          <span><strong>{task.name}</strong><small>{target.kind === 'chat' ? '每次创建独立对话' : target.kind === 'task' ? '已有任务待接入' : isWorkflowTarget(target) ? '每次新建一次工作流运行' : '旧的工作流引用，仅供查看'}</small></span>
        </button></TableCell>
        <TableCell><Badge variant="outline"><Icon data-icon="inline-start" />{label}</Badge></TableCell>
        <TableCell><span className={cn('sp-project-label', !task.projectId && 'muted')}>{task.projectId && <Folder size={14} />}{projectName(task.projectId,data)}</span></TableCell>
        <TableCell><div className="sp-cell-stack"><span>{ruleText(task.rule)}</span><small>{due ? `预计 ${formatTime(due, task.rule.timezone)}` : '已暂停自动运行'}</small></div></TableCell>
        <TableCell><button className="sp-run-cell" onClick={() => onOpen(task, 'history')}><RunStatus run={latest} /><small>{latest ? formatTime(latest.startedAt, task.rule.timezone) : '查看服务端状态'}</small></button></TableCell>
        <TableCell><Switch disabled={toggling} checked={task.enabled} onCheckedChange={() => onToggle(task)} aria-label={`${task.name}启用状态`} /></TableCell>
        <TableCell><Button variant="ghost" size="icon-sm" aria-label={`查看${task.name}`} onClick={() => onOpen(task)}><ChevronRight /></Button></TableCell>
      </TableRow>
    })}</TableBody>
  </Table></div>
}
