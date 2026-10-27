import { Rocket } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Link, useParams } from 'react-router-dom'
import { Button, buttonVariants } from '@/components/ui/button'
import { WorkflowPublishDialog } from '@/features/workflows/editor/workflow-publish-dialog'
import { workspacePaths } from '@/lib/paths'

/**
 * The toolbar's publish control, plus the way to the 「版本」 tab.
 *
 * Publishing first flushes the draft so the frozen version is never stale: a save that could not
 * persist (a version conflict, a dropped connection) blocks the publish dialog instead of freezing
 * the previous content. The version list itself lives on its own tab rather than in a popover, so
 * one place answers "what versions exist" and the same place rolls back to them.
 *
 * @param props.tenantId - Tenant the workflow belongs to.
 * @param props.workflowId - Workflow being edited.
 * @param props.flushDraft - Writes the draft to completion before publishing.
 */
export function WorkflowVersionTools({
  tenantId,
  workflowId,
  flushDraft,
}: {
  tenantId: string
  workflowId: string
  flushDraft: () => Promise<boolean>
}) {
  const { t } = useTranslation()
  const { workspaceSlug } = useParams<{ workspaceSlug?: string }>()
  const paths = workspacePaths(workspaceSlug ?? tenantId)
  const [publishOpen, setPublishOpen] = useState(false)

  return (
    <div className="flex items-center gap-2">
      <Button
        variant="outline"
        size="sm"
        title={t('workflows.version.publishHint')}
        onClick={() => void openPublish()}
      >
        <Rocket className="size-3.5" />
        {t('workflows.version.publish')}
      </Button>
      <Link
        to={paths.workflowVersions(workflowId)}
        title={t('workflows.version.historyHint')}
        className={buttonVariants({ variant: 'outline', size: 'sm' })}
      >
        {t('workflows.version.history')}
      </Link>
      <WorkflowPublishDialog
        tenantId={tenantId}
        workflowId={workflowId}
        open={publishOpen}
        onOpenChange={setPublishOpen}
      />
    </div>
  )

  /** Persists the draft, then lets a member turn it into a frozen version. */
  async function openPublish(): Promise<void> {
    const saved = await flushDraft()
    if (!saved) {
      return
    }
    setPublishOpen(true)
  }
}
