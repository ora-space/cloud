import type { ReactNode } from 'react'
import { Handle, Position, useReactFlow, useStore, type ReactFlowState } from '@xyflow/react'
import { useTranslation } from 'react-i18next'
import { Trash } from 'lucide-react'
import { cn } from '@/lib/utils'
import {
  WORKFLOW_LOOP_MIN_HEIGHT,
  WORKFLOW_LOOP_MIN_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import { loopAuthoredSize, loopContentBox } from '@/features/workflows/runtime/loop-graph'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { useWorkflowIterationActions } from '@/features/workflows/editor/workflow-iteration-actions'
import { ContainerFrameResize } from '@/features/workflows/editor/iteration-node-frame'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'

/** Height of the frame's title strip, which centers the outer handles. */
const LOOP_HEADER_HEIGHT = 44

/**
 * The box the Loop's measured members need, as `"width:height"` so the selector result is
 * compared by value and the frame re-renders only when the needed box changes.
 */
function contentBoxSelector(loopId: string): (state: ReactFlowState) => string {
  return (state) => {
    const box = loopContentBox(state.nodes, loopId)
    return `${box.width}:${box.height}`
  }
}

/** The subset of a canvas node the frame renders from. */
export interface LoopNodeFrameProps {
  id: string
  data: WorkflowCanvasNode['data']
  selected: boolean
  deletable: boolean
  /** Extra chrome in the title strip, such as the run viewer's round summary. */
  badge?: ReactNode
}

/**
 * Renders one Loop container on the canvas.
 *
 * The frame is presentation-only: the body is ordinary cards owning the Loop through
 * `parentId` and `containerId`, its entry is the real body start, and the box grows with
 * its members (`expandParent`) or by the corner resize, both persisted as the Loop's
 * `initialWidth` / `initialHeight`. Outer handles sit on the title strip, so edges into and
 * out of the Loop read as edges of the whole round, not of one member.
 */
export function LoopNodeFrame({ id, data, selected, deletable, badge }: LoopNodeFrameProps) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const { readOnly } = useWorkflowIterationActions()
  const { deleteElements, getNode } = useReactFlow<WorkflowCanvasNode>()
  const metadata = workflowNodeMetadata(data.kind)
  const Icon = metadata.icon
  const kindLabel = translate(workflowNodeTypeDefinition(data.kind).labelKey)
  const frame = getNode(id)
  // Authored size and members only: never the size React Flow measured or wrote back, which is
  // what this frame drew last time and would keep it from shrinking.
  const authored = frame === undefined ? { width: 0, height: 0 } : loopAuthoredSize(frame)
  const [contentWidth = 0, contentHeight = 0] = useStore(contentBoxSelector(id))
    .split(':')
    .map(Number)
  const boxWidth = Math.max(WORKFLOW_LOOP_MIN_WIDTH, authored.width, contentWidth)
  const boxHeight = Math.max(WORKFLOW_LOOP_MIN_HEIGHT, authored.height, contentHeight)
  const maxIterations = data.loopConfig?.maxIterations

  return (
    <div
      data-workflow-node
      data-workflow-node-id={id}
      data-workflow-loop-frame
      aria-label={`${kindLabel}: ${data.title}`}
      className={cn(
        'group/loop-frame relative overflow-visible rounded-2xl border bg-amber-500/[0.035] shadow-sm transition-[border-color,box-shadow]',
        selected
          ? 'border-foreground/45 shadow-md ring-2 ring-ring/25'
          : 'border-amber-500/40 hover:shadow-md',
      )}
      style={{ width: boxWidth, height: boxHeight }}
    >
      <Handle
        type="target"
        position={Position.Left}
        aria-label={t('workflows.editor.connectTo', { name: data.title })}
        className="workflow-port workflow-port-input !size-2.5 !border-0 !bg-transparent"
        style={{ top: LOOP_HEADER_HEIGHT / 2 }}
      />
      <Handle
        type="source"
        position={Position.Right}
        aria-label={t('workflows.editor.connectFrom', { name: data.title })}
        className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
        style={{ top: LOOP_HEADER_HEIGHT / 2 }}
      />
      <div
        className="relative z-20 flex items-center gap-2 border-b border-amber-500/20 px-3"
        style={{ height: LOOP_HEADER_HEIGHT }}
      >
        <span
          className={cn(
            'flex size-7 shrink-0 items-center justify-center rounded-lg',
            metadata.tone,
          )}
        >
          <Icon className="size-4" strokeWidth={1.9} />
        </span>
        <span className="min-w-0 flex-1 truncate text-sm font-semibold">{data.title}</span>
        {badge ??
          (maxIterations !== undefined && (
            <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
              {t('workflows.loop.frameSummary', { max: maxIterations })}
            </span>
          ))}
        {selected && deletable && !readOnly && (
          <button
            type="button"
            className="nodrag nopan flex size-7 shrink-0 items-center justify-center rounded-md text-muted-foreground outline-none hover:bg-destructive/10 hover:text-destructive focus-visible:ring-2 focus-visible:ring-ring"
            aria-label={t('workflows.editor.deleteNode', { name: data.title })}
            onClick={() => {
              void deleteElements({ nodes: [{ id }] })
            }}
          >
            <Trash className="size-3.5" />
          </button>
        )}
      </div>
      {!readOnly && (
        <ContainerFrameResize
          minWidth={WORKFLOW_LOOP_MIN_WIDTH}
          minHeight={WORKFLOW_LOOP_MIN_HEIGHT}
        />
      )}
    </div>
  )
}
