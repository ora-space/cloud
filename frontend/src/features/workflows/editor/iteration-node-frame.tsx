import { useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import {
  Handle,
  NodeResizeControl,
  Position,
  ResizeControlVariant,
  useReactFlow,
  useUpdateNodeInternals,
} from '@xyflow/react'
import { useTranslation } from 'react-i18next'
import { ChevronDown, ChevronUp, Home, Trash, type LucideIcon } from 'lucide-react'
import { cn } from '@/lib/utils'
import {
  WORKFLOW_ITERATION_COLLAPSED_HEIGHT,
  WORKFLOW_ITERATION_COLLAPSED_WIDTH,
  WORKFLOW_ITERATION_ENTRY_ANCHOR_Y,
  WORKFLOW_ITERATION_NODE_HEIGHT,
  WORKFLOW_ITERATION_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import {
  IterationInsertMenu,
  useWorkflowIterationActions,
} from '@/features/workflows/editor/workflow-iteration-actions'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'

/** Matches the internal-start block: a 44px card wrapping a blue home badge. */
const ITERATION_START_SIZE = 44
const ITERATION_START_LEFT = 24
/** Dify-style corner affordance: a forgiving resize hit zone flush with the corner. */
const ITERATION_RESIZE_HANDLE_SIZE = 24
/** Dify's resize glyph: one soft arc hugging the rounded corner. */
const ITERATION_RESIZE_ARC_PATH = 'M5.19009 11.8398C8.26416 10.6196 10.7144 8.16562 11.9297 5.08904'
/** Height of the frame's title strip, which centers the outer handles. */
const ITERATION_HEADER_HEIGHT = 44

/** The subset of a canvas node the frame renders from. */
export interface IterationNodeFrameProps {
  id: string
  data: WorkflowCanvasNode['data']
  selected: boolean
  deletable: boolean
  /** Replaces the member-count chip, such as the run viewer's item summary. */
  badge?: ReactNode
}

/**
 * Renders one iteration composite region on the canvas.
 *
 * The frame is presentation-only: its members are ordinary cards sharing the
 * iteration's id as `parentId`, the title strip carries the region's controls,
 * and folding hides the members rather than removing them from the persisted
 * graph. The frame's size is authored (`initialWidth` / `initialHeight`) so an
 * empty region stays droppable and a save/load round trip survives.
 */
export function IterationNodeFrame({
  id,
  data,
  selected,
  deletable,
  badge,
}: IterationNodeFrameProps) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const updateNodeInternals = useUpdateNodeInternals()
  const iterationActions = useWorkflowIterationActions()
  const { getNode } = useReactFlow<WorkflowCanvasNode>()
  const collapsed = data.collapsed === true
  const memberCount = data.regionMemberCount ?? 0
  const metadata = workflowNodeMetadata(data.kind)
  const kindLabel = translate(workflowNodeTypeDefinition(data.kind).labelKey)
  const canDelete = deletable ?? true
  const frame = getNode(id)
  const expandedWidth = Math.max(
    WORKFLOW_ITERATION_NODE_WIDTH,
    frame?.initialWidth ?? WORKFLOW_ITERATION_NODE_WIDTH,
  )
  const expandedHeight = Math.max(
    WORKFLOW_ITERATION_NODE_HEIGHT,
    frame?.initialHeight ?? WORKFLOW_ITERATION_NODE_HEIGHT,
  )

  // The internal start handle exists only in expanded mode, so folding or
  // unfolding swaps which handles React Flow must know about. Refreshing after
  // the DOM commit keeps that registry aligned with the chrome; the ref tracks
  // the collapsed flip so a re-render that hasn't actually folded (a selection,
  // a hover) doesn't fire the refresh.
  const lastCollapsed = useRef(collapsed)
  useLayoutEffect(() => {
    if (lastCollapsed.current === collapsed) {
      return
    }
    lastCollapsed.current = collapsed
    updateNodeInternals(id)
  }, [collapsed, id, updateNodeInternals])

  return (
    <div
      data-workflow-node
      data-workflow-node-id={id}
      data-workflow-iteration-frame
      data-collapsed={collapsed}
      aria-label={`${kindLabel}: ${data.title}`}
      className={cn(
        'group/iteration-frame relative overflow-visible rounded-2xl border bg-violet-500/[0.035] shadow-sm transition-[border-color,box-shadow]',
        selected
          ? 'border-foreground/45 shadow-md ring-2 ring-ring/25'
          : 'border-violet-500/40 hover:shadow-md',
      )}
      style={{
        width: collapsed ? WORKFLOW_ITERATION_COLLAPSED_WIDTH : expandedWidth,
        height: collapsed ? WORKFLOW_ITERATION_COLLAPSED_HEIGHT : expandedHeight,
      }}
    >
      <Handle
        type="target"
        position={Position.Left}
        aria-label={t('workflows.editor.connectTo', { name: data.title })}
        className="workflow-port workflow-port-input !size-2.5 !border-0 !bg-transparent"
        style={{ top: ITERATION_HEADER_HEIGHT / 2 }}
      />
      <Handle
        type="source"
        position={Position.Right}
        aria-label={t('workflows.editor.connectFrom', { name: data.title })}
        className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
        style={{ top: ITERATION_HEADER_HEIGHT / 2 }}
      />
      <IterationFrameHeader
        id={id}
        icon={metadata.icon}
        tone={metadata.tone}
        title={data.title}
        memberCount={memberCount}
        badge={badge}
        collapsed={collapsed}
        selected={selected}
        canDelete={canDelete}
      />
      {!collapsed && (
        <>
          {memberCount === 0 && (
            <span className="pointer-events-none absolute top-[60px] right-3 text-[11px] text-muted-foreground">
              {t('workflows.iteration.emptyHint')}
            </span>
          )}
          <IterationStartSeam id={id} title={data.title} />
          {!iterationActions.readOnly && (
            <ContainerFrameResize
              minWidth={WORKFLOW_ITERATION_NODE_WIDTH}
              minHeight={WORKFLOW_ITERATION_NODE_HEIGHT}
            />
          )}
        </>
      )}
    </div>
  )
}

/**
 * The region's title strip: the kind tile, the name, the member count, and the
 * fold and delete controls. Hidden controls leave no dead buttons on the
 * read-only detail canvas, whose provider passes no callbacks.
 */
function IterationFrameHeader({
  id,
  icon: Icon,
  tone,
  title,
  memberCount,
  badge,
  collapsed,
  selected,
  canDelete,
}: {
  id: string
  icon: LucideIcon
  tone: string
  title: string
  memberCount: number
  badge: ReactNode | undefined
  collapsed: boolean
  selected: boolean
  canDelete: boolean
}) {
  const { t } = useTranslation()
  const { deleteElements } = useReactFlow<WorkflowCanvasNode>()
  const { readOnly, toggleCollapsed } = useWorkflowIterationActions()
  return (
    <div
      className="relative z-20 flex items-center gap-2 border-b border-violet-500/20 px-3"
      style={{ height: ITERATION_HEADER_HEIGHT }}
    >
      <span className={cn('flex size-7 shrink-0 items-center justify-center rounded-lg', tone)}>
        <Icon className="size-4" strokeWidth={1.9} />
      </span>
      <span className="min-w-0 flex-1 truncate text-sm font-semibold">{title}</span>
      {badge ?? (
        <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
          {t('workflows.iteration.regionSummary', { total: memberCount })}
        </span>
      )}
      {!readOnly && (
        <button
          type="button"
          className="nodrag nopan flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={t(collapsed ? 'workflows.iteration.expand' : 'workflows.iteration.collapse')}
          onClick={() => toggleCollapsed(id)}
        >
          {collapsed ? <ChevronUp className="size-4" /> : <ChevronDown className="size-4" />}
        </button>
      )}
      {selected && canDelete && !readOnly && (
        <button
          type="button"
          className="nodrag nopan flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-destructive/10 hover:text-destructive focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={t('workflows.editor.deleteNode', { name: title })}
          onClick={() => {
            void deleteElements({ nodes: [{ id }] })
          }}
        >
          <Trash className="size-3.5" />
        </button>
      )}
    </div>
  )
}

/**
 * The internal start of the region: a small blue badge whose port doubles as the
 * trigger for the member picker, so a connection drag from the handle is never
 * intercepted by a decorative circle.
 */
function IterationStartSeam({ id, title }: { id: string; title: string }) {
  const { t } = useTranslation()
  const iterationActions = useWorkflowIterationActions()
  const [entryMenuOpen, setEntryMenuOpen] = useState(false)
  return (
    <div
      className="nodrag nopan absolute z-20"
      style={{
        left: ITERATION_START_LEFT,
        top: WORKFLOW_ITERATION_ENTRY_ANCHOR_Y - ITERATION_START_SIZE / 2,
      }}
    >
      <div
        data-workflow-iteration-start
        role="img"
        aria-label={t('workflows.iteration.internalStart')}
        title={t('workflows.iteration.internalStart')}
        className="relative flex size-11 items-center justify-center rounded-xl border border-border bg-card shadow-xs"
        onClick={iterationActions.readOnly ? undefined : () => setEntryMenuOpen((open) => !open)}
      >
        <span className="flex size-6 items-center justify-center rounded-full bg-blue-600 text-white">
          <Home className="size-3" aria-hidden="true" />
        </span>
        <Handle
          id="iteration-entry"
          type="source"
          position={Position.Right}
          aria-label={t('workflows.iteration.entryHandle', { name: title })}
          className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
          onClick={
            iterationActions.readOnly
              ? undefined
              : (event) => {
                  // The port click must not double-toggle through the badge
                  // handler that widens the entry affordance.
                  event.stopPropagation()
                  setEntryMenuOpen((open) => !open)
                }
          }
        />
      </div>
      {/* The blue plus stays decorative and centered on the entry port; the
          port itself opens the picker, so connection drags are never blocked. */}
      <IterationInsertMenu
        insertion={{ type: 'entry', iterationId: id }}
        label={t('workflows.iteration.addNode')}
        open={entryMenuOpen}
        onOpenChange={setEntryMenuOpen}
        className="pointer-events-none absolute top-1/2 left-full z-10 -translate-x-1/2 -translate-y-1/2 opacity-0 transition-opacity duration-150 group-hover/iteration-frame:opacity-100 focus-visible:opacity-100"
      />
    </div>
  )
}

/**
 * The Dify-style corner resize affordance with a forgiving hit zone, shared by every container
 * frame. The gesture's `resizing` dimension changes are what the draft mirrors onto the
 * authored `initialWidth` / `initialHeight`.
 *
 * @param props.minWidth - Smallest box the gesture may leave; the container's default size.
 */
export function ContainerFrameResize({
  minWidth,
  minHeight,
}: {
  minWidth: number
  minHeight: number
}) {
  return (
    <NodeResizeControl
      variant={ResizeControlVariant.Handle}
      position="bottom-right"
      minWidth={minWidth}
      minHeight={minHeight}
      className="group/iteration-resize"
      style={{
        left: 'auto',
        top: 'auto',
        right: 0,
        bottom: 0,
        width: ITERATION_RESIZE_HANDLE_SIZE,
        height: ITERATION_RESIZE_HANDLE_SIZE,
        translate: 'none',
        border: 'none',
        backgroundColor: 'transparent',
      }}
    >
      {/* The built-in handle pins itself to the frame corner with a small dot;
          the larger in-corner zone keeps the gesture forgiving while staying
          clear of the rounded border. */}
      <svg
        aria-hidden="true"
        width={16}
        height={16}
        viewBox="0 0 16 16"
        fill="none"
        className={cn(
          'absolute right-px bottom-px text-foreground opacity-0 transition-opacity duration-150 group-hover/iteration-frame:opacity-100 group-hover/loop-frame:opacity-100 group-hover/iteration-resize:opacity-100',
        )}
      >
        <path
          d={ITERATION_RESIZE_ARC_PATH}
          stroke="currentColor"
          strokeOpacity="0.16"
          strokeWidth="2"
          strokeLinecap="round"
        />
      </svg>
    </NodeResizeControl>
  )
}
