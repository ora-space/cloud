import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflow } from '@/features/workflows/api'
import { WorkflowDetailShell } from '@/features/workflows/workflow-detail-shell'
import { WorkflowEditor } from '@/features/workflows/editor/workflow-editor'
import { workspacePaths } from '@/lib/paths'

/**
 * The 「编辑」 tab of a workflow: the canvas, under the workflow's own name.
 *
 * The page owns the fetch and nothing else: it resolves the workflow, then hands
 * the loaded record to the editor, which owns the draft from that point on. The
 * editor is keyed by workflow id so opening a different workflow starts a fresh
 * draft instead of carrying the previous one's nodes across.
 *
 * @param props.slug - Tenant id the workflow belongs to.
 */
export function WorkflowEditorPage({ slug }: { slug: string }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const { workspaceSlug, workflowId } = useParams<{ workspaceSlug: string; workflowId: string }>()
  const paths = workspacePaths(workspaceSlug ?? slug)
  const workflow = useWorkflow(slug, workflowId)
  const title = t('workflows.list.title')
  const id = workflowId ?? ''

  if (workflow.isPending) {
    return (
      <WorkflowDetailShell title={title} workflowId={id}>
        <div className="flex-1 p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      </WorkflowDetailShell>
    )
  }

  if (!workflow.data) {
    return (
      <WorkflowDetailShell title={title} workflowId={id}>
        <p className="p-4 text-sm text-muted-foreground">{t('workflows.detail.notFound')}</p>
      </WorkflowDetailShell>
    )
  }

  return (
    <WorkflowDetailShell title={workflow.data.name} workflowId={workflow.data.id}>
      <WorkflowEditor
        key={workflow.data.id}
        tenantId={slug}
        workflow={workflow.data}
        guardNavigation
        onImported={(importedId) => navigate(paths.workflowDetail(importedId))}
      />
    </WorkflowDetailShell>
  )
}
