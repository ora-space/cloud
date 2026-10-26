import { useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { useParams } from 'react-router-dom'
import type { WorkflowRun } from '@/api/generated.schemas'
import { PageHeader } from '@/components/layout/page-header'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflowRun } from '@/features/workflows/api'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import { parseRunNodeStates, type RunNodeEntry } from '@/features/workflows/run/run-node-state'
import { RunInspector } from '@/features/workflows/run/run-inspector'
import { RunOverviewCanvas } from '@/features/workflows/run/run-overview-canvas'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import { workspacePaths } from '@/lib/paths'

/** The route table returned by {@link workspacePaths}, as passed down to the shell. */
type WorkflowPaths = ReturnType<typeof workspacePaths>

/**
 * One workflow run's read-only destination.
 *
 * The header carries the run's identity and status; the body pairs the Overview canvas
 * (the frozen snapshot graph painted with per-node run status) with a read-only inspector
 * for the node the reader selects. Nothing here writes: the run is final.
 *
 * @param props.slug - Tenant id the run belongs to.
 */
export function WorkflowRunDetailPage({ slug }: { slug: string }) {
  const { t } = useTranslation()
  const { workspaceSlug, workflowId, runId } = useParams<{
    workspaceSlug: string
    workflowId: string
    runId: string
  }>()
  const paths = workspacePaths(workspaceSlug ?? slug)
  const run = useWorkflowRun(slug, workflowId, runId)
  const nodeStates = useMemo(() => parseRunNodeStates(run.data?.nodeStates), [run.data?.nodeStates])

  if (run.isPending) {
    return (
      <RunPageShell title={t('workflows.run.detailTitle')}>
        <div className="flex-1 p-4">
          <Skeleton className="h-24 w-full" />
        </div>
      </RunPageShell>
    )
  }

  if (!run.data) {
    const title = t('workflows.list.title')
    return (
      <RunPageShell title={title} breadcrumb={{ label: title, to: paths.workflows }}>
        <p className="p-4 text-sm text-muted-foreground">{t('workflows.run.notFound')}</p>
      </RunPageShell>
    )
  }

  return (
    <WorkflowRunDetailBody key={run.data.id} run={run.data} nodeStates={nodeStates} paths={paths} />
  )
}

/**
 * The loaded run's header and Overview body.
 *
 * Keyed by run id from the parent, so a fresh run mounts with an empty selection
 * instead of resetting state through an effect. The Overview is read-only; only the
 * inspector selection changes here.
 */
function WorkflowRunDetailBody({
  run,
  nodeStates,
  paths,
}: {
  run: WorkflowRun
  nodeStates: Record<string, RunNodeEntry>
  paths: WorkflowPaths
}) {
  const { t } = useTranslation()
  const [selectedNodeId, setSelectedNodeId] = useState<string | null>(null)
  const selectedNode = findFrozenNode(run.definitionSnapshot, selectedNodeId)

  return (
    <RunPageShell
      title={run.name}
      breadcrumb={{
        label: run.workflowName || run.name,
        to: paths.workflowDetail(run.workflowId),
      }}
    >
      <div className="flex min-h-0 flex-1 flex-col">
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 border-b border-border px-4 py-2">
          <RunStatusBadge status={run.status} />
          <RunTimestamp label={t('workflows.run.created')} iso={run.createdAt} />
          {run.startedAt !== null && run.startedAt !== '' && (
            <RunTimestamp label={t('workflows.run.started')} iso={run.startedAt} />
          )}
          {run.finishedAt !== null && run.finishedAt !== '' && (
            <RunTimestamp label={t('workflows.run.finished')} iso={run.finishedAt} />
          )}
          <span className="rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
            {shortId(run.snapshotId)}
          </span>
        </div>
        <div className="flex min-h-0 flex-1">
          {/* The canvas sizes itself with `h-full` inside a `flex-1` root, so its column must be a
              flex container: a block column lets `flex-1` go inert and collapses the canvas to 0. */}
          <div className="flex min-h-0 min-w-0 flex-1 flex-col">
            <RunOverviewCanvas
              definitionSnapshot={run.definitionSnapshot}
              nodeStates={nodeStates}
              selectedNodeId={selectedNodeId}
              onSelectNode={setSelectedNodeId}
            />
          </div>
          <aside
            className="w-80 shrink-0 border-l border-border bg-background"
            aria-label={t('workflows.run.inspector.label')}
          >
            {selectedNode === undefined ? (
              <p className="p-6 text-center text-xs text-muted-foreground">
                {t('workflows.run.inspector.empty')}
              </p>
            ) : (
              <RunInspector data={selectedNode.data} entry={nodeStates[selectedNodeId ?? '']} />
            )}
          </aside>
        </div>
      </div>
    </RunPageShell>
  )
}

/** A small timestamp with a label, used in the run header strip. */
function RunTimestamp({ label, iso }: { label: string; iso: string }) {
  return (
    <span className="text-xs text-muted-foreground">
      {label}{' '}
      <time dateTime={iso} className="font-mono text-[11px] tabular-nums">
        {formatDateTime(iso)}
      </time>
    </span>
  )
}

/** The subtitle line under every run page state, prefixed by the workspace header. */
function RunPageShell({
  title,
  breadcrumb,
  children,
}: {
  title: string
  breadcrumb?: { label: string; to: string } | undefined
  children: ReactNode
}) {
  return (
    <div className="flex h-full flex-col">
      <PageHeader title={title} {...(breadcrumb === undefined ? {} : { breadcrumb })} />
      {children}
    </div>
  )
}

/** Finds one frozen node by id, tolerating an unreadable or absent graph. */
function findFrozenNode(
  definitionSnapshot: Record<string, unknown> | null | undefined,
  nodeId: string | null,
): { data: WorkflowNodeData } | undefined {
  if (nodeId === null) {
    return undefined
  }
  const envelope = parseWorkflowGraphValue(definitionSnapshot ?? undefined)
  const node = envelope.nodes.find((candidate) => candidate.id === nodeId)
  return node === undefined ? undefined : { data: node.data }
}

/** Local `YYYY-MM-DD HH:MM` rendering for the header strip. */
function formatDateTime(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '—'
  }
  return date.toLocaleString([], {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

/** Shortens a uuid to its 8-character prefix for compact display. */
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id
}
