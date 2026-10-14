import { format } from 'date-fns'
import { Play, ScrollText } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { useNavigate, useParams } from 'react-router-dom'
import type { WorkflowRunStatus } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  useCreateWorkflowRun,
  useWorkflowRuns,
  useWorkflowSnapshots,
} from '@/features/workflows/api'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import { workspacePaths } from '@/lib/paths'

/**
 * The toolbar's run controls: kick off one run now, or browse the run history.
 *
 * A run executes against a published snapshot, so the primary button is disabled while
 * the workflow has none — there is nothing honest to execute a draft against. Kicking off
 * a run navigates to it; browsing history opens the popover, whose rows lead to each run's
 * read-only detail page.
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
  const runs = useWorkflowRuns(tenantId, workflowId)
  const createRun = useCreateWorkflowRun(tenantId)
  const [historyOpen, setHistoryOpen] = useState(false)

  const noSnapshot = snapshots.data !== undefined && snapshots.data.length === 0
  const runNowDisabled = noSnapshot || createRun.isPending

  return (
    <>
      <Button
        variant="default"
        size="sm"
        disabled={runNowDisabled}
        title={noSnapshot ? t('workflows.run.noSnapshotHint') : t('workflows.run.runHint')}
        onClick={() => {
          createRun.mutate(
            { workflowId },
            {
              onSuccess: (run) => {
                setHistoryOpen(false)
                void navigate(paths.workflowRun(run.workflowId, run.id))
              },
            },
          )
        }}
      >
        <Play className="size-3.5" fill="currentColor" />
        {t('workflows.run.runNow')}
      </Button>
      <Popover
        open={historyOpen}
        onOpenChange={(next) => {
          setHistoryOpen(next)
          if (next) {
            void runs.refetch()
          }
        }}
      >
        <PopoverTrigger
          render={
            <Button
              variant="outline"
              size="icon"
              aria-label={t('workflows.run.history')}
              title={t('workflows.run.historyHint')}
            />
          }
        >
          <ScrollText className="size-4" />
        </PopoverTrigger>
        <PopoverContent align="end" className="w-80 p-0">
          <div className="border-b border-border px-3 py-2.5">
            <h3 className="text-sm font-semibold">{t('workflows.run.history')}</h3>
          </div>
          <RunHistoryList runs={runs.data ?? []} isPending={runs.isPending} onOpen={openRun} />
        </PopoverContent>
      </Popover>
    </>
  )

  /** Navigates to a run's read-only detail page. */
  function openRun(runId: string): void {
    setHistoryOpen(false)
    void navigate(paths.workflowRun(workflowId, runId))
  }
}

/** The run history rows: loading / empty / the runs newest first. */
function RunHistoryList({
  runs,
  isPending,
  onOpen,
}: {
  runs: Array<{ id: string; name: string; status: WorkflowRunStatus; createdAt: string }>
  isPending: boolean
  onOpen: (runId: string) => void
}) {
  const { t } = useTranslation()
  if (runs.length === 0) {
    return (
      <ScrollArea className="max-h-72 min-w-fit p-2">
        <p className="px-2.5 py-3 text-xs text-muted-foreground">
          {isPending ? t('workflows.detail.loading') : t('workflows.run.historyEmpty')}
        </p>
      </ScrollArea>
    )
  }
  return (
    <ScrollArea className="max-h-72 min-w-fit p-2">
      <ul className="space-y-1">
        {runs.map((run) => (
          <li key={run.id}>
            <button
              type="button"
              aria-label={t('workflows.run.openRun', { name: run.name })}
              onClick={() => onOpen(run.id)}
              className="w-full rounded-md px-2.5 py-2 text-left transition-colors hover:bg-muted"
            >
              <span className="flex min-w-0 items-center gap-2">
                <RunStatusBadge status={run.status} className="shrink-0 px-1.5 py-0 text-[9px]" />
                <span className="min-w-0 flex-1 truncate text-xs font-medium">{run.name}</span>
              </span>
              <span className="mt-0.5 block text-[11px] text-muted-foreground">
                {t('workflows.run.startedAt', {
                  time: format(new Date(run.createdAt), 'M月d日 HH:mm'),
                })}
              </span>
            </button>
          </li>
        ))}
      </ul>
    </ScrollArea>
  )
}
