import { useMemo, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { useParams } from 'react-router-dom'
import type { WorkflowRun } from '@/api/generated.schemas'
import { PageHeader } from '@/components/layout/page-header'
import { Skeleton } from '@/components/ui/skeleton'
import { useWorkflowRun } from '@/features/workflows/api'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import { formatRunInstant, shortRunId } from '@/features/workflows/run/run-time'
import { parseRunNodeStates, type RunNodeEntry } from '@/features/workflows/run/run-node-state'
import {
  parseRunContainers,
  projectRoundStates,
  type RunContainer,
  type RunRoundSelection,
} from '@/features/workflows/run/run-rounds'
import { RunContainerRounds, RunMemberRoundSwitch } from '@/features/workflows/run/run-round-views'
import { effectiveWorkflowContainer } from '@/features/workflows/runtime/container-layout'
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
 * inspector selection and the round each container shows change here. A container the
 * reader has not picked a round for follows its latest round, so a running Loop keeps
 * showing the round under way.
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
  const [roundSelection, setRoundSelection] = useState<RunRoundSelection>({})
  const frozenNodes = useMemo(
    () => parseWorkflowGraphValue(run.definitionSnapshot ?? undefined).nodes,
    [run.definitionSnapshot],
  )
  const containers = useMemo(
    () => parseRunContainers(run.nodeStates, frozenNodes),
    [run.nodeStates, frozenNodes],
  )
  const shownStates = useMemo(
    () => projectRoundStates(nodeStates, containers, roundSelection),
    [nodeStates, containers, roundSelection],
  )
  const selectedNode = frozenNodes.find((node) => node.id === selectedNodeId)
  const selectRound = (containerId: string, index: number): void => {
    setRoundSelection((current) => ({ ...current, [containerId]: index }))
  }

  return (
    <RunPageShell
      title={run.name}
      // The way back is the run list this run was opened from, not the editor: a member browsing
      // history returns to the history, where the next run to look at is one click away.
      breadcrumb={{
        label: `${run.workflowName || run.name} · ${t('workflows.run.history')}`,
        to: paths.workflowRuns(run.workflowId),
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
            {shortRunId(run.snapshotId)}
          </span>
        </div>
        <div className="flex min-h-0 flex-1">
          {/* The canvas sizes itself with `h-full` inside a `flex-1` root, so its column must be a
              flex container: a block column lets `flex-1` go inert and collapses the canvas to 0. */}
          <div className="flex min-h-0 min-w-0 flex-1 flex-col">
            <RunOverviewCanvas
              definitionSnapshot={run.definitionSnapshot}
              nodeStates={shownStates}
              containers={containers}
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
              <RunInspector
                data={selectedNode.data}
                entry={shownStates[selectedNode.id]}
                rounds={
                  <InspectorRounds
                    nodeId={selectedNode.id}
                    owner={effectiveWorkflowContainer(selectedNode)}
                    containers={containers}
                    selection={roundSelection}
                    memberTitles={memberTitles(frozenNodes)}
                    onSelectRound={selectRound}
                  />
                }
              />
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
        {formatRunInstant(iso)}
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

/**
 * The inspector's round section: the round list of a selected container, or the round switch
 * of a selected container member; nothing for a node outside both.
 */
function InspectorRounds({
  nodeId,
  owner,
  containers,
  selection,
  memberTitles: titles,
  onSelectRound,
}: {
  nodeId: string
  owner: string | undefined
  containers: Record<string, RunContainer>
  selection: RunRoundSelection
  memberTitles: ReadonlyMap<string, string>
  onSelectRound: (containerId: string, index: number) => void
}) {
  const own = containers[nodeId]
  if (own !== undefined) {
    return (
      <RunContainerRounds
        container={own}
        selection={selection}
        memberTitles={titles}
        onSelectRound={onSelectRound}
      />
    )
  }
  const parent = owner === undefined ? undefined : containers[owner]
  return parent === undefined ? null : (
    <RunMemberRoundSwitch container={parent} selection={selection} onSelectRound={onSelectRound} />
  )
}

/** Frozen node titles by id, for the round member list. */
function memberTitles(
  nodes: readonly { id: string; data: WorkflowNodeData }[],
): Map<string, string> {
  return new Map(nodes.map((node) => [node.id, node.data.title]))
}
