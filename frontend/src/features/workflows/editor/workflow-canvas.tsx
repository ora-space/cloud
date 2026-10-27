import { useEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Background,
  BackgroundVariant,
  Controls,
  MarkerType,
  MiniMap,
  ReactFlow,
  ReactFlowProvider,
  useReactFlow,
  type DefaultEdgeOptions,
  type OnConnect,
  type OnEdgesChange,
  type OnNodesChange,
  type Viewport,
  type XYPosition,
} from '@xyflow/react'
import { LayoutDashboard } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  WORKFLOW_FLOW_EDGE_TYPE,
  WORKFLOW_FLOW_NODE_TYPE,
  WORKFLOW_SNAP_GRID,
  nodePositionAt,
  snapNodePosition,
} from '@/features/workflows/runtime/layout'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'
import {
  repairIterationGraphAfterNodeDeletion,
  resolveIterationDeletionCascade,
} from '@/features/workflows/runtime/iteration-graph'
import { applyIterationDragRules } from '@/features/workflows/runtime/iteration-containment'
import {
  buildIterationRegionGraph,
  workflowPointInsideIterationFrame,
} from '@/features/workflows/editor/workflow-iteration-region'
import { WorkflowIterationActionsProvider } from '@/features/workflows/editor/workflow-iteration-actions'
import { isValidWorkflowConnection } from '@/features/workflows/runtime/connection-validation'
import type { WorkflowContainerInsertion } from '@/features/workflows/runtime/loop-graph'
import { WorkflowNodeCard } from '@/features/workflows/editor/workflow-node-card'
import { WorkflowNodeCatalog } from '@/features/workflows/editor/workflow-node-catalog'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'
import '@xyflow/react/dist/style.css'
import '@/features/workflows/editor/workflow-canvas.css'

/** Zoom bounds the editor keeps the canvas within. */
const MIN_ZOOM = 0.2
const MAX_ZOOM = 2

/** Viewport a graph with no saved one opens at. */
const DEFAULT_VIEWPORT: Viewport = { x: 0, y: 0, zoom: 1 }

/** Human-readable notices the canvas raises while authoring iteration regions. */
type WorkflowIterationHintKey =
  'workflows.iteration.useInternalAdd' | 'workflows.iteration.collectTargetDeleted'

interface WorkflowIterationHint {
  key: WorkflowIterationHintKey
  params?: Record<string, string | number>
}

/** How long a transient hint pill stays visible before fading out. */
const ITERATION_HINT_MS = 2600

const NODE_TYPES = { [WORKFLOW_FLOW_NODE_TYPE]: WorkflowNodeCard }

const DEFAULT_EDGE_OPTIONS = {
  type: WORKFLOW_FLOW_EDGE_TYPE,
  markerEnd: {
    type: MarkerType.ArrowClosed,
    width: 22,
    height: 22,
    markerUnits: 'userSpaceOnUse',
    color: 'color-mix(in oklch, var(--foreground) 64%, transparent)',
  },
} satisfies DefaultEdgeOptions

/** The graph the author asked to delete, shaped for React Flow's before-delete hook. */
type WorkflowDeletionRequest = {
  nodes: WorkflowCanvasNode[]
  edges: WorkflowCanvasEdge[]
}

/** Props for {@link WorkflowCanvas}. */
export interface WorkflowCanvasProps {
  /** Graph nodes to draw. */
  nodes: WorkflowCanvasNode[]
  /** Graph edges to draw. */
  edges: WorkflowCanvasEdge[]
  /** Viewport the graph was last saved at. */
  viewport?: Viewport
  /** Applies node moves, selections and removals. */
  onNodesChange: OnNodesChange<WorkflowCanvasNode>
  /** Applies edge changes. */
  onEdgesChange: OnEdgesChange<WorkflowCanvasEdge>
  /** Applies a completed connection gesture. */
  onConnect: OnConnect
  /** Adds a node at a flow-space position. */
  onAddNode: (kind: WorkflowNodeKind, position: XYPosition) => void
  /** Re-arranges the graph in execution order. */
  onOrganize: () => void
  /** Reports the viewport once a pan or zoom settles, so it can be saved. */
  onViewportChange?: (viewport: Viewport) => void
  /** Iteration region authoring, wired by the editable editor only. */
  onIterationInsert?: (insertion: WorkflowContainerInsertion, kind: WorkflowNodeKind) => void
  /** Folds or unfolds one iteration frame. */
  onToggleIterationCollapsed?: (iterationId: string) => void
  /** Replaces the draft nodes after an iteration drag containment correction. */
  onIterationCorrection?: (nodes: WorkflowCanvasNode[]) => void
  /**
   * Resolves whether a region with members may be deleted. The editor renders the
   * confirmation; this resolves it. Returns false without a handler.
   */
  onConfirmIterationDeletion?: (memberCount: number) => Promise<boolean>
  /** Opens the node-drag history transaction at drag start. */
  onHistoryNodeDragStart?: () => void
  /** Closes the node-drag transaction once containment settles the positions. */
  onHistoryNodeDragStop?: () => void
  /** Draws the graph as a preview: nothing can be moved, connected or added. */
  readOnly?: boolean
}

