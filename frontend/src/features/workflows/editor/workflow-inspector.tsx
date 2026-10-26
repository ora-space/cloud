import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { PanelRightClose, SlidersHorizontal } from 'lucide-react'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import {
  decorateWorkflowVariableCatalog,
  deriveWorkflowVariableCatalog,
  normalizeWorkflowGlobalVariables,
} from '@/features/workflows/runtime/variable-catalog'
import type { WorkflowGlobalVariable, WorkflowNodeData } from '@/features/workflows/runtime/types'
import {
  AgentNodeFields,
  NodeDescriptionField,
  StartNodeFields,
  UnconfigurableNotice,
} from '@/features/workflows/editor/workflow-inspector-fields'
import { ConditionNodeFields } from '@/features/workflows/editor/workflow-condition-fields'
import { OutputNodeFields } from '@/features/workflows/editor/workflow-output-fields'
import {
  HumanNodeFields,
  JunctionNodeFields,
  LoopExitNodeFields,
  SubflowNodeFields,
  ToolNodeFields,
  type WorkflowKindFieldsProps,
} from '@/features/workflows/editor/workflow-kind-fields'
import { AggregatorNodeFields } from '@/features/workflows/editor/workflow-aggregator-fields'
import { LoopNodeFields } from '@/features/workflows/editor/workflow-loop-fields'
import { IterationNodeFields } from '@/features/workflows/editor/workflow-iteration-fields'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import { workflowNodeMetadata } from '@/features/workflows/editor/node-metadata'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'

/** Props for {@link WorkflowInspector}. */
export interface WorkflowInspectorProps {
  /** Selected node, or `null` when the canvas has no selection. */
  node: WorkflowCanvasNode | null
  /** Replaces the selected node's data with an edited copy. */
  onUpdate: (data: WorkflowNodeData) => void
  /** Adds a loopExit into the selected loop's body. */
  onAddLoopExit?: (loopId: string) => void
  /** Collapses the panel. */
  onClose: () => void
  /** Whole-graph nodes, from which the selectable variable catalog is derived. */
  nodes: WorkflowCanvasNode[]
  /** Whole-graph edges, which decide what counts as upstream for the catalog. */
  edges: WorkflowCanvasEdge[]
  /** Workflow-wide declarations, normalized before they seed the catalog. */
  globalVariables: readonly WorkflowGlobalVariable[]
}

/**
 * The right rail: everything about the selected node, and nothing about the graph.
 *
 * Keeping node editing here rather than on the card means the canvas stays a
 * view of the draft, so there is exactly one place that can change it.
 *
 * @param props - The selection, the callbacks the rail needs, and the graph the
 * variable catalog is derived from.
 */
export function WorkflowInspector({
  node,
  onUpdate,
  onAddLoopExit,
  onClose,
  nodes,
  edges,
  globalVariables,
}: WorkflowInspectorProps) {
  if (node === null) {
    return <WorkflowInspectorEmpty />
  }
  return (
    <WorkflowNodeInspector
      node={node}
      onUpdate={onUpdate}
      {...(onAddLoopExit === undefined ? {} : { onAddLoopExit })}
      onClose={onClose}
      nodes={nodes}
      edges={edges}
      globalVariables={globalVariables}
    />
  )
}

/** Shown when the rail is open but nothing is selected. */
function WorkflowInspectorEmpty() {
  const { t } = useTranslation()
  return (
    <aside className="flex h-full min-h-0 w-full min-w-0 flex-1 flex-col overflow-hidden border-l border-border bg-background">
      <div className="border-b border-border px-4 py-3">
        <h3 className="text-xs font-semibold">{t('workflows.inspector.configuration')}</h3>
        <p className="mt-1 text-[11px] text-muted-foreground">
          {t('workflows.inspector.selectNodeHint')}
        </p>
      </div>
      <div className="flex flex-1 flex-col items-center justify-center px-6 text-center">
        <span className="mb-3 flex size-10 items-center justify-center rounded-xl bg-muted">
          <SlidersHorizontal className="size-5 text-muted-foreground" />
        </span>
        <p className="text-xs font-medium">{t('workflows.inspector.noSelection')}</p>
        <p className="mt-1 text-[11px] leading-5 text-muted-foreground">
          {t('workflows.inspector.noSelectionHint')}
        </p>
      </div>
    </aside>
  )
}

/** Edits one node in place, with the fields its kind actually supports. */
function WorkflowNodeInspector({
  node,
  onUpdate,
  onAddLoopExit,
  onClose,
  nodes,
  edges,
  globalVariables,
}: WorkflowInspectorProps & { node: WorkflowCanvasNode }) {
  // The catalog is a graph question asked against the current selection: what the selected
  // node may reference depends on what reaches it, which edges decide. It is derived rather
  // than stored so the answer cannot drift from the graph it describes.
  const catalog = useMemo(() => {
    const normalized = normalizeWorkflowGlobalVariables(globalVariables)
    return decorateWorkflowVariableCatalog(
      deriveWorkflowVariableCatalog(nodes, edges, node.id, normalized),
      nodes,
      normalized,
    )
  }, [nodes, edges, node.id, globalVariables])
  return (
    <aside
      data-workflow-inspector=""
      className="flex h-full min-h-0 w-full min-w-0 flex-1 flex-col overflow-hidden border-l border-border bg-background"
    >
      <NodeInspectorHeader node={node} onUpdate={onUpdate} onClose={onClose} />
      <div className="min-h-0 min-w-0 flex-1 space-y-4 overflow-x-hidden overflow-y-auto p-4">
        <NodeDescriptionField data={node.data} onChange={onUpdate} />
        <NodeKindFields
          nodeId={node.id}
          data={node.data}
          onChange={onUpdate}
          catalog={catalog}
          graphNodes={nodes}
          edges={edges}
          globalVariables={globalVariables}
          {...(onAddLoopExit === undefined ? {} : { onAddLoopExit })}
        />
      </div>
    </aside>
  )
}

