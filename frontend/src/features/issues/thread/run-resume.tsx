import type { IssueRun } from '@/features/issues/types'

/**
 * The line saying which earlier Revision an agent run continues from, or null
 * for a run that started from a fresh clone. The resumed Revision is looked up
 * among the issue's runs to show its short final commit; when that run is not
 * in the list the line still says the run resumed, without a commit.
 */
export function resumeSummary(run: IssueRun, runs: readonly IssueRun[]): string | null {
  const resumed = run.resumeRevisionId
  if (!resumed) return null
  const source = runs.find((candidate) => candidate.revision?.id === resumed)?.revision
  return source
    ? `续接自 Revision ${source.finalCommit.slice(0, 7)}`
    : '续接自上一次保存的 Revision'
}

/** The resume line above an agent run's Thread; renders nothing for a fresh run. */
export function RunResume({ run, runs }: { run: IssueRun; runs: readonly IssueRun[] }) {
  const summary = resumeSummary(run, runs)
  if (!summary) return null
  return (
    <p role="note" aria-label="续接" className="text-xs text-muted-foreground">
      {summary}
    </p>
  )
}