/**
 * The graph surface of a workflow editor.
 *
 * Presentational: it owns no workflow state, so the editor decides what the
 * graph contains and when it is persisted. It does own the React Flow store,
 * which is what lets the palette turn screen coordinates into flow space. The
 * element must sit in a parent with a definite height, because React Flow
 * measures its container to size the viewport.
 *
 * Iteration regions are a canvas presentation too: the draft holds the plain
 * persisted graph, and this component derives the region chrome (frames, member
 * constraints, hidden folds, projected edges) on the way into React Flow.
 *
 * @param props - The graph, the change handlers and the read-only flag.
 */
export function WorkflowCanvas(props: WorkflowCanvasProps) {
  const graph = useMemo(
    () => ({ nodes: props.nodes, edges: props.edges }),
    [props.nodes, props.edges],
  )
  return (
    <ReactFlowProvider>
      <WorkflowIterationActionsProvider
        graph={graph}
        readOnly={props.readOnly ?? false}
        onInsert={(kind, insertion) => {
          props.onIterationInsert?.(insertion, kind)
        }}
        onToggleCollapsed={(iterationId) => {
          props.onToggleIterationCollapsed?.(iterationId)
        }}
      >
        <WorkflowCanvasInner {...props} />
      </WorkflowIterationActionsProvider>
    </ReactFlowProvider>
  )
}

/**
 * Canvas-gesture state only the editable canvas needs: the transient hint,
 * the drag snapshot, and the actions that enforce region membership. Hooks
 * must be ordered before any conditional render so the drag handlers can sit
 * at the top of the component without breaking the rules of hooks.
 */
/** Callbacks the iteration-guard gestures hand back to the editor. */
interface WorkflowIterationGuardServices {
  onIterationCorrection: ((nodes: WorkflowCanvasNode[]) => void) | undefined
  onConfirmIterationDeletion: ((memberCount: number) => Promise<boolean>) | undefined
  onHistoryNodeDragStart: (() => void) | undefined
  onHistoryNodeDragStop: (() => void) | undefined
}

