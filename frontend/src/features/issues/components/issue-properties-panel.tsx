import { Link, useParams } from 'react-router-dom'
import { ActorAvatar } from '@/components/common/actor-avatar'
import {
  PRIORITY_ORDER,
  PriorityIcon,
  StatusIcon,
  priorityLabelText,
  parseIssuePriority,
} from '@/components/common/issue-badges'
import { Badge } from '@/components/ui/badge'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { ContextRefsPanel } from '@/features/issues/components/context-refs-panel'
import { IssueProjectSelect } from './issue-project-select'
import { workspacePaths } from '@/lib/paths'
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
  saving,
}: {
  issue: Issue
  statuses: ReturnType<typeof useIssueStatuses>['data']
  onCommit: (patch: UpdateIssueInput) => void
  saving: boolean
}) {
  const keys = orderedColumns(statuses ?? [], [issue.status])
  return (
    <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
      <PropertyLabel>状态</PropertyLabel>
      <Select
        value={issue.status}
        disabled={saving}
        onValueChange={(v) => {
          if (v !== null) onCommit({ status: v })
        }}
      >
        <SelectTrigger className="w-full" aria-label="状态">
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
  saving,
}: {
  issue: Issue
  onCommit: (patch: UpdateIssueInput) => void
  saving: boolean
}) {
  return (
    <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
      <PropertyLabel>优先级</PropertyLabel>
      <Select
        value={issue.priority}
        disabled={saving}
        onValueChange={(v) => {
          const next = parseIssuePriority(v)
          if (next !== undefined) onCommit({ priority: next })
        }}
      >
        <SelectTrigger className="w-full" aria-label="优先级">
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
  saving,
}: {
  issue: Issue
  issues: Issue[]
  onCommit: (patch: UpdateIssueInput) => void
  saving: boolean
}) {
  const candidates = issues.filter((i) => i.id !== issue.id)
  return (
    <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
      <PropertyLabel>父任务</PropertyLabel>
      <Select
        value={issue.parentIssueId ?? 'none'}
        disabled={saving}
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
    <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
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
    <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
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

function ProjectField({
  issue,
  onCommit,
  saving,
}: {
  issue: Issue
  onCommit: (patch: UpdateIssueInput) => void
  saving: boolean
}) {
  return (
    <IssueProjectSelect
      id="issue-project"
      value={issue.projectRef || 'none'}
      disabled={saving}
      onChange={(value) => {
        const projectRef = value === 'none' ? '' : value
        if (projectRef !== (issue.projectRef ?? '')) onCommit({ projectRef })
      }}
    />
  )
}

function AgentExecutionEntry() {
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const paths = workspaceSlug ? workspacePaths(workspaceSlug) : undefined
  return (
    <div className="space-y-2">
      <PropertyLabel>Agent 执行</PropertyLabel>
      <p className="text-xs text-muted-foreground">
        在任务输入区选择 Agent 并发送；工作流从任务动态中配置并确认。
      </p>
      {paths && (
        <div className="flex flex-wrap gap-2 text-xs">
          <Link className="underline" to={paths.modelConnections}>
            模型连接设置
          </Link>
          <Link className="underline" to={paths.plugins}>
            管理插件
          </Link>
        </div>
      )}
    </div>
  )
}

function LabelsAndPropertiesFields({ issue }: { issue: Issue }) {
  return (
    <>
      {issue.labels.length > 0 && (
        <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
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
        <div className="grid grid-cols-[70px_minmax(0,1fr)] items-center gap-2">
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
  /** Disables project changes while a versioned task update is awaiting its result. */
  saving?: boolean
}

/**
 * The issue detail page's properties column: status, priority, assignee,
 * parent and sub-issues, project, labels, custom properties and context refs.
 * Edits go through `onCommit`, which owns the version guard; this column only
 * renders the current issue and never fetches the issue itself.
 */
export function IssuePropertiesPanel({
  issue,
  issues,
  statuses,
  members,
  issueHref,
  onCommit,
  saving = false,
}: IssuePropertiesPanelProps) {
  return (
    <div className="w-full space-y-3">
      <StatusField issue={issue} statuses={statuses} onCommit={onCommit} saving={saving} />
      <PriorityField issue={issue} onCommit={onCommit} saving={saving} />
      <AssigneeField issue={issue} members={members} />
      <details>
        <summary className="cursor-pointer text-xs text-muted-foreground">更多属性</summary>
        <div className="mt-3 space-y-3">
          <ParentField issue={issue} issues={issues} onCommit={onCommit} saving={saving} />
          <SubIssuesField issue={issue} issues={issues} issueHref={issueHref} />

          <LabelsAndPropertiesFields issue={issue} />
        </div>
      </details>
      <ProjectField issue={issue} onCommit={onCommit} saving={saving} />
    </div>
  )
}

/** Secondary context references and configuration links stay outside the compact property editor. */
export function IssueContextDetails({ tid, issueId }: { tid: string; issueId: string }) {
  return (
    <details className="space-y-3 border-t pt-4">
      <summary className="cursor-pointer text-sm font-medium">上下文与设置</summary>
      <ContextRefsPanel slug={tid} issueId={issueId} />
      <AgentExecutionEntry />
    </details>
  )
}
