import { Trash2 } from 'lucide-react'
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
  const { data: issue, isPending } = useIssue(slug, issueId)
  const { data: statuses } = useIssueStatuses(slug)
  const { data: members = [] } = useMembers(slug)
  const { data: allIssues = [] } = useIssues(slug)
  const updateIssue = useUpdateIssue(slug)
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

  const commit = (patch: UpdateIssueInput) =>
    updateIssue.mutate({ id: issue.id, version: issue.version, patch })

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
        />
        <IssueThreadPanel tid={slug} issueId={issue.id} />
      </div>
    </div>
  )
}
