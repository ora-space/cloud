import { Play, ScrollText } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Link, useNavigate, useParams } from 'react-router-dom'
import type { WorkflowRun } from '@/api/generated.schemas'
import { Button, buttonVariants } from '@/components/ui/button'
import { useWorkflowSnapshots } from '@/features/workflows/api'
import { WorkflowRunDialog } from '@/features/workflows/editor/workflow-run-dialog'
import { workspacePaths } from '@/lib/paths'

/**
 * The toolbar's run controls: start one run, or open the run history.
 *
 * A run executes against a published snapshot, so the primary button is disabled while the
 * workflow has none — there is nothing honest to execute a draft against. Starting a run opens
 * {@link WorkflowRunDialog}, which is where the version and the run's own input are chosen; a
 * started run then navigates to its read-only detail page.
 *
 * Browsing history is a link, not a popover: the history has its own tab, so the same URL a member
 * can share is the one this control opens, and opening it never starts anything.
 *
 * @param props.tenantId - Tenant the workflow belongs to; scopes the API calls only, never a
 * browser path segment (the route's workspace slug is the one paths are built from).
 * @param props.workflowId - Workflow being edited.
 */
export function WorkflowRunTools({
  tenantId,
  workflowId,
}: {
  tenantId: string
  workflowId: string
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  // `tenantId` scopes the API calls; the browser path must carry the workspace slug from the
  // route instead, or the Layout cannot resolve the workspace and bounces the member to Issues
  // (IssueCard follows the same rule). Only a bare mount outside the workspace route has no slug.
  const { workspaceSlug } = useParams<{ workspaceSlug: string }>()
  const paths = workspacePaths(workspaceSlug ?? tenantId)
  const snapshots = useWorkflowSnapshots(tenantId, workflowId)
  const [startOpen, setStartOpen] = useState(false)

  const noSnapshot = snapshots.data !== undefined && snapshots.data.length === 0

  return (
    <>
      <Button
        variant="default"
        size="sm"
        disabled={noSnapshot}
        title={noSnapshot ? t('workflows.run.noSnapshotHint') : t('workflows.run.runHint')}
        onClick={() => setStartOpen(true)}
      >
        <Play className="size-3.5" fill="currentColor" />
        {t('workflows.run.runNow')}
      </Button>
      <Link
        to={paths.workflowRuns(workflowId)}
        title={t('workflows.run.historyHint')}
        className={buttonVariants({ variant: 'outline', size: 'sm' })}
      >
        <ScrollText className="size-4" />
        {t('workflows.run.history')}
      </Link>
      <WorkflowRunDialog
        tenantId={tenantId}
        workflowId={workflowId}
        open={startOpen}
        onOpenChange={setStartOpen}
        onStarted={openStartedRun}
      />
    </>
  )

  /** Leaves the dialog for the run the server just created. */
  function openStartedRun(run: WorkflowRun): void {
    setStartOpen(false)
    void navigate(paths.workflowRun(run.workflowId, run.id))
  }
}
