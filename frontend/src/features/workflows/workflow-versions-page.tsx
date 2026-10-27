import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflow } from '@/features/workflows/api'
import { WorkflowVersionHistory } from '@/features/workflows/editor/workflow-version-history'
import { WorkflowDetailShell } from '@/features/workflows/workflow-detail-shell'
import { workspacePaths } from '@/lib/paths'

/**
 * The 「版本」 tab: every frozen version of this workflow, with rollback.
 *
 * A restore replaces the live document, so a successful rollback lands on the 「编辑」 tab — the
 * member sees the graph they just restored instead of staying on a list that would have to describe
 * the result. The document version the restore guards on is the one this page read from the server,
 * which is the right authority here: the editor is not mounted, so there is no draft to flush.
 *
 * @param props.slug - Tenant id the workflow belongs to.
 */
export function WorkflowVersionsPage({ slug }: { slug: string }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { workspaceSlug, workflowId } = useParams<{ workspaceSlug: string; workflowId: string }>()
  const paths = workspacePaths(workspaceSlug ?? slug)
  const id = workflowId ?? ''
  const workflow = useWorkflow(slug, workflowId)

  return (
    <WorkflowDetailShell title={workflow.data?.name ?? t('workflows.list.title')} workflowId={id}>
      <div className="flex-1 overflow-y-auto p-4">
        {workflow.isPending && <Skeleton className="h-24 w-full" />}
        {!workflow.isPending && !workflow.data && (
          <p className="text-sm text-muted-foreground">{t('workflows.detail.notFound')}</p>
        )}
        {workflow.data && (
          <WorkflowVersionHistory
            tenantId={slug}
            workflowId={workflow.data.id}
            committedVersion={workflow.data.version}
            onRestored={() => void navigate(paths.workflowDetail(workflow.data.id))}
          />
        )}
      </div>
    </WorkflowDetailShell>
  )
}