function useIterationCanvasGuards(
  editor: { graphNodes: WorkflowCanvasNode[]; graphEdges: WorkflowCanvasEdge[] },
  readOnly: boolean,
  services: WorkflowIterationGuardServices,
) {
  const { graphNodes, graphEdges } = editor
  const { getNodes } = useReactFlow<WorkflowCanvasNode, WorkflowCanvasEdge>()
  const [hint, setHint] = useState<WorkflowIterationHint | null>(null)
  const hintTimer = useRef<number | null>(null)
  // Snapshot of `{ nodes, edges }` exactly as React Flow saw them when the drag
  // began; the reference comparison in the stop handler depends on this shape.
  const dragStartRef = useRef<{ nodes: WorkflowCanvasNode[]; edges: WorkflowCanvasEdge[] } | null>(
    null,
  )

  // The transient hint is self-clearing; unmounting must not leave a timer that
  // sets state on a dead component.
  useEffect(() => {
    return () => {
      if (hintTimer.current !== null) {
        window.clearTimeout(hintTimer.current)
      }
    }
  }, [])

  /** Raises one transient notice about a refused or auto-repaired gesture. */
  function showIterationHint(
    key: WorkflowIterationHintKey,
    params?: Record<string, string | number>,
  ): void {
    if (hintTimer.current !== null) {
      window.clearTimeout(hintTimer.current)
    }
    setHint(params === undefined ? { key } : { key, params })
    hintTimer.current = window.setTimeout(() => {
      setHint(null)
      hintTimer.current = null
    }, ITERATION_HINT_MS)
  }

  /**
   * Snapshot the graph as the author releases a drag, so containment can restore it.
   *
   * Only one node is ever in flight (multi-selection is disabled), so the dragged
   * id is enough to describe the gesture.
   */
  function handleNodeDragStart(): void {
    dragStartRef.current = { nodes: graphNodes, edges: graphEdges }
    // The move transaction opens at the finger-down of a drag, so the whole
    // gesture (including any containment correction at stop) undoes as one step.
    services.onHistoryNodeDragStart?.()
  }

  /**
   * Enforces region membership after a drag: refusals return to their start
   * position, moved members widen their frame, and the correction is written
   * back to the draft only when the graph actually changed.
   */
  function handleNodeDragStop(_event: unknown, node: WorkflowCanvasNode): void {
    const before = dragStartRef.current
    dragStartRef.current = null
    if (before === null || readOnly) {
      return
    }
    // The store holds the presented copy with the final drag positions; the
    // edges never change during a node drag and the raw graph is still current.
    const end = { nodes: getNodes(), edges: graphEdges }
    const result = applyIterationDragRules(end, before, [node.id])
    if (result.workflow !== end) {
      services.onIterationCorrection?.(result.workflow.nodes)
    }
    if (result.rejectedNodeIds.length > 0) {
      showIterationHint('workflows.iteration.useInternalAdd')
    }
    // Committing after the containment correction files one complete step:
    // the draft by then holds the final resting place of the dragged node.
    services.onHistoryNodeDragStop?.()
  }

  /** Resolves a deletion request through confirmation and the cascade repair. */
  function handleBeforeDelete(
    request: WorkflowDeletionRequest,
  ): Promise<boolean | WorkflowDeletionRequest> {
    return confirmAndResolveDeletion(request, editor, readOnly, {
      onConfirmIterationDeletion: services.onConfirmIterationDeletion,
      showIterationHint,
    })
  }

  return {
    hint,
    handleNodeDragStart,
    handleNodeDragStop,
    handleBeforeDelete,
    showIterationHint,
  }
}

/** Resolves a deletion request (keyboard delete or a card's trash button):
 * iteration containers confirm with their member count, then drag their whole
 * region out; plain deletions hint when they orphan a region's collect target.
 */
async function confirmAndResolveDeletion(
  request: WorkflowDeletionRequest,
  editor: { graphNodes: WorkflowCanvasNode[]; graphEdges: WorkflowCanvasEdge[] },
  readOnly: boolean,
  services: {
    onConfirmIterationDeletion: ((memberCount: number) => Promise<boolean>) | undefined
    showIterationHint: (
      key: WorkflowIterationHintKey,
      params?: Record<string, string | number>,
    ) => void
  },
): Promise<boolean | WorkflowDeletionRequest> {
  if (readOnly) {
    return false
  }
  const graph = { nodes: editor.graphNodes, edges: editor.graphEdges }
  const requestedNodeIds = new Set(request.nodes.map((node) => node.id))
  const cascade = resolveIterationDeletionCascade(graph, requestedNodeIds)
  const repair = repairIterationGraphAfterNodeDeletion(graph, cascade.nodeIds)
  const deletedIteration = request.nodes.find((node) => node.data.kind === 'iteration')
  const deletedContainer =
    deletedIteration ?? request.nodes.find((node) => node.data.kind === 'loop')
  if (deletedIteration !== undefined && cascade.memberCount > 0) {
    if (services.onConfirmIterationDeletion === undefined) {
      return false
    }
    const confirmed = await services.onConfirmIterationDeletion(cascade.memberCount)
    if (!confirmed) {
      return false
    }
  }
  if (repair.clearedCollectSelectorIterationIds.length > 0) {
    const iterationId = repair.clearedCollectSelectorIterationIds[0]
    if (iterationId !== undefined) {
      const title = editor.graphNodes.find((node) => node.id === iterationId)?.data.title
      if (title !== undefined) {
        services.showIterationHint('workflows.iteration.collectTargetDeleted', { name: title })
      }
    }
  }
  if (deletedContainer === undefined) {
    // An ordinary node delete: React Flow removes it and its edges, and the
    // draft's change pipeline runs the collect-selector repair it needs.
    return true
  }
  // Deleting a container takes its members and their edges along in one step.
  return {
    nodes: [...cascade.nodeIds].flatMap((id) => {
      const node = editor.graphNodes.find((candidate) => candidate.id === id)
      return node === undefined ? [] : [node]
    }),
    edges: [...cascade.edgeIds].flatMap((id) => {
      const edge = editor.graphEdges.find((candidate) => candidate.id === id)
      return edge === undefined ? [] : [edge]
    }),
  }
}

