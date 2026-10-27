import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'
import type { WorkflowRun } from '@/api/generated.schemas'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import { formatRunInstant, shortRunId } from '@/features/workflows/run/run-time'

/**
 * One run in the history: when it started and finished, how it ended, which version it executed,
 * and the way in.
 *
 * The row shows the run's whole span, not just the instant it became visible: "when did this take
 * how long" is most of why a member reads a history, and a finished run's start alone cannot answer
 * it. A run that has not started yet is stated as such, with its creation time, rather than looking
 * like one that started at that instant.
 *
 * The whole row is a link, so a member does not have to aim at a small control to open a run, and
 * the snapshot is shown as the frozen version label the rest of the surface uses — the run detail
 * page is that same frozen document, never the workflow's current draft.
 *
 * @param props.run - The run to render.
 * @param props.href - Destination of the row's link, the run's read-only detail page.
 */
export function RunHistoryRow({ run, href }: { run: WorkflowRun; href: string }) {
  const { t } = useTranslation()
  // Both columns are nullable: a run that has not been picked up has no start, and one still going
  // has no finish. They are read once here so the two labels cannot disagree about the same field.
  const startedAt = run.startedAt ?? ''
  const finishedAt = run.finishedAt ?? ''
  return (
    <li>
      <Link
        to={href}
        aria-label={t('workflows.runs.open', { name: run.name })}
        className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border px-3 py-2 hover:bg-muted/50"
      >
        <RunStatusBadge status={run.status} className="shrink-0" />
        <span className="min-w-0 flex-1 truncate text-sm font-medium">{run.name}</span>
        <span className="shrink-0 text-xs text-muted-foreground">
          {startedAt === ''
            ? t('workflows.runs.notStarted', { time: formatRunInstant(run.createdAt) })
            : t('workflows.runs.started', { time: formatRunInstant(startedAt) })}
        </span>
        {(startedAt !== '' || finishedAt !== '') && (
          <span className="shrink-0 text-xs text-muted-foreground">
            {finishedAt === ''
              ? t('workflows.runs.notFinished')
              : t('workflows.runs.finished', { time: formatRunInstant(finishedAt) })}
          </span>
        )}
        <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
          {t('workflows.runs.version', { id: shortRunId(run.snapshotId) })}
        </span>
      </Link>
    </li>
  )
}
