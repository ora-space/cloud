import { createContext, memo, useContext, useMemo, type ReactNode } from 'react'
import { Handle, Position, type Node, type NodeProps } from '@xyflow/react'
import { cn } from '@/lib/utils'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { IterationNodeFrame } from '@/features/workflows/editor/iteration-node-frame'
import { LoopNodeFrame } from '@/features/workflows/editor/loop-node-frame'
import {
  WORKFLOW_NODE_ANCHOR_Y,
  WORKFLOW_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { isNodeWorking, runStatusTone } from '@/features/workflows/runtime/run-status-style'
import type { RunOverviewNodeData } from '@/features/workflows/runtime/run-overview-layout'
import { NodeIdentity } from '@/features/workflows/editor/node-identity'
import { RunStatusBadge } from '@/features/workflows/run/run-status-mark'
import type { RunNodeEntry } from '@/features/workflows/run/run-node-state'
import type { RunContainer } from '@/features/workflows/run/run-rounds'
import { RunContainerSummary } from '@/features/workflows/run/run-round-views'

/**
 * The per-node statuses the overview renders against, keyed by node id, with members already
 * showing their container's selected round, plus the round projection the frames summarize.
 */
interface RunOverviewStates {
  states: Record<string, RunNodeEntry>
  containers?: Record<string, RunContainer> | undefined
}

const RunOverviewStatesContext = createContext<RunOverviewStates | null>(null)

/** Provides the run's node rows to overview node renderers. */
export function RunOverviewStatesProvider({
  states,
  containers,
  children,
}: RunOverviewStates & { children: ReactNode }) {
  const value = useMemo(() => ({ states, containers }), [states, containers])
  return (
    <RunOverviewStatesContext.Provider value={value}>{children}</RunOverviewStatesContext.Provider>
  )
}

/**
 * The read-only card the run Overview paints for one snapshot node.
 *
 * The card shows the same identity chrome as the editor but swaps every editor affordance
 * for the node's run status: a status badge, the status-tinted frame ring, and the
 * started→finished timing strip when the trace recorded one. Nothing here can write.
 */
export const RunOverviewNode = memo(function RunOverviewNode({
  id,
  data,
  selected,
  deletable,
}: NodeProps<Node<RunOverviewNodeData, 'workflow'>>) {
  const translate = useWorkflowTranslator()
  const context = useContext(RunOverviewStatesContext)
  const entry = context?.states[id]
  const status = entry?.status ?? 'idle'
  const tone = runStatusTone(status)
  const working = isNodeWorking(status)
  const metadata = workflowNodeMetadata(data.kind)
  const Icon = metadata.icon
  const kindLabel = translate(workflowNodeTypeDefinition(data.kind).labelKey)
  const canDelete = deletable ?? true
  const container = context?.containers?.[id]
  const badge = container === undefined ? undefined : <RunContainerSummary container={container} />

  if (data.kind === 'iteration') {
    return (
      <div data-workflow-run-node="" className={cn('rounded-2xl', tone.ring, 'ring-1')}>
        <IterationNodeFrame
          id={id}
          data={data}
          selected={selected}
          deletable={canDelete}
          badge={badge}
        />
      </div>
    )
  }
  if (data.kind === 'loop') {
    return (
      <div data-workflow-run-node="" className={cn('rounded-2xl', tone.ring, 'ring-1')}>
        <LoopNodeFrame id={id} data={data} selected={selected} deletable={false} badge={badge} />
      </div>
    )
  }

  return (
    <article
      data-workflow-run-node=""
      data-workflow-node-id={id}
      aria-label={`${data.title}: ${translate(tone.labelKey)}`}
      style={{ width: WORKFLOW_NODE_WIDTH * 0.92 }}
      className={cn(
        'rounded-xl border bg-card shadow-sm outline-none transition-[border-color,box-shadow] duration-200',
        selected ? 'border-foreground/45 shadow-md ring-2 ring-ring/25' : 'border-border',
        'ring-1',
        tone.ring,
        working && 'ring-sky-500/35',
      )}
    >
      <Handle
        type="target"
        position={Position.Left}
        className="workflow-port !size-2.5 !border-0 !bg-transparent"
        style={{ top: WORKFLOW_NODE_ANCHOR_Y * 0.92 }}
        isConnectable={false}
      />
      <NodeIdentity
        id={id}
        title={data.title}
        description={data.description}
        kindLabel={kindLabel}
        tone={metadata.tone}
        icon={Icon}
        action={
          <RunStatusBadge status={status} live={working} className="px-1.5 py-0 text-[9px]" />
        }
      />
      <RunTiming entry={entry} />
      <Handle
        type="source"
        position={Position.Right}
        className="workflow-port !size-2.5 !border-0 !bg-transparent"
        style={{ top: WORKFLOW_NODE_ANCHOR_Y * 0.92 }}
        isConnectable={false}
      />
    </article>
  )
})

/** The started→finished strip, omitted entirely when the trace recorded no timing. */
function RunTiming({ entry }: { entry: RunNodeEntry | undefined }) {
  const startedAt = entry?.startedAt
  const finishedAt = entry?.finishedAt
  if (startedAt === undefined && finishedAt === undefined) {
    return null
  }
  return (
    <p className="border-t border-border/60 px-3 pt-1.5 pb-2 font-mono text-[9px] tabular-nums text-muted-foreground">
      {formatClock(startedAt)}
      {' — '}
      {formatClock(finishedAt)}
    </p>
  )
}

/** Formats an ISO timestamp as local `HH:MM:SS`, or `—` when a bound is missing. */
function formatClock(iso: string | undefined): string {
  if (iso === undefined) {
    return '—'
  }
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '—'
  }
  return date.toLocaleTimeString([], { hour12: false })
}
