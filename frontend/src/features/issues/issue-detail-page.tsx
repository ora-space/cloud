import { Trash2 } from 'lucide-react'
import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { PageHeader } from '@/components/layout/page-header'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { ActivityPanel } from '@/features/issues/components/activity-panel'
import { IssuePropertiesPanel } from '@/features/issues/components/issue-properties-panel'
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
import { IssueThreadPanel } from '@/features/issues/thread/thread-panel'
import { workspacePaths } from '@/lib/paths'

/**
 * One issue: the description and activity column, the properties column and,
 * when the issue has an agent run, the "Agent 会话" Thread column. The `slug`
 * prop is the tenant id (forwarded by CloudScope); navigation links carry the
 * space slug from the route instead.
 */
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

  if (isPending || !issue) {
    return (
      <div className="flex h-full flex-col">
        <PageHeader title="任务" breadcrumb={{ label: '任务', to: p.issues }} />
        <div className="space-y-3 p-6">
          <Skeleton className="h-6 w-2/3" />
          <Skeleton className="h-24 w-full" />
        </div>
      </div>
    )
  }

  const commit = (patch: UpdateIssueInput) => {
    if (propertiesBusy) return
    updateIssue.mutate({ id: issue.id, version: issue.version, patch })
  }

  return (
    <div className="flex h-full flex-col">
      <PageHeader
        title={issueNumber(issue)}
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
      <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6 md:flex-row">
        <div className="min-w-0 flex-1 space-y-4">
          <h1 className="text-xl font-semibold">{issue.title}</h1>
          <p className="whitespace-pre-wrap text-sm text-muted-foreground">
            {issue.description || '暂无描述。'}
          </p>
          {updateIssue.isError && (
            <div role="alert" className="space-y-1 text-sm text-destructive">
              <p>保存任务失败，请重新加载任务确认当前状态后重试。</p>
              <Button
                type="button"
                variant="outline"
                size="sm"
                disabled={propertiesBusy}
                onClick={() => {
                  setReloading(true)
                  void issueQuery.refetch().then((result) => {
                    if (!result.isError) updateIssue.reset()
                    setReloading(false)
                    return result
                  })
                }}
              >
                重新加载任务
              </Button>
            </div>
          )}
          <ActivityPanel slug={slug} issueId={issue.id} members={members} />
        </div>
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
        <IssueThreadPanel tid={slug} issueId={issue.id} members={members} />
      </div>
    </div>
  )
}