/**
 * Placement helpers that translate palette clicks and drops into flow space.
 *
 * The measured canvas box and the screen-to-flow projection are needed by both
 * the palette-dock `onAdd` and the drag-drop path, so they live together here
 * under one measurement.
 */
function useCanvasPlacement(
  graphNodes: WorkflowCanvasNode[],
  onAddNode: (kind: WorkflowNodeKind, position: XYPosition) => void,
  onOrganize: () => void,
  showIterationHint: (key: WorkflowIterationHintKey) => void,
) {
  const canvasRef = useRef<HTMLDivElement>(null)
  const { fitView, screenToFlowPosition } = useReactFlow<WorkflowCanvasNode, WorkflowCanvasEdge>()

  /** Converts a client point into a snapped top-left node position. */
  function positionAtClient(client: XYPosition): XYPosition {
    return snapNodePosition(nodePositionAt(screenToFlowPosition(client, { snapToGrid: false })))
  }

  /** Places a node in the middle of the visible canvas. */
  function addAtViewportCenter(kind: WorkflowNodeKind): void {
    const bounds = canvasRef.current?.getBoundingClientRect()
    if (bounds === undefined) {
      onAddNode(kind, nodePositionAt({ x: 0, y: 0 }))
      return
    }
    const center = { x: bounds.left + bounds.width / 2, y: bounds.top + bounds.height / 2 }
    onAddNode(kind, positionAtClient(center))
  }

  /** Applies the new layout, then frames it once React Flow has the positions. */
  function organizeAndFrame(): void {
    onOrganize()
    requestAnimationFrame(() => {
      void fitView({ duration: 240, maxZoom: 1, minZoom: MIN_ZOOM, padding: 0.16 })
    })
  }

  /**
   * Places a node where a palette drag was released, refusing releases on frames.
   *
   * Membership comes only from a region's entry seam, so a drop that would land
   * the card inside an iteration frame is refused rather than granted — the
   * canvas must never imply membership the document does not contain.
   */
  function dropAtClientPosition(kind: WorkflowNodeKind, position: XYPosition): void {
    const bounds = canvasRef.current?.getBoundingClientRect()
    if (bounds === undefined || !isInside(bounds, position)) {
      return
    }
    const flow = positionAtClient(position)
    if (workflowPointInsideIterationFrame(graphNodes, flow)) {
      showIterationHint('workflows.iteration.useInternalAdd')
      return
    }
    onAddNode(kind, flow)
  }

  return { canvasRef, addAtViewportCenter, dropAtClientPosition, organizeAndFrame }
}

