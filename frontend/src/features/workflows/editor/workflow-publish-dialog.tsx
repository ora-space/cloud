import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { Rocket } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { usePublishWorkflow } from '@/features/workflows/api'
import { WorkflowPublishError } from '@/features/workflows/editor/workflow-publish-error'

/**
 * Confirms publishing the workflow's live graph as the next immutable version.
 *
 * The editor flushes its draft before opening this dialog, so the version that
 * gets frozen is the one the author last saw on the canvas. The version name is
 * optional — the backend defaults it to the workflow name — and the confirm is
 * an idempotent POST, so retrying a publish never mints a duplicate version.
 *
 * @param props.tenantId - Tenant the workflow belongs to.
 * @param props.workflowId - Workflow to publish.
 * @param props.open - Whether the dialog is showing.
 * @param props.onOpenChange - Receives the next open state.
 */
export function WorkflowPublishDialog({
  tenantId,
  workflowId,
  open,
  onOpenChange,
}: {
  tenantId: string
  workflowId: string
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const { t } = useTranslation()
  const publish = usePublishWorkflow(tenantId)
  const [name, setName] = useState('')

  /** Closing the dialog blanks the name so the next publish starts fresh. */
  function handleOpenChange(next: boolean): void {
    if (!next) {
      setName('')
    }
    onOpenChange(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{t('workflows.version.publishTitle')}</DialogTitle>
          <DialogDescription>{t('workflows.version.publishDescription')}</DialogDescription>
        </DialogHeader>
        <label className="text-sm font-medium" htmlFor="workflow-version-name">
          {t('workflows.version.versionName')}
        </label>
        <Input
          id="workflow-version-name"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder={t('workflows.version.versionPlaceholder')}
          autoFocus
          onKeyDown={(event) => {
            if (event.key === 'Enter') {
              event.preventDefault()
              void confirm()
            }
          }}
        />
        {publish.error && <WorkflowPublishError fault={publish.error.response?.data} />}
        <DialogFooter>
          <Button variant="outline" onClick={() => handleOpenChange(false)}>
            {t('workflows.list.cancel')}
          </Button>
          <Button disabled={publish.isPending} onClick={() => void confirm()}>
            <Rocket className="size-3.5" />
            {t('workflows.version.publish')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )

  /** Freezes the live graph as the next version, then closes on success. */
  async function confirm(): Promise<void> {
    try {
      await publish.mutateAsync({
        workflowId,
        name: name.trim(),
      })
      setName('')
      onOpenChange(false)
    } catch {
      // the fault line above the footer already reports it
    }
  }
}
