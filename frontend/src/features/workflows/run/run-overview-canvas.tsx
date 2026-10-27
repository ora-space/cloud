import { useEffect, useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Background,
  BackgroundVariant,
  MarkerType,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  useViewport,
  type DefaultEdgeOptions,
  type Edge,
  type Node,
} from '@xyflow/react'
import { Maximize, Minus, Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { WorkflowIterationActionsProvider } from '@/features/workflows/editor/workflow-iteration-actions'
import { parseWorkflowGraphValue } from '@/features/workflows/runtime/graph-codec'
import {
  WORKFLOW_ITERATION_NODE_HEIGHT,
  WORKFLOW_ITERATION_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import {
  createRunOverviewNodes,
  type RunOverviewNodeData,
} from '@/features/workflows/runtime/run-overview-layout'
import {
  RunOverviewNode,
  RunOverviewStatesProvider,
} from '@/features/workflows/run/run-overview-node'
import type { RunNodeEntry } from '@/features/workflows/run/run-node-state'
import type { RunContainer } from '@/features/workflows/run/run-rounds'

const NODE_TYPE = 'workflow' as const
const FIT_PADDING = 0.18
const MIN_ZOOM = 0.2
const MAX_ZOOM = 2

const nodeTypes = { [NODE_TYPE]: RunOverviewNode }

const DEFAULT_EDGE_OPTIONS = {
  markerEnd: {
    type: MarkerType.ArrowClosed,
    width: 22,
    height: 22,
    markerUnits: 'userSpaceOnUse',
    color: 'color-mix(in oklch, var(--foreground) 40%, transparent)',
  },
} satisfies DefaultEdgeOptions

/** The steady edge colour: dim below the editor's so idle traces stay quiet. */
const IDLE_EDGE_STROKE = 'color-mix(in oklch, var(--foreground) 30%, transparent)'
/** The bright edge used once its source node has executed. */
const ACTIVE_EDGE_STROKE = 'color-mix(in oklch, var(--foreground) 72%, transparent)'

/**
 * The read-only Overview canvas of one run.
 *
 * Nodes come from the run's frozen snapshot, not the live workflow document, and the
 * run trace is overlaid as per-node status. The graph cannot be edited: no dragging,
 * connecting, or deleting. Clicking a node selects it for the sibling inspector; the
 * iteration frames render their members via the editor's frame chrome in read-only mode.
 *
 * @param props.definitionSnapshot - The frozen graph document (ops of the run). May be
 * missing for a legacy run row, which then renders an empty canvas.
 * @param props.nodeStates - Per-node run rows keyed by node id; container members already show
 * their container's selected round.
 * @param props.containers - The round projection the Loop and Iteration frames summarize.
 * @param props.selectedNodeId - Node the reader opened in the inspector, if any.
 * @param props.onSelectNode - Receives a node click, or `null` when the canvas clears.
 */
export function RunOverviewCanvas({
  definitionSnapshot,
  nodeStates,
  containers,
  selectedNodeId,
  onSelectNode,
}: {
  definitionSnapshot: Record<string, unknown> | null | undefined
  nodeStates: Record<string, RunNodeEntry>
  containers?: Record<string, RunContainer>
  selectedNodeId: string | null
  onSelectNode: (nodeId: string | null) => void
}) {
  const { t } = useTranslation()
  const graph = useMemo(
    () => buildRunGraph(definitionSnapshot, nodeStates, selectedNodeId),
    [definitionSnapshot, nodeStates, selectedNodeId],
  )

  return (
    <div
      className="relative min-h-0 flex-1 bg-muted/15"
      aria-label={t('workflows.run.overview.label')}
    >
      <ReactFlowProvider>
        <RunOverviewStatesProvider states={nodeStates} containers={containers}>
          <WorkflowIterationActionsProvider
            graph={graph}
            readOnly
            onInsert={() => {}}
            onToggleCollapsed={() => {}}
          >
            <ReactFlow
              className="h-full w-full"
              nodes={graph.nodes}
              edges={graph.edges}
              nodeTypes={nodeTypes}
              defaultEdgeOptions={DEFAULT_EDGE_OPTIONS}
              nodesDraggable={false}
              nodesConnectable={false}
              edgesReconnectable={false}
              elementsSelectable
              minZoom={MIN_ZOOM}
              maxZoom={MAX_ZOOM}
              panOnScroll={false}
              zoomOnScroll
              zoomOnPinch
              proOptions={{ hideAttribution: true }}
              onNodeClick={(_event, node) => onSelectNode(node.id)}
              onPaneClick={() => onSelectNode(null)}
            >
              <OverviewFitController />
              <RunOverviewZoomControls />
              <Background
                id="run-overview-dots"
                variant={BackgroundVariant.Dots}
                gap={22}
                size={1.1}
                color="color-mix(in oklch, var(--foreground) 12%, transparent)"
              />
            </ReactFlow>
          </WorkflowIterationActionsProvider>
        </RunOverviewStatesProvider>
      </ReactFlowProvider>
      <p className="pointer-events-none absolute bottom-3 left-3 rounded-md border border-border/70 bg-background/85 px-2 py-1 text-[10px] text-muted-foreground backdrop-blur-sm">
        {t('workflows.run.overview.hint')}
      </p>
    </div>
  )
}

/** Fits the frozen graph once the canvas has measured; re-fits only when the run changes. */
function OverviewFitController() {
  const { fitView } = useReactFlow()
  useEffect(() => {
    const frame = requestAnimationFrame(() => {
      void fitView({ padding: FIT_PADDING, duration: 200, maxZoom: 1 })
    })
    return () => cancelAnimationFrame(frame)
    // Fit against the run's shape only; a selection change must not re-centre.
  }, [fitView])
  return null
}

/** Explicit zoom out / zoom in / fit controls for readers that prefer buttons. */
function RunOverviewZoomControls() {
  const { t } = useTranslation()
  const { fitView, zoomTo } = useReactFlow()
  const { zoom } = useViewport()
  return (
    <div
      className="absolute right-3 top-3 z-10 flex items-center rounded-lg border border-border/80 bg-background/95 p-px shadow-sm backdrop-blur"
      role="toolbar"
      aria-label={t('workflows.run.overview.zoomControls')}
    >
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-7 rounded-md"
        aria-label={t('workflows.run.overview.zoomOut')}
        disabled={zoom <= MIN_ZOOM}
        onClick={() => {
          void zoomTo(Math.max(MIN_ZOOM, zoom - 0.1))
        }}
      >
        <Minus />
      </Button>
      <span className="flex h-7 w-9 items-center justify-center text-[9px] font-medium tabular-nums text-muted-foreground">
        {Math.round(zoom * 100)}%
      </span>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-7 rounded-md"
        aria-label={t('workflows.run.overview.zoomIn')}
        disabled={zoom >= MAX_ZOOM}
        onClick={() => {
          void zoomTo(Math.min(MAX_ZOOM, zoom + 0.1))
        }}
      >
        <Plus />
      </Button>
      <Button
        variant="ghost"
        size="icon-sm"
        className="size-7 rounded-md"
        aria-label={t('workflows.run.overview.fitView')}
        onClick={() => {
          void fitView({ padding: FIT_PADDING, duration: 180, maxZoom: 1 })
        }}
      >
        <Maximize />
      </Button>
    </div>
  )
}

/** Builds the overview nodes and edges from the frozen graph plus the run trace. */
function buildRunGraph(
  definitionSnapshot: Record<string, unknown> | null | undefined,
  nodeStates: Record<string, RunNodeEntry>,
  selectedNodeId: string | null,
): { nodes: Node<RunOverviewNodeData, 'workflow'>[]; edges: Edge[] } {
  const envelope = parseWorkflowGraphValue(definitionSnapshot ?? undefined)
  const memberCountByIteration = new Map<string, number>()
  for (const node of envelope.nodes) {
    const parentId = node.parentId
    if (parentId !== undefined) {
      memberCountByIteration.set(parentId, (memberCountByIteration.get(parentId) ?? 0) + 1)
    }
  }

  const nodes: Node<RunOverviewNodeData, 'workflow'>[] = []
  for (const base of createRunOverviewNodes(envelope.nodes, nodeStates)) {
    const isIteration = base.data.kind === 'iteration'
    const node: Node<RunOverviewNodeData, 'workflow'> = {
      ...base,
      type: NODE_TYPE,
      selected: base.id === selectedNodeId,
      data: {
        ...base.data,
        ...(isIteration
          ? { collapsed: false, regionMemberCount: memberCountByIteration.get(base.id) ?? 0 }
          : {}),
      },
    }
    if (isIteration) {
      node.style = {
        width: Math.max(
          WORKFLOW_ITERATION_NODE_WIDTH,
          finiteDimension(base.initialWidth, WORKFLOW_ITERATION_NODE_WIDTH),
        ),
        height: Math.max(
          WORKFLOW_ITERATION_NODE_HEIGHT,
          finiteDimension(base.initialHeight, WORKFLOW_ITERATION_NODE_HEIGHT),
        ),
      }
    }
    nodes.push(node)
  }

  const edges: Edge[] = []
  for (const edge of envelope.edges) {
    const activePath = (nodeStates[edge.source]?.status ?? 'idle') !== 'idle'
    edges.push({
      ...edge,
      type: 'default',
      selectable: false,
      focusable: false,
      reconnectable: false,
      style: { stroke: activePath ? ACTIVE_EDGE_STROKE : IDLE_EDGE_STROKE },
    })
  }
  return { nodes, edges }
}

/** Accepts persisted dimensions only when they are positive finite numbers. */
function finiteDimension(value: number | undefined, fallback: number): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 ? value : fallback
}
