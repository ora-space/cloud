import { format } from 'date-fns'
import { RotateCcw, Search } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import type { Workflow as CloudWorkflow, WorkflowSnapshot } from '@/api/generated.schemas'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Skeleton } from '@/components/ui/skeleton'
import { useRestoreWorkflowSnapshot, useWorkflowSnapshots } from '@/features/workflows/api'
import { workflowErrorKey } from '@/features/workflows/error-messages'

/**
 * The workflow's published versions, newest first, each with a rollback action.
 *
 * Every snapshot is an immutable freeze of the graph at publish time; there is no active-version
 * mark, so "rolling back" means restoring that graph over the live document — an edit that bumps
 * the document version like any other write. A confirmation guards the action because it replaces
 * whatever is on the canvas right now, unsaved changes included; this panel is the 「版本」 tab, so
 * the member reaches it after the editor has written its draft on the way out.
 *
 * A published version is what a run executes, which is why this surface stays separate from the
 * run history: one lists what the workflow *can* run, the other what it *did* run.
 *
 * @param props.tenantId - Tenant the workflow belongs to.
 * @param props.workflowId - Workflow whose versions are shown.
 * @param props.committedVersion - Document version the server confirmed; a stale value makes the
 *   restore a 409 rather than clobbering another editor.
 * @param props.onRestored - Receives the workflow once its graph was replaced.
 */
export function WorkflowVersionHistory({
  tenantId,
  workflowId,
  committedVersion,
  onRestored,
}: {
  tenantId: string
  workflowId: string
  committedVersion: number
  onRestored: (workflow: CloudWorkflow) => void
}) {
  const snapshots = useWorkflowSnapshots(tenantId, workflowId)
  const restore = useRestoreWorkflowSnapshot(tenantId)
  const [query, setQuery] = useState('')
  const [restoreTarget, setRestoreTarget] = useState<WorkflowSnapshot | null>(null)

  const visible = useMemo(() => {
    const list = snapshots.data ?? []
    const needle = query.trim().toLowerCase()
    if (needle === '') {
      return list
    }
    return list.filter(
      (snapshot) =>
        snapshot.name.toLowerCase().includes(needle) || String(snapshot.version).includes(needle),
    )
  }, [query, snapshots.data])

  return (
    <div className="space-y-3">
      <VersionHistorySearch query={query} onQueryChange={setQuery} />
      <VersionHistoryList
        snapshots={visible}
        isPending={snapshots.isPending}
        onRollback={setRestoreTarget}
      />
      <RestoreSnapshotDialog
        target={restoreTarget}
        isPending={restore.isPending}
        errorCode={restore.error?.response?.data?.code}
        onSettle={setRestoreTarget}
        onConfirm={(snapshot) => void confirmRestore(snapshot)}
      />
    </div>
  )

  /** Writes the chosen snapshot's graph over the live document, then reports the result. */
  async function confirmRestore(snapshot: WorkflowSnapshot): Promise<void> {
    try {
      const restored = await restore.mutateAsync({
        workflowId,
        snapshotId: snapshot.id,
        version: committedVersion,
      })
      setRestoreTarget(null)
      onRestored(restored)
    } catch {
      // the fault line above the dialog footer already reports it
    }
  }
}

/** The version-name search box above the list. */
function VersionHistorySearch({
  query,
  onQueryChange,
}: {
  query: string
  onQueryChange: (query: string) => void
}) {
  const { t } = useTranslation()
  return (
    <div className="relative max-w-sm">
      <Search className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground" />
      <Input
        value={query}
        onChange={(event) => onQueryChange(event.target.value)}
        aria-label={t('workflows.version.historySearch')}
        placeholder={t('workflows.version.historySearch')}
        className="h-8 pl-8 text-xs"
      />
    </div>
  )
}

/** The snapshot list body: loading, empty, or the rollback rows. */
function VersionHistoryList({
  snapshots,
  isPending,
  onRollback,
}: {
  snapshots: WorkflowSnapshot[]
  isPending: boolean
  onRollback: (snapshot: WorkflowSnapshot) => void
}) {
  const { t } = useTranslation()
  if (isPending) {
    return <Skeleton className="h-24 w-full" />
  }
  if (snapshots.length === 0) {
    return (
      <div className="rounded-lg border border-dashed p-8 text-center">
        <p className="text-sm font-medium">{t('workflows.version.historyEmpty')}</p>
      </div>
    )
  }
  return (
    <ul className="space-y-2">
      {snapshots.map((snapshot) => (
        <li key={snapshot.id}>
          <button
            type="button"
            aria-label={t('workflows.version.rollbackNamed', {
              version: snapshot.version,
            })}
            onClick={() => onRollback(snapshot)}
            className="flex w-full items-center gap-3 rounded-lg border px-3 py-2 text-left transition-colors hover:bg-muted/50"
          >
            <span className="min-w-0 flex-1 truncate text-sm font-medium">
              #{snapshot.version} {snapshot.name}
            </span>
            <span className="shrink-0 text-xs text-muted-foreground">
              {t('workflows.version.publishedAt', {
                time: format(new Date(snapshot.createdAt), 'M月d日 HH:mm'),
              })}
            </span>
            <RotateCcw className="size-3.5 shrink-0 text-muted-foreground" />
          </button>
        </li>
      ))}
    </ul>
  )
}

/** The confirmation guarding a rollback, with a fault line for restore errors. */
function RestoreSnapshotDialog({
  target,
  isPending,
  errorCode,
  onSettle,
  onConfirm,
}: {
  /** The snapshot about to be restored, or `null` while the dialog is closed. */
  target: WorkflowSnapshot | null
  /** Whether the restore request is in flight. */
  isPending: boolean
  /** Backend fault code from the last failed restore, if any. */
  errorCode: string | undefined
  /** Receives the new dialog state; `null` closes it. */
  onSettle: (target: WorkflowSnapshot | null) => void
  /** Carries the confirmed snapshot to the restore call. */
  onConfirm: (snapshot: WorkflowSnapshot) => void
}) {
  const { t } = useTranslation()
  return (
    <AlertDialog
      open={target !== null}
      onOpenChange={(next) => {
        if (!next) {
          onSettle(null)
        }
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>
            {t('workflows.version.restoreTitle', {
              version: target?.version ?? '',
            })}
          </AlertDialogTitle>
          <AlertDialogDescription>
            {t('workflows.version.restoreDescription')}
          </AlertDialogDescription>
        </AlertDialogHeader>
        {errorCode && <p className="text-xs text-destructive">{t(workflowErrorKey(errorCode))}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel>{t('workflows.list.cancel')}</AlertDialogCancel>
          {/* A plain button, not the dialog's Close action: a rejected restore
              must leave the confirmation open so the fault line is readable. */}
          <Button
            disabled={isPending}
            onClick={() => {
              if (target !== null) {
                onConfirm(target)
              }
            }}
          >
            <RotateCcw className="size-3.5" />
            {t('workflows.version.restore')}
          </Button>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
