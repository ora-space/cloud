import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { useParams } from 'react-router-dom'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflow, useWorkflowRuns, runHistoryRows } from '@/features/workflows/api'
import { RunHistoryRow } from '@/features/workflows/run/run-history-row'
import { WorkflowDetailShell } from '@/features/workflows/workflow-detail-shell'
import { faultCode, isForbiddenError } from '@/lib/api-client'
import { workspacePaths } from '@/lib/paths'

/**
 * The 「运行记录」 tab: every run this workflow has had, newest first.
 *
 * Reading history never starts anything — the only writes on this page are the retry and the
 * "load more" request, both reads underneath. Each state the member can land in is stated as
 * itself: a pending read, a workflow that has never run, a failed read that can be retried, and a
 * missing permission. Reporting the last two as "no runs" would tell a member who lost access, or
 * whose request failed, that their history is empty.
 *
 * @param props.slug - Tenant id the workflow belongs to.
 */
export function WorkflowRunsPage({ slug }: { slug: string }) {
  const { t } = useTranslation()
  const { workspaceSlug, workflowId } = useParams<{ workspaceSlug: string; workflowId: string }>()
  const paths = workspacePaths(workspaceSlug ?? slug)
  const id = workflowId ?? ''
  const workflow = useWorkflow(slug, workflowId)
  const history = useWorkflowRuns(slug, workflowId)
  const rows = useMemo(() => runHistoryRows(history.data?.pages), [history.data])

  return (
    <WorkflowDetailShell title={workflow.data?.name ?? t('workflows.list.title')} workflowId={id}>
      {workflow.isPending ? (
        <div className="flex-1 p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto p-4">
          {workflow.data === undefined ? (
            <WorkflowReadFault
              error={workflow.error}
              onRetry={() => void workflow.refetch().catch(() => undefined)}
            />
          ) : (
            <RunHistoryBody
              history={history}
              rows={rows}
              hrefFor={(runId) => paths.workflowRun(id, runId)}
            />
          )}
        </div>
      )}
    </WorkflowDetailShell>
  )
}

/** Missing data alone does not mean missing workflow: only the public not_found fault does. */
function WorkflowReadFault({ error, onRetry }: { error: unknown; onRetry: () => void }) {
  const { t } = useTranslation()
  if (faultCode(error) === 'not_found') {
    return <p className="text-sm text-muted-foreground">{t('workflows.detail.notFound')}</p>
  }
  return <HistoryFault forbidden={isForbiddenError(error)} onRetry={onRetry} />
}

/** What the history query can be, resolved into one of the states a member can act on. */
type HistoryQuery = ReturnType<typeof useWorkflowRuns>

/**
 * The history body: loading, empty, unreadable, forbidden, or the rows with the way to go deeper.
 *
 * A failed read is only a whole-page failure when nothing was loaded: once rows are on screen they
 * stay, and the failure is reported by the footer instead. Replacing a readable history with an
 * error page would lose the runs the member was already looking at, which is the opposite of what a
 * history is for.
 */
function RunHistoryBody({
  history,
  rows,
  hrefFor,
}: {
  history: HistoryQuery
  rows: ReturnType<typeof runHistoryRows>
  hrefFor: (runId: string) => string
}) {
  const { t } = useTranslation()
  const loaded = rows.length > 0

  if (history.isPending) {
    return <p className="text-sm text-muted-foreground">{t('workflows.runs.loading')}</p>
  }
  if (history.isError && !loaded) {
    return (
      <HistoryFault
        forbidden={isForbiddenError(history.error)}
        onRetry={() => void history.refetch().catch(() => undefined)}
      />
    )
  }
  if (!loaded) {
    return (
      <div className="rounded-lg border border-dashed p-8 text-center">
        <p className="text-sm font-medium">{t('workflows.runs.emptyTitle')}</p>
        <p className="mt-1 text-xs text-muted-foreground">{t('workflows.runs.emptyDescription')}</p>
      </div>
    )
  }

  return (
    <div className="space-y-2">
      <ul className="space-y-2">
        {rows.map((run) => (
          <RunHistoryRow key={run.id} run={run} href={hrefFor(run.id)} />
        ))}
      </ul>
      <HistoryFooter history={history} />
    </div>
  )
}

/**
 * The paging footer: a failed refresh, the next page, its failure, or the end of the walk.
 *
 * The failure reads here rather than on a fault page, because the rows already loaded stay: a
 * history is not less readable because its next page failed. A failed read is reported whether or
 * not a next page exists — once the walk reached its end, a failure is a failed *refresh* of rows
 * that are already on screen, and it still needs a retry, so it must not be reported as the end of
 * the history.
 *
 * A 403 keeps its own meaning in both cases: the caller cannot read the rest of the history, and a
 * retry cannot change that, so it reads as the permission state rather than as a loading failure.
 */
function HistoryFooter({ history }: { history: HistoryQuery }) {
  const { t } = useTranslation()
  const failed = history.isError
  const forbidden = failed && isForbiddenError(history.error)

  if (failed && forbidden) {
    return (
      <div className="pt-2">
        <HistoryFault forbidden />
      </div>
    )
  }
  if (failed && !history.isFetchNextPageError) {
    return (
      <div className="pt-2">
        <HistoryFault
          forbidden={false}
          onRetry={() => void history.refetch().catch(() => undefined)}
        />
      </div>
    )
  }
  if (!history.hasNextPage) {
    return <p className="pt-2 text-xs text-muted-foreground">{t('workflows.runs.endOfHistory')}</p>
  }
  return (
    <div className="flex flex-wrap items-center gap-2 pt-2">
      <Button
        variant="outline"
        size="sm"
        disabled={history.isFetchingNextPage}
        onClick={() => void history.fetchNextPage().catch(() => undefined)}
      >
        {history.isFetchingNextPage
          ? t('workflows.runs.loadingMore')
          : t('workflows.runs.loadMore')}
      </Button>
      {history.isFetchNextPageError && (
        <p className="text-xs text-destructive">{t('workflows.runs.loadMoreFailed')}</p>
      )}
    </div>
  )
}

/**
 * A failed history read: a permission problem reads differently from a retryable failure.
 *
 * The retry is only offered for a failure a retry can fix — a refused read stays refused until the
 * member's role changes, and a button that cannot work is worse than no button.
 *
 * @param props.forbidden - Whether the read was refused as unpermitted.
 * @param props.onRetry - Repeats the failed read; required when `forbidden` is false.
 */
function HistoryFault({
  forbidden,
  onRetry,
}: {
  forbidden: boolean
  onRetry?: (() => void) | undefined
}) {
  const { t } = useTranslation()
  if (forbidden) {
    return (
      <div className="rounded-lg border border-dashed p-8 text-center">
        <p className="text-sm font-medium">{t('workflows.runs.forbiddenTitle')}</p>
        <p className="mt-1 text-xs text-muted-foreground">
          {t('workflows.runs.forbiddenDescription')}
        </p>
      </div>
    )
  }
  return (
    <div className="rounded-lg border border-dashed p-8 text-center">
      <p className="text-sm font-medium">{t('workflows.runs.failedTitle')}</p>
      <p className="mt-1 text-xs text-muted-foreground">{t('workflows.runs.failedDescription')}</p>
      {onRetry !== undefined && (
        <Button variant="outline" size="sm" className="mt-3" onClick={onRetry}>
          {t('workflows.runs.retry')}
        </Button>
      )}
    </div>
  )
}
