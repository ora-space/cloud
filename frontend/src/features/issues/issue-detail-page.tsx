import { Trash2 } from 'lucide-react'
import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { IssueWorkspace } from '@/features/issues/components/issue-workspace'
import { ConversationMarkdown } from '@/features/conversation-content/markdown'
import {
  IssuePropertiesPanel,
  IssueContextDetails,
} from '@/features/issues/components/issue-properties-panel'
import {
  useDeleteIssue,
  useIssue,
  useIssues,
  useIssueStatuses,
  useMembers,
  useUpdateIssue,
  type UpdateIssueInput,
} from '@/features/issues/api'
import { issueNumber } from '@/features/issues/present'
import { workspacePaths } from '@/lib/paths'
import { faultCode, isUnauthorizedError, isForbiddenError } from '@/lib/api-client'
import { useProjects } from '@/features/projects/api'
import type { Issue, TenantMember } from './types'

/** Composes the task heading, versioned property edits and persistent execution workspace. */
export function IssueDetailPage({ slug }: { slug: string }) {
  const { issueId, workspaceSlug } = useParams<{ issueId: string; workspaceSlug: string }>()
  const navigate = useNavigate()
  const issueQuery = useIssue(slug, issueId)
  const { data: issue, isPending } = issueQuery
  const { data: statuses } = useIssueStatuses(slug)
  const { data: members = [] } = useMembers(slug)
  const { data: allIssues = [] } = useIssues(slug)
  const updateIssue = useUpdateIssue(slug)
  const [reloading, setReloading] = useState(false)
  const propertiesBusy = updateIssue.isPending || reloading
  const deleteIssue = useDeleteIssue(slug)
  const p = workspacePaths(workspaceSlug ?? slug)

  const inaccessible =
    isUnauthorizedError(issueQuery.error) ||
    isForbiddenError(issueQuery.error) ||
    faultCode(issueQuery.error) === 'not_found'
  if (issueQuery.isError && (!issue || inaccessible))
    return (
      <TaskReadFailure
        issuesHref={p.issues}
        error={issueQuery.error}
        retrying={issueQuery.isFetching}
        retry={() => void issueQuery.refetch()}
      />
    )
  if (isPending || !issue) return <TaskLoading issuesHref={p.issues} />

  const commit = (patch: UpdateIssueInput) => {
    if (propertiesBusy) return
    updateIssue.mutate({ id: issue.id, version: issue.version, patch })
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        title={
          <span title={issue.title}>
            {issueNumber(issue)} · {issue.title}
          </span>
        }
        breadcrumb={{ label: '任务', to: p.issues }}
        actions={
          <Button
            variant="ghost"
            size="icon"
            onClick={() =>
              deleteIssue.mutate(
                { id: issue.id, version: issue.version },
                { onSuccess: () => navigate(p.issues) },
              )
            }
          >
            <Trash2 className="size-4" />
          </Button>
        }
      />
      <IssueWorkspace
        key={issue.id}
        tid={slug}
        issueId={issue.id}
        members={members}
        heading={
          <>
            <TaskHeading issue={issue} />
            {updateIssue.isError && (
              <TaskSaveFailure
                busy={propertiesBusy}
                reload={() => {
                  setReloading(true)
                  void issueQuery.refetch().then((result) => {
                    if (!result.isError) updateIssue.reset()
                    setReloading(false)
                    return result
                  })
                }}
              />
            )}
          </>
        }
        properties={
          <IssuePropertiesPanel
            tid={slug}
            issue={issue}
            issues={allIssues}
            statuses={statuses}
            members={members}
            issueHref={p.issueDetail}
            onCommit={commit}
            saving={propertiesBusy}
          />
        }
        details={<TaskMetadata issue={issue} members={members} tid={slug} />}
      />
    </div>
  )
}

function TaskSaveFailure({ busy, reload }: { busy: boolean; reload: () => void }) {
  return (
    <div role="alert" className="text-sm text-destructive">
      <p>保存任务失败，请重新加载任务确认当前状态后重试。</p>
      <Button type="button" variant="outline" size="sm" disabled={busy} onClick={reload}>
        重新加载任务
      </Button>
    </div>
  )
}

function TaskMetadata({
  issue,
  members,
  tid,
}: {
  issue: Issue
  members: TenantMember[]
  tid: string
}) {
  return (
    <>
      <section className="space-y-2 border-t pt-4 text-xs text-muted-foreground">
        <h2 className="text-sm font-medium text-foreground">详情</h2>
        <p>
          创建者：
          {members.find((member) => member.userId === issue.creatorUserId)?.displayName ?? '用户'}
        </p>
        <p>创建时间：{new Date(issue.createdAt).toLocaleString()}</p>
        <p>更新时间：{new Date(issue.updatedAt).toLocaleString()}</p>
      </section>
      <IssueContextDetails tid={tid} issueId={issue.id} />
    </>
  )
}

function TaskHeading({ issue }: { issue: Issue }) {
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const projects = useProjects(workspaceSlug ?? '')
  const project = projects.data?.find((item) => item.id === issue.projectRef)
  return (
    <>
      {project && workspaceSlug && (
        <Link
          className="text-xs text-muted-foreground hover:underline"
          to={workspacePaths(workspaceSlug).projectDetail(project.id)}
        >
          {project.title}
        </Link>
      )}
      <h1 className="truncate text-xl font-semibold" title={issue.title}>
        {issue.title}
      </h1>
      <details className="text-sm">
        <summary className="cursor-pointer text-muted-foreground">任务说明</summary>
        <div className="max-h-40 overflow-y-auto py-2">
          <ConversationMarkdown text={issue.description || '暂无描述。'} />
        </div>
      </details>
    </>
  )
}

function TaskReadFailure({
  issuesHref,
  error,
  retrying,
  retry,
}: {
  issuesHref: string
  error: unknown
  retrying: boolean
  retry: () => void
}) {
  if (isUnauthorizedError(error) || isForbiddenError(error))
    return (
      <p role="alert" className="p-6">
        任务读取失败或已不可访问，请刷新重试。
      </p>
    )
  const missing = faultCode(error) === 'not_found'
  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader title="任务" breadcrumb={{ label: '任务', to: issuesHref }} />
      <div className="space-y-3 p-6">
        <p role="alert">{missing ? '任务不存在或已不可访问。' : '任务暂时无法加载，请重试。'}</p>
        <div className="flex items-center gap-3">
          {!missing && (
            <Button onClick={retry} disabled={retrying} variant="outline" size="sm">
              重试加载任务
            </Button>
          )}
          <Link to={issuesHref} className="text-sm underline">
            返回任务列表
          </Link>
        </div>
      </div>
    </div>
  )
}

function TaskLoading({ issuesHref }: { issuesHref: string }) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader title="任务" breadcrumb={{ label: '任务', to: issuesHref }} />
      <div role="status" aria-label="正在加载任务" className="space-y-3 p-6">
        <Skeleton className="h-6 w-2/3" />
        <Skeleton className="h-24 w-full" />
      </div>
    </div>
  )
}
