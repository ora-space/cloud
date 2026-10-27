import { useLayoutEffect, useRef, useState } from 'react'
import { Handle, Position, useUpdateNodeInternals } from '@xyflow/react'
import { useTranslation } from 'react-i18next'
import {
  IterationInsertMenu,
  useOptionalWorkflowIterationActions,
} from '@/features/workflows/editor/workflow-iteration-actions'
import { resolveConditionCases } from '@/features/workflows/runtime/condition-cases'
import { WORKFLOW_NODE_ANCHOR_Y } from '@/features/workflows/runtime/node-geometry'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'

/**
 * Condition ports retain their case ids through connection gestures. The implicit ELSE has its
 * own stable port; other executable nodes keep their ordinary source and loopExit has none.
 *
 * Inside a loop body every port also opens the append menu: clicking the port (not dragging
 * from it) offers the node kinds a round may run, so a connection drag is never intercepted.
 */
export function WorkflowNodeSourceHandles({
  nodeId,
  data,
}: {
  nodeId: string
  data: WorkflowNodeData
}) {
  const { t } = useTranslation()
  const updateNodeInternals = useUpdateNodeInternals()
  const branches = resolveConditionCases(data).map((branch, index) => ({
    id: branch.id,
    label: index === 0 ? 'IF' : `ELIF ${index}`,
  }))
  const portIdentity = `${nodeId}:${data.kind === 'condition' ? JSON.stringify(branches) : data.kind}`
  const measuredIdentity = useRef<string | undefined>(undefined)
  useLayoutEffect(() => {
    // React Flow measures handles separately from node data; case edits must invalidate that
    // registry after the new port DOM exists or connections retain the removed branch positions.
    if (measuredIdentity.current !== portIdentity) {
      measuredIdentity.current = portIdentity
      updateNodeInternals(nodeId)
    }
  }, [nodeId, portIdentity, updateNodeInternals])

  if (data.kind === 'loopExit') {
    return null
  }
  if (data.kind !== 'condition') {
    return (
      <SourcePort
        nodeId={nodeId}
        handleId={null}
        top={WORKFLOW_NODE_ANCHOR_Y}
        connectLabel={t('workflows.editor.connectFrom', { name: data.title })}
        addLabel={t('workflows.loop.addAfter', { name: data.title })}
      />
    )
  }
  return (
    <div className="mt-2 border-t border-border py-1">
      {[...branches, { id: 'else', label: 'ELSE' }].map((branch) => (
        <div key={branch.id} className="relative flex h-8 items-center justify-end px-4">
          <span className="text-[10px] font-semibold text-muted-foreground">{branch.label}</span>
          <SourcePort
            nodeId={nodeId}
            handleId={branch.id}
            top="50%"
            connectLabel={t('workflows.condition.connectFromBranch', {
              name: data.title,
              branch: branch.label,
            })}
            addLabel={t('workflows.loop.addAfterBranch', {
              name: data.title,
              branch: branch.label,
            })}
          />
        </div>
      ))}
    </div>
  )
}

/**
 * One output port, plus the loop-body append menu when the port belongs to a loop member.
 *
 * The blue plus is decorative and centered on the port; the port's own click opens the menu,
 * the same arrangement as the iteration entry, so dragging a connection still starts from the
 * port. Outside a loop (or on a read-only canvas) this is the plain handle.
 */
function SourcePort({
  nodeId,
  handleId,
  top,
  connectLabel,
  addLabel,
}: {
  nodeId: string
  handleId: string | null
  top: number | string
  connectLabel: string
  addLabel: string
}) {
  const actions = useOptionalWorkflowIterationActions()
  const insertion = actions?.loopOutputInsertion(nodeId, handleId) ?? null
  const [menuOpen, setMenuOpen] = useState(false)
  return (
    <>
      <Handle
        {...(handleId === null ? {} : { id: handleId })}
        type="source"
        position={Position.Right}
        aria-label={connectLabel}
        className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
        style={{ top }}
        onClick={
          insertion === null
            ? undefined
            : (event) => {
                // The port click opens the menu; it must not also select the card under it.
                event.stopPropagation()
                setMenuOpen((open) => !open)
              }
        }
      />
      {insertion !== null && (
        <div className="pointer-events-none absolute right-0" style={{ top }}>
          <IterationInsertMenu
            insertion={insertion}
            label={addLabel}
            open={menuOpen}
            onOpenChange={setMenuOpen}
            className="pointer-events-none absolute top-0 left-0 z-10 -translate-x-1/2 -translate-y-1/2 opacity-0 transition-opacity duration-150 group-hover/workflow-node:opacity-100 focus-visible:opacity-100"
          />
        </div>
      )}
    </>
  )
}