/** The kind-specific part of the rail. */
function NodeKindFields({
  nodeId,
  data,
  onChange,
  catalog,
  graphNodes,
  edges,
  globalVariables,
  onAddLoopExit,
}: WorkflowKindFieldsProps & {
  edges: readonly WorkflowCanvasEdge[]
  onAddLoopExit?: (loopId: string) => void
}) {
  if (data.kind === 'start') {
    return <StartNodeFields data={data} onChange={onChange} />
  }
  if (data.kind === 'agent') {
    return <AgentNodeFields data={data} onChange={onChange} catalog={catalog} />
  }
  if (data.kind === 'condition') {
    return <ConditionNodeFields data={data} onChange={onChange} catalog={catalog} />
  }
  if (data.kind === 'aggregator') {
    return <AggregatorNodeFields data={data} onChange={onChange} catalog={catalog} />
  }
  if (data.kind === 'tool') {
    return <ToolNodeFields data={data} onChange={onChange} />
  }
  if (data.kind === 'junction') {
    return <JunctionNodeFields data={data} onChange={onChange} />
  }
  if (data.kind === 'human') {
    return <HumanNodeFields data={data} onChange={onChange} />
  }
  if (data.kind === 'loop') {
    return (
      <LoopNodeFields
        data={data}
        onChange={onChange}
        nodeId={nodeId}
        graphNodes={graphNodes}
        edges={edges}
        globalVariables={globalVariables}
        {...(onAddLoopExit === undefined ? {} : { onAddExit: () => onAddLoopExit(nodeId) })}
      />
    )
  }
  if (data.kind === 'iteration') {
    return (
      <IterationNodeFields
        nodeId={nodeId}
        data={data}
        onChange={onChange}
        catalog={catalog}
        graphNodes={graphNodes}
        globalVariables={globalVariables}
      />
    )
  }
  if (data.kind === 'loopExit') {
    return <LoopExitNodeFields />
  }
  if (data.kind === 'subflow') {
    return <SubflowNodeFields />
  }
  if (data.kind === 'output') {
    return <OutputNodeFields data={data} onChange={onChange} catalog={catalog} />
  }
  return <UnconfigurableNotice />
}

/** Identity block: kind icon, editable title and the collapse control. */
function NodeInspectorHeader({
  node,
  onUpdate,
  onClose,
}: {
  node: WorkflowCanvasNode
  onUpdate: (data: WorkflowNodeData) => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState(node.data.title)
  const metadata = workflowNodeMetadata(node.data.kind)
  const Icon = metadata.icon
  const kindLabel = translate(workflowNodeTypeDefinition(node.data.kind).labelKey)

  /** Commits a non-empty title and restores the previous one for a blank edit. */
  function commitTitle(): void {
    const title = draft.trim() || node.data.title
    setDraft(title)
    setEditing(false)
    if (title !== node.data.title) {
      onUpdate({ ...node.data, title })
    }
  }

  return (
    <header className="min-w-0 border-b border-border px-4 py-3">
      <div className="flex min-w-0 items-center gap-2.5">
        <span
          title={kindLabel}
          className={cn(
            'flex size-8 shrink-0 items-center justify-center rounded-lg',
            metadata.tone,
          )}
        >
          <Icon className="size-4" strokeWidth={1.9} />
        </span>
        {editing ? (
          <Input
            autoFocus
            aria-label={t('workflows.inspector.name')}
            className="h-9 min-w-0 flex-1 px-2 font-sans text-base font-bold"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            onBlur={commitTitle}
            onKeyDown={(event) => {
              if (event.key === 'Enter') {
                event.currentTarget.blur()
              } else if (event.key === 'Escape') {
                setDraft(node.data.title)
                setEditing(false)
              }
            }}
          />
        ) : (
          <h3 className="min-w-0 flex-1 font-sans text-base font-bold">
            <button
              type="button"
              title={t('workflows.inspector.editTitle')}
              className="block w-full truncate rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
              onDoubleClick={() => {
                setDraft(node.data.title)
                setEditing(true)
              }}
            >
              {node.data.title}
            </button>
          </h3>
        )}
        <Button
          variant="ghost"
          size="icon-sm"
          className="shrink-0"
          aria-label={t('workflows.inspector.close')}
          onClick={onClose}
        >
          <PanelRightClose className="size-4" />
        </Button>
      </div>
    </header>
  )
}
