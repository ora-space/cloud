import { format } from 'date-fns'
import { MoreHorizontal, Pencil, ScrollText, Trash2, Workflow } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Link } from 'react-router-dom'
import type { Workflow as CloudWorkflow } from '@/api/generated.schemas'
import { Button, buttonVariants } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import { cn } from '@/lib/utils'

/**
 * One workflow in the list: its name, its newest run, and the actions a member can take on it.
 *
 * The row used to lead only into the editor, which is why "what did this workflow do last" was
 * invisible from the list. The last-run label answers that here, and the run-history control opens
 * the workflow's run list directly, so browsing runs does not require opening the editor first.
 * Both are read-only paths: neither starts a run.
 *
 * Each of the two actions is a link of its own rather than a click handler on the row, so a member
 * can open the editor, the runs, or a specific run in a new tab — and the row's own link keeps
 * pointing at the editor, the destination it always had.
 *
 * @param props.workflow - The workflow to render, including its newest run when it has one.
 * @param props.href - Destination of the row's main link, the workflow's editor.
 * @param props.runsHref - Destination of the run-history control.
 * @param props.runHref - Builds a run's detail destination from its id.
 * @param props.onRename - Opens the rename dialog for this workflow.
 * @param props.onDelete - Opens the delete confirmation for this workflow.
 */
export function WorkflowRow({
  workflow,
  href,
  runsHref,
  runHref,
  onRename,
  onDelete,
}: {
  workflow: CloudWorkflow
  href: string
  runsHref: string
  runHref: (runId: string) => string
  onRename: (workflow: CloudWorkflow) => void
  onDelete: (workflow: CloudWorkflow) => void
}) {
  const { t } = useTranslation()
  const nodeCount = useMemo(
    () => parseWorkflowGraphValue(workflow.graph).nodes.length,
    [workflow.graph],
  )
  const latestRun = workflow.latestRun ?? undefined

  return (
    <div className="flex items-center gap-3 rounded-lg border p-3 hover:bg-muted/50">
      <span className="flex size-8 shrink-0 items-center justify-center rounded-md bg-muted">
        <Workflow className="size-4 text-muted-foreground" />
      </span>
      <Link
        to={href}
        className="min-w-0 flex-1"
        aria-label={t('workflows.list.open', { name: workflow.name })}
      >
        <p className="truncate text-sm font-medium">{workflow.name}</p>
        <p className="truncate text-xs text-muted-foreground">
          {workflow.description || t('workflows.list.nodeCount', { count: nodeCount })}
        </p>
      </Link>
      {latestRun === undefined ? (
        <span className="hidden shrink-0 text-xs text-muted-foreground sm:block">
          {t('workflows.list.neverRan')}
        </span>
      ) : (
        <Link
          to={runHref(latestRun.id)}
          className="hidden shrink-0 items-center gap-2 sm:flex"
          aria-label={t('workflows.list.openLatestRun', { name: workflow.name })}
        >
          <RunStatusBadge status={latestRun.status} className="shrink-0" />
          <span className="text-xs text-muted-foreground">
            {t('workflows.list.latestRunAt', {
              time: format(new Date(latestRun.startedAt ?? latestRun.createdAt), 'M月d日 HH:mm'),
            })}
          </span>
        </Link>
      )}
      <Link
        to={runsHref}
        title={t('workflows.list.runsHint')}
        className={cn(buttonVariants({ variant: 'ghost', size: 'sm' }), 'shrink-0')}
      >
        <ScrollText className="size-3.5" />
        {t('workflows.run.history')}
      </Link>
      <span className="hidden shrink-0 text-xs text-muted-foreground md:block">
        {t('workflows.list.updatedAt', { time: format(new Date(workflow.updatedAt), 'M月d日') })}
      </span>
      <DropdownMenu>
        <DropdownMenuTrigger
          render={
            <Button
              variant="ghost"
              size="icon"
              className="size-7 shrink-0"
              aria-label={t('workflows.list.open', { name: workflow.name })}
            />
          }
        >
          <MoreHorizontal className="size-4" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end">
          <DropdownMenuItem onClick={() => onRename(workflow)}>
            <Pencil className="size-3.5" />
            {t('workflows.list.rename')}
          </DropdownMenuItem>
          <DropdownMenuItem variant="destructive" onClick={() => onDelete(workflow)}>
            <Trash2 className="size-3.5" />
            {t('workflows.list.deleteNamed', { name: workflow.name })}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </div>
  )
}