/** The canvas body, inside the provider so it can reach the React Flow store. */
function WorkflowCanvasInner({
  nodes: graphNodes,
  edges: graphEdges,
  viewport,
  onNodesChange,
  onEdgesChange,
  onConnect,
  onAddNode,
  onOrganize,
  onViewportChange,
  onIterationCorrection,
  onConfirmIterationDeletion,
  onHistoryNodeDragStart,
  onHistoryNodeDragStop,
  readOnly = false,
}: WorkflowCanvasProps) {
  const { t } = useTranslation()
  const { hint, showIterationHint, handleNodeDragStart, handleNodeDragStop, handleBeforeDelete } =
    useIterationCanvasGuards({ graphNodes, graphEdges }, readOnly, {
      onIterationCorrection,
      onConfirmIterationDeletion,
      onHistoryNodeDragStart,
      onHistoryNodeDragStop,
    })
  const { canvasRef, addAtViewportCenter, dropAtClientPosition, organizeAndFrame } =
    useCanvasPlacement(graphNodes, onAddNode, onOrganize, showIterationHint)
  // The regions the draft cannot know about (folded members, constrained boxes,
  // member counts, derived ordering) are derived here instead of stored, so the
  // persisted graph never depends on this render's presentation.
  const presentation = useMemo(
    () => buildIterationRegionGraph(graphNodes, graphEdges),
    [graphNodes, graphEdges],
  )

  return (
    <div className="relative min-h-0 min-w-0 flex-1">
      <CanvasSurface canvasRef={canvasRef} label={t('workflows.editor.canvas')}>
        <ReactFlow
          className="workflow-flow bg-muted/25"
          nodes={presentation.nodes}
          edges={presentation.edges}
          nodeTypes={NODE_TYPES}
          defaultViewport={viewport ?? DEFAULT_VIEWPORT}
          minZoom={MIN_ZOOM}
          maxZoom={MAX_ZOOM}
          proOptions={{ hideAttribution: true }}
          nodesFocusable
          edgesFocusable
          nodesDraggable={!readOnly}
          nodesConnectable={!readOnly}
          elementsSelectable={!readOnly}
          deleteKeyCode={readOnly ? [] : ['Backspace', 'Delete']}
          multiSelectionKeyCode={null}
          snapGrid={WORKFLOW_SNAP_GRID}
          snapToGrid
          panOnScroll={false}
          zoomOnScroll
          zoomOnPinch
          // Middle-drag pans, which is also what a trackpad two-finger gesture
          // produces; left-drag is left for box-selection.
          panOnDrag={[1]}
          selectionOnDrag={!readOnly}
          selectNodesOnDrag={false}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChange}
          onConnect={onConnect}
          // The draft re-checks every connection; this only stops the port lighting up for a
          // target the gesture could never be connected to.
          isValidConnection={(connection) =>
            isValidWorkflowConnection(connection, { nodes: graphNodes, edges: graphEdges })
          }
          onNodeDragStart={handleNodeDragStart}
          onNodeDragStop={handleNodeDragStop}
          onBeforeDelete={handleBeforeDelete}
          onMoveEnd={(_event, nextViewport) => onViewportChange?.(nextViewport)}
          defaultEdgeOptions={DEFAULT_EDGE_OPTIONS}
        >
          <Background
            id="workflow-dots"
            variant={BackgroundVariant.Dots}
            gap={20}
            size={1}
            color="color-mix(in oklch, var(--foreground) 18%, transparent)"
          />
          <Controls showInteractive={false} />
          <MiniMap pannable zoomable />
        </ReactFlow>
      </CanvasSurface>
      {hint !== null && <IterationHintPill message={t(hint.key, hint.params ?? {})} />}
      {!readOnly && (
        <CanvasOverlays
          hasStartNode={graphNodes.some((node) => node.data.kind === 'start')}
          onAdd={addAtViewportCenter}
          onDrop={dropAtClientPosition}
          onOrganize={organizeAndFrame}
        />
      )}
    </div>
  )
}

/** A short, self-clearing notice over the canvas about the last gesture. */
function IterationHintPill({ message }: { message: string }) {
  return (
    <div
      data-workflow-iteration-hint
      className="pointer-events-none absolute bottom-20 left-1/2 z-40 -translate-x-1/2 rounded-md bg-foreground/90 px-3 py-1.5 text-xs text-background shadow-md"
    >
      {message}
    </div>
  )
}

/** The measured, focusable box React Flow mounts into. */
function CanvasSurface({
  canvasRef,
  label,
  children,
}: {
  canvasRef: RefObject<HTMLDivElement | null>
  label: string
  children: ReactNode
}) {
  return (
    <div
      ref={canvasRef}
      aria-label={label}
      className="absolute inset-0 touch-none outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
    >
      {children}
    </div>
  )
}

/** Everything the editor floats over the canvas: arrange, and the node dock. */
function CanvasOverlays({
  hasStartNode,
  onAdd,
  onDrop,
  onOrganize,
}: {
  hasStartNode: boolean
  onAdd: (kind: WorkflowNodeKind) => void
  onDrop: (kind: WorkflowNodeKind, position: XYPosition) => void
  onOrganize: () => void
}) {
  const { t } = useTranslation()
  return (
    <>
      <div className="absolute top-3 right-3 z-30">
        <Button
          variant="outline"
          size="sm"
          title={t('workflows.editor.organizeHint')}
          onClick={onOrganize}
        >
          <LayoutDashboard className="size-3.5" />
          {t('workflows.editor.organize')}
        </Button>
      </div>
      <div
        data-workflow-controls
        className="absolute bottom-3 left-1/2 z-30 w-fit max-w-[calc(100%-6rem)] -translate-x-1/2"
      >
        <WorkflowNodeCatalog hasStartNode={hasStartNode} onAdd={onAdd} onDrop={onDrop} />
      </div>
    </>
  )
}

/** Whether a client point falls inside a measured box. */
function isInside(bounds: DOMRect, point: XYPosition): boolean {
  return (
    point.x >= bounds.left &&
    point.x <= bounds.right &&
    point.y >= bounds.top &&
    point.y <= bounds.bottom
  )
}
