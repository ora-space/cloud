import { useLayoutEffect, useRef } from 'react'
import { Handle, Position, useUpdateNodeInternals } from '@xyflow/react'
import { useTranslation } from 'react-i18next'
import { resolveConditionCases } from '@/features/workflows/runtime/condition-cases'
import { WORKFLOW_NODE_ANCHOR_Y } from '@/features/workflows/runtime/node-geometry'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'

/**
 * Condition ports retain their case ids through connection gestures. The implicit ELSE has its
 * own stable port; other executable nodes keep their ordinary source and loopExit has none.
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
      <Handle
        type="source"
        position={Position.Right}
        aria-label={t('workflows.editor.connectFrom', { name: data.title })}
        className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
        style={{ top: WORKFLOW_NODE_ANCHOR_Y }}
      />
    )
  }
  return (
    <div className="mt-2 border-t border-border py-1">
      {[...branches, { id: 'else', label: 'ELSE' }].map((branch) => (
        <div key={branch.id} className="relative flex h-8 items-center justify-end px-4">
          <span className="text-[10px] font-semibold text-muted-foreground">{branch.label}</span>
          <Handle
            id={branch.id}
            type="source"
            position={Position.Right}
            aria-label={t('workflows.condition.connectFromBranch', {
              name: data.title,
              branch: branch.label,
            })}
            className="workflow-port workflow-port-output !size-2.5 !border-0 !bg-transparent"
            style={{ top: '50%' }}
          />
        </div>
      ))}
    </div>
  )
}
