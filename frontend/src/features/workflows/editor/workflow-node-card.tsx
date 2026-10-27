import { memo } from 'react'
import { Handle, Position, useReactFlow, type NodeProps } from '@xyflow/react'
import { useTranslation } from 'react-i18next'
import { Trash } from 'lucide-react'
import { cn } from '@/lib/utils'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import {
  WORKFLOW_NODE_ANCHOR_Y,
  WORKFLOW_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import { NodeParameterSummary } from '@/features/workflows/editor/node-parameter-summary'
import { IterationNodeFrame } from '@/features/workflows/editor/iteration-node-frame'
import { LoopNodeFrame } from '@/features/workflows/editor/loop-node-frame'
import { NodeIdentity } from '@/features/workflows/editor/node-identity'
import { WorkflowNodeSourceHandles } from '@/features/workflows/editor/workflow-node-source-handles'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import type { WorkflowCanvasNode } from '@/features/workflows/editor/canvas-types'

/**
 * One workflow card on the canvas.
 *
 * The card is a pure view of `data` plus React Flow's selection state; every
 * edit goes through the editor, which is the only writer of the graph. The
 * delete control therefore asks React Flow to emit a removal change rather than
 * touching the graph itself.
 */
export const WorkflowNodeCard = memo(function WorkflowNodeCard({
  id,
  data,
  selected,
  deletable,
}: NodeProps<WorkflowCanvasNode>) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const { deleteElements } = useReactFlow<WorkflowCanvasNode>()
  const metadata = workflowNodeMetadata(data.kind)
  const Icon = metadata.icon
  const kindLabel = translate(workflowNodeTypeDefinition(data.kind).labelKey)
  // React Flow deletes any node whose `deletable` is not explicitly false, so
  // the card has to offer the control on the same terms.
  const canDelete = deletable ?? true

  if (data.kind === 'iteration') {
    return <IterationNodeFrame id={id} data={data} selected={selected} deletable={deletable} />
  }
  if (data.kind === 'loop') {
    return <LoopNodeFrame id={id} data={data} selected={selected} deletable={canDelete} />
  }

  return (
    <article
      data-workflow-node-id={id}
      aria-label={`${kindLabel}: ${data.title}`}
      style={{ width: WORKFLOW_NODE_WIDTH }}
      className={cn(
        'group/workflow-node rounded-xl border bg-card shadow-sm outline-none transition-[border-color,box-shadow] duration-200',
        selected
          ? 'border-foreground/45 shadow-md ring-2 ring-ring/25'
          : 'border-border hover:border-foreground/25 hover:shadow-md',
      )}
    >
      <Handle
        type="target"
        position={Position.Left}
        aria-label={t('workflows.editor.connectTo', { name: data.title })}
        className="workflow-port workflow-port-input !size-2.5 !border-0 !bg-transparent"
        style={{ top: WORKFLOW_NODE_ANCHOR_Y }}
      />
      <NodeIdentity
        id={id}
        title={data.title}
        description={data.description}
        kindLabel={kindLabel}
        tone={metadata.tone}
        icon={Icon}
        action={
          selected &&
          canDelete && (
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
          )
        }
      />
      <NodeParameterSummary data={data} />
      <WorkflowNodeSourceHandles nodeId={id} data={data} />
    </article>
  )
})
