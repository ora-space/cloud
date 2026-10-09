import { Link } from 'react-router-dom'
import { ActorAvatar } from '@/components/common/actor-avatar'
import {
  PRIORITY_ORDER,
  PriorityIcon,
  StatusIcon,
  priorityLabelText,
  parseIssuePriority,
} from '@/components/common/issue-badges'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { ContextRefsPanel } from '@/features/issues/components/context-refs-panel'
import type { UpdateIssueInput, useIssueStatuses } from '@/features/issues/api'
import {
  assigneeName,
  assigneeType,
  columnLabel,
  issueNumber,
  memberNameById,
  orderedColumns,
} from '@/features/issues/present'
import type { Issue, TenantMember } from '@/features/issues/types'

function PropertyLabel({ children }: { children: React.ReactNode }) {
  return <p className="text-xs font-medium text-muted-foreground">{children}</p>
}

function StatusField({
  issue,
  statuses,
  onCommit,
}: {
  issue: Issue
  statuses: ReturnType<typeof useIssueStatuses>['data']
  onCommit: (patch: UpdateIssueInput) => void
}) {
  const keys = orderedColumns(statuses ?? [], [issue.status])
  return (
    <div className="space-y-1.5">
      <PropertyLabel>状态</PropertyLabel>
      <Select
        value={issue.status}
        onValueChange={(v) => {
          if (v !== null) onCommit({ status: v })
        }}
      >
        <SelectTrigger className="w-full">
          <span className="flex items-center gap-2">
            <StatusIcon status={issue.status} />
            <SelectValue>
              {(value: unknown) =>
                columnLabel(statuses ?? [], typeof value === 'string' ? value : issue.status)
              }
            </SelectValue>
          </span>
        </SelectTrigger>
        <SelectContent>
          {keys.map((key) => (
            <SelectItem key={key} value={key}>
              {columnLabel(statuses ?? [], key)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

function PriorityField({
  issue,
  onCommit,
}: {
  issue: Issue
  onCommit: (patch: UpdateIssueInput) => void
}) {
  return (
    <div className="space-y-1.5">
      <PropertyLabel>优先级</PropertyLabel>
      <Select
        value={issue.priority}
        onValueChange={(v) => {
          const next = parseIssuePriority(v)
          if (next !== undefined) onCommit({ priority: next })
        }}
      >
        <SelectTrigger className="w-full">
          <span className="flex items-center gap-2">
            <PriorityIcon priority={issue.priority} />
            <SelectValue>
              {(value: unknown) => priorityLabelText(parseIssuePriority(value) ?? issue.priority)}
            </SelectValue>
          </span>
        </SelectTrigger>
        <SelectContent>
          {PRIORITY_ORDER.map((pr) => (
            <SelectItem key={pr} value={pr}>
              {priorityLabelText(pr)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

function ParentField({
  issue,
  issues,
  onCommit,
}: {
  issue: Issue
  issues: Issue[]
  onCommit: (patch: UpdateIssueInput) => void
}) {
  const candidates = issues.filter((i) => i.id !== issue.id)
  return (
    <div className="space-y-1.5">
      <PropertyLabel>父任务</PropertyLabel>
      <Select
        value={issue.parentIssueId ?? 'none'}
        onValueChange={(v) => {
          if (v !== null) onCommit({ parentIssueId: v === 'none' ? '' : v })
        }}
      >
        <SelectTrigger className="w-full">
          <SelectValue placeholder="无父任务">
            {(value: unknown) =>
              value === 'none' || value == null
                ? '无父任务'
                : (issues.find((i) => i.id === value)?.title ?? '父任务')
            }
          </SelectValue>
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="none">无父任务</SelectItem>
          {candidates.map((i) => (
            <SelectItem key={i.id} value={i.id}>
              {issueNumber(i)} {i.title}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  )
}

function AssigneeField({ issue, members }: { issue: Issue; members: TenantMember[] }) {
  const names = memberNameById(members)
  const type = assigneeType(issue)
  return (
    <div className="space-y-1.5">
      <PropertyLabel>负责人</PropertyLabel>
      <div className="flex items-center gap-2 rounded-md border px-2.5 py-1.5 text-sm">
        <ActorAvatar
          actor={type ? { name: assigneeName(issue, names), type } : undefined}
          size="sm"
        />
        <span className="truncate">{assigneeName(issue, names)}</span>
        {type === 'agent' || type === 'team' ? (
          <span className="shrink-0 text-xs text-muted-foreground">暂不可用</span>
        ) : null}
      </div>
    </div>
  )
}

function SubIssuesField({
  issue,
  issues,
  issueHref,
}: {
  issue: Issue
  issues: Issue[]
  issueHref: (id: string) => string
}) {
  const subIssues = issues.filter((i) => i.parentIssueId === issue.id)
  return (
    <div className="space-y-1.5">
      <PropertyLabel>子任务</PropertyLabel>
      {subIssues.length === 0 ? (
        <p className="text-sm text-muted-foreground">暂无子任务</p>
      ) : (
        <ul className="space-y-1">
          {subIssues.map((sub) => (
            <li key={sub.id}>
              <Link
                to={issueHref(sub.id)}
                className="flex items-center gap-1.5 text-sm hover:underline"
              >
                <span className="shrink-0 text-xs text-muted-foreground">{issueNumber(sub)}</span>
                <span className="truncate">{sub.title}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function ProjectField({ issue }: { issue: Issue }) {
  return (
    <div className="space-y-1.5">
      <PropertyLabel>项目</PropertyLabel>
      {issue.projectRef ? (
        <div className="text-sm">
          <p className="text-muted-foreground">项目详情暂不可用</p>
          <p className="truncate font-mono text-xs text-muted-foreground">{issue.projectRef}</p>
        </div>
      ) : (
        <p className="text-sm text-muted-foreground">未关联项目</p>
      )}
    </div>
  )
}

function LabelsAndPropertiesFields({ issue }: { issue: Issue }) {
  return (
    <>
      {issue.labels.length > 0 && (
        <div className="space-y-1.5">
          <PropertyLabel>标签</PropertyLabel>
          <div className="flex flex-wrap gap-1.5">
            {issue.labels.map((label) => (
              <Badge key={label.id} variant="secondary">
                {label.name}
              </Badge>
            ))}
          </div>
        </div>
      )}
      {Object.keys(issue.properties).length > 0 && (
        <div className="space-y-1.5">
          <PropertyLabel>属性</PropertyLabel>
          <dl className="space-y-1 text-sm">
            {Object.entries(issue.properties).map(([key, value]) => (
              <div key={key} className="flex justify-between gap-2">
                <dt className="text-muted-foreground">{key}</dt>
                <dd className="min-w-0 truncate">{String(value)}</dd>
              </div>
            ))}
          </dl>
        </div>
      )}
    </>
  )
}

/** Inputs of the issue properties column; `onCommit` sends one versioned patch. */
export interface IssuePropertiesPanelProps {
  tid: string
  issue: Issue
  issues: Issue[]
  statuses: ReturnType<typeof useIssueStatuses>['data']
  members: TenantMember[]
  issueHref: (id: string) => string
  onCommit: (patch: UpdateIssueInput) => void
}

/**
 * The issue detail page's properties column: status, priority, assignee,
 * parent and sub-issues, project, labels, custom properties and context refs.
 * Edits go through `onCommit`, which owns the version guard; this column only
 * renders the current issue and never fetches the issue itself.
 */
export function IssuePropertiesPanel({
  tid,
  issue,
  issues,
  statuses,
  members,
  issueHref,
  onCommit,
}: IssuePropertiesPanelProps) {
  return (
    <div className="w-full shrink-0 space-y-4 md:w-64">
      <StatusField issue={issue} statuses={statuses} onCommit={onCommit} />
      <PriorityField issue={issue} onCommit={onCommit} />
      <AssigneeField issue={issue} members={members} />
      <ParentField issue={issue} issues={issues} onCommit={onCommit} />
      <SubIssuesField issue={issue} issues={issues} issueHref={issueHref} />
      <ProjectField issue={issue} />
      <LabelsAndPropertiesFields issue={issue} />
      <div className="space-y-1.5">
        <PropertyLabel>上下文引用</PropertyLabel>
        <ContextRefsPanel slug={tid} issueId={issue.id} />
      </div>
      <div className="space-y-1.5">
        <PropertyLabel>执行</PropertyLabel>
        <Button variant="outline" size="sm" className="w-full" disabled>
          运行（暂不可用）
        </Button>
        <p className="text-xs text-muted-foreground">Agent / Team / Workflow 执行尚未接入。</p>
      </div>
    </div>
  )
}
