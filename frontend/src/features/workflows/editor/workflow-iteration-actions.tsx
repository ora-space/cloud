import { createContext, useContext, useMemo, type ReactNode } from 'react'
import { Plus } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import {
  workflowNodeTypeDefinition,
  workflowPaletteNodeTypes,
  type WorkflowNodeTypeDefinition,
} from '@/features/workflows/runtime/node-catalog'
import {
  loopOutputInsertion,
  type WorkflowContainerInsertion,
  type WorkflowLoopOutputInsertion,
} from '@/features/workflows/runtime/loop-graph'
import type { WorkflowIterationGraph } from '@/features/workflows/runtime/iteration-graph'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/**
 * Graph-aware container authoring actions exposed to composite chrome.
 *
 * Frames, their insert seams and loop-member ports read this context instead of
 * accepting one callback each, because the same menu renders at several seams and
 * every seam needs the same gate: which node kinds the container accepts and
 * whether the canvas is read-only.
 */
export interface WorkflowIterationActions {
  /** Palette entries a seam accepts, in menu order: loop bodies and iteration regions differ. */
  nodeTypesFor: (insertion: WorkflowContainerInsertion) => readonly WorkflowNodeTypeDefinition[]
  readOnly: boolean
  /** Adds a member of `kind` through an explicit container insertion seam. */
  insert: (kind: WorkflowNodeKind, insertion: WorkflowContainerInsertion) => void
  /** Folds or unfolds one iteration frame; members stay in the graph. */
  toggleCollapsed: (iterationId: string) => void
  /**
   * The append seam behind one output port of a loop body member, or `null` when the port
   * belongs to a node outside a loop (or the canvas is read-only).
   */
  loopOutputInsertion: (
    nodeId: string,
    sourceHandle: string | null,
  ) => WorkflowLoopOutputInsertion | null
}

/** The editor scope whose palette a seam offers. */
function insertionScope(insertion: WorkflowContainerInsertion): 'loop' | 'iteration' {
  return insertion.type === 'loop-output' ? 'loop' : 'iteration'
}

const WorkflowIterationActionsContext = createContext<WorkflowIterationActions | null>(null)

/** Reads iteration graph actions; only valid below the canvas provider. */
export function useWorkflowIterationActions(): WorkflowIterationActions {
  const value = useContext(WorkflowIterationActionsContext)
  if (value === null) {
    throw new Error('useWorkflowIterationActions requires WorkflowIterationActionsProvider')
  }
  return value
}

/**
 * Reads container actions where a card may also render outside an authoring canvas (a bare
 * React Flow in a test, a preview): without a provider there are no seams to offer.
 */
export function useOptionalWorkflowIterationActions(): WorkflowIterationActions | null {
  return useContext(WorkflowIterationActionsContext)
}

/**
 * Provides graph-aware container authoring actions to custom nodes.
 *
 * @param props.graph - The persisted graph, read to decide which ports open a loop seam.
 */
export function WorkflowIterationActionsProvider({
  graph,
  readOnly,
  onInsert,
  onToggleCollapsed,
  children,
}: {
  graph: WorkflowIterationGraph
  readOnly: boolean
  onInsert: (kind: WorkflowNodeKind, insertion: WorkflowContainerInsertion) => void
  onToggleCollapsed: (iterationId: string) => void
  children: ReactNode
}) {
  const value = useMemo<WorkflowIterationActions>(
    () => ({
      nodeTypesFor: (insertion) => workflowPaletteNodeTypes(insertionScope(insertion)),
      readOnly,
      insert: onInsert,
      toggleCollapsed: onToggleCollapsed,
      loopOutputInsertion: (nodeId, sourceHandle) =>
        readOnly ? null : loopOutputInsertion(graph, nodeId, sourceHandle),
    }),
    [graph, onInsert, onToggleCollapsed, readOnly],
  )
  return (
    <WorkflowIterationActionsContext.Provider value={value}>
      {children}
    </WorkflowIterationActionsContext.Provider>
  )
}

/** Props shared by every insert-seam trigger. */
interface IterationInsertMenuProps {
  /** The graph seam the picker inserts at. */
  insertion: WorkflowContainerInsertion
  /** Accessible label of the trigger, naming the seam. */
  label: string
  /** Controlled menu state for seams whose port doubles as the trigger. */
  open?: boolean
  onOpenChange?: (open: boolean) => void
  /** Extra classes for the blue affordance button. */
  className?: string
}

/**
 * The capability-filtered node picker for one region insertion seam.
 *
 * The trigger mirrors the entry affordance: a solid blue circle with a white
 * plus. Seams keep the circle decorative (`pointer-events-none`) and open the
 * menu from the underlying port's click instead, so a connection drag from the
 * handle is never intercepted.
 */
export function IterationInsertMenu({
  insertion,
  label,
  open,
  onOpenChange,
  className,
}: IterationInsertMenuProps) {
  const translate = useWorkflowTranslator()
  const { nodeTypesFor, readOnly, insert } = useWorkflowIterationActions()
  const nodeTypes = nodeTypesFor(insertion)
  if (readOnly || nodeTypes.length === 0) {
    return null
  }
  return (
    <DropdownMenu open={open} onOpenChange={onOpenChange}>
      <DropdownMenuTrigger
        render={
          <Button
            type="button"
            variant="default"
            size="icon-xs"
            aria-label={label}
            className={cn(
              'nodrag nopan rounded-full bg-blue-600 text-white shadow-sm hover:bg-blue-700',
              className,
            )}
          />
        }
      >
        <Plus className="size-3" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="center" className="w-44">
        {nodeTypes.map((nodeType) => {
          const Icon = workflowNodeMetadata(nodeType.kind).icon
          return (
            <DropdownMenuItem
              key={nodeType.kind}
              className="gap-2 text-xs"
              onClick={() => insert(nodeType.kind, { ...insertion })}
            >
              <Icon className="size-3.5" />
              {translate(workflowNodeTypeDefinition(nodeType.kind).labelKey)}
            </DropdownMenuItem>
          )
        })}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
