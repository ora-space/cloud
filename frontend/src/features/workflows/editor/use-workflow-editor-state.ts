import { useCallback, useRef, useState } from 'react'
import type { Connection, EdgeChange, NodeChange, Viewport, XYPosition } from '@xyflow/react'
import type { TFunction } from 'i18next'
import { useTranslation } from 'react-i18next'
import type { Workflow as CloudWorkflow } from '@/api/generated.schemas'
import { useUpdateWorkflowGraph } from '@/features/workflows/api'
import { isAuthoredNodeChange } from '@/features/workflows/runtime/layout'
import type { WorkflowIterationInsertion } from '@/features/workflows/runtime/iteration-graph'
import type {
  WorkflowGlobalVariable,
  WorkflowLaunchField,
  WorkflowNodeData,
  WorkflowNodeKind,
  WorkflowViewport,
} from '@/features/workflows/runtime/types'
import {
  useWorkflowDraft,
  type WorkflowDraft,
} from '@/features/workflows/editor/use-workflow-draft'
import { useWorkflowHistory } from '@/features/workflows/editor/use-workflow-history'
import type {
  WorkflowHistoryEvent,
  WorkflowHistoryMeta,
  WorkflowHistorySnapshot,
} from '@/features/workflows/editor/workflow-history'
import {
  useWorkflowDraftAutosave,
  type WorkflowDraftSaveResult,
  type WorkflowDraftSaveStatus,
} from '@/features/workflows/editor/use-workflow-draft-autosave'
import { useWorkflowTranslator } from '@/features/workflows/editor/use-workflow-translator'
import type {
  WorkflowCanvasEdge,
  WorkflowCanvasNode,
} from '@/features/workflows/editor/canvas-types'

/** What the canvas calls when the graph, the selection or the viewport changes. */
export interface WorkflowEditorHandlers {
  onNodesChange: (changes: NodeChange<WorkflowCanvasNode>[]) => void
  onEdgesChange: (changes: EdgeChange<WorkflowCanvasEdge>[]) => void
  onConnect: (connection: Connection) => void
  onAddNode: (kind: WorkflowNodeKind, position: XYPosition) => void
  /** Adds a loopExit into the selected loop's body. */
  onAddLoopExit: (loopId: string) => void
  onOrganize: () => void
  onNodeDataUpdate: (data: WorkflowNodeData) => void
  /** Adds one region member through an iteration insertion seam. */
  onIterationInsert: (insertion: WorkflowIterationInsertion, kind: WorkflowNodeKind) => void
  /** Folds or unfolds one iteration frame; members stay in the graph. */
  onToggleIterationCollapsed: (iterationId: string) => void
  /** Replaces the graph nodes after an iteration drag correction. */
  onIterationCorrection: (nodes: WorkflowCanvasNode[]) => void
  onViewportChange: (viewport: Viewport) => void
  /** Opens a node-drag history transaction; the canvas fires it at drag start. */
  onHistoryNodeDragStart: () => void
  /** Closes the node-drag transaction; the canvas fires it after any correction. */
  onHistoryNodeDragStop: () => void
}

/** Everything the editor mounts against the session history. */
export interface WorkflowHistoryControls {
  canUndo: boolean
  canRedo: boolean
  past: ReturnType<typeof useWorkflowHistory>['past']
  future: ReturnType<typeof useWorkflowHistory>['future']
  currentEvent: ReturnType<typeof useWorkflowHistory>['currentEvent']
  currentMeta: ReturnType<typeof useWorkflowHistory>['currentMeta']
  undo: () => void
  redo: () => void
  jump: (direction: 'past' | 'future', steps: number) => void
  clear: () => void
}

/**
 * The graph being edited, the autosave writing it, and every callback the canvas needs.
 *
 * `history` exposes the session's undo/redo stacks and actions to the toolbar;
 * the canvas gestures (drag, resize, delete) are recorded inside the handlers.
 */
export interface WorkflowEditorState extends WorkflowEditorHandlers {
  nodes: WorkflowCanvasNode[]
  edges: WorkflowCanvasEdge[]
  /** Workflow-wide declarations the inspector and kind panels resolve variables against. */
  globalVariables: readonly WorkflowGlobalVariable[]
  /** Which platform launch fields the `@` form asks for, as the launch-fields dialog reads them. */
  launchFields: readonly WorkflowLaunchField[]
  /**
   * Names the graph's Start node declares, which the launch-fields dialog reads as the author
   * owning those keys. Empty when the graph has no Start node.
   */
  startVariableNames: readonly string[]
  /** Viewport the canvas should open at. */
  viewport: WorkflowViewport
  /** The single selected node, or `null` when the selection is empty or multiple. */
  selectedNode: WorkflowCanvasNode | null
  /** Autosave state, driving the indicator and the save button. */
  status: WorkflowDraftSaveStatus
  /** Writes the draft now instead of waiting out the debounce. */
  saveNow: () => void
  /**
   * Writes until the draft is clean, reporting whether it actually persisted.
   * Publish calls this first so a released version is never stale.
   */
  flushDraft: () => Promise<boolean>
  /** The document version the server last confirmed; its value guards a restore. */
  committedVersion: number
  /** Replaces the draft with a fresh workflow, e.g. after a version restore. */
  resetDraft: (workflow: CloudWorkflow) => void
  /** Serializes the live draft, unsaved edits included, into the graph document. */
  draftGraph: () => Record<string, unknown>
  /** Replaces the workflow-wide declarations as one history step. */
  onGlobalVariablesSave: (variables: WorkflowGlobalVariable[]) => void
  /** Replaces the launch-field declaration as one history step. */
  onLaunchFieldsSave: (fields: WorkflowLaunchField[]) => void
  /** Undo and redo controls for the session. */
  history: WorkflowHistoryControls
}

/**
 * Wires the draft, the history, the autosave and the canvas callbacks together;
 * the canvas writes into the draft and a debounced save serializes the whole.
 *
 * @param tenantId - Tenant the workflow belongs to; scopes every write.
 * @param workflow - The workflow being edited.
 */
export function useWorkflowEditorState(
  tenantId: string,
  workflow: CloudWorkflow,
): WorkflowEditorState {
  const { t } = useTranslation()
  const translate = useWorkflowTranslator()
  const draft = useWorkflowDraft(workflow, translate)
  const saveGraph = useUpdateWorkflowGraph(tenantId)
  const { snapshot, nodes, edges, selectedNode, viewport, globalVariables, launchFields } = draft

  // The viewport rides along with every write but never marks the draft dirty,
  // so panning and zooming cannot bump the workflow version on their own.
  const viewportRef = useRef<WorkflowViewport>(viewport)
  // Advanced only by this editor's own successful writes; `committedVersion`
  // mirrors it for the toolbar. Never synced from the loaded workflow, so a
  // version that moved underneath is another editor's work, not ours to adopt.
  const versionRef = useRef(workflow.version)
  const [committedVersion, setCommittedVersion] = useState(workflow.version)

  const save = useCallback(async (): Promise<WorkflowDraftSaveResult> => {
    try {
      const updated = await saveGraph.mutateAsync({
        id: workflow.id,
        name: workflow.name,
        graph: snapshot(viewportRef.current),
        version: versionRef.current,
      })
      versionRef.current = updated.version
      setCommittedVersion(updated.version)
      return 'saved'
    } catch {
      // A rejected write is almost always the version guard, which retrying
      // cannot resolve: this draft is behind, and only a reload can catch up.
      return 'failed'
    }
  }, [saveGraph, snapshot, workflow.id, workflow.name])

  const { status, markDirty, flush, cancel } = useWorkflowDraftAutosave({ enabled: true, save })
  // A history restore writes the chosen snapshot over the draft and keeps the
  // restored content eligible for autosave, exactly like the edit it replaces.
  const history = useWorkflowHistory(
    useCallback(
      (restored: WorkflowHistorySnapshot): void => {
        draft.restoreContent(restored)
        markDirty()
      },
      [draft, markDirty],
    ),
  )

  const globalVariablesSaver = useWorkflowGlobalVariablesRecorder(draft, markDirty, history, t)
  const launchFieldsSaver = useWorkflowLaunchFieldsRecorder(draft, markDirty, history, t)

  // The names the graph's Start node declares. The first Start node is the one that counts, which is
  // the same rule the `@` projection reads its variables by: a graph carrying two of them cannot have
  // one supply the prompt and the other the variables, so the dialog must not treat the second node's
  // names as declared either.
  const startNode = nodes.find((node) => node.data.kind === 'start')
  const startVariableNames = (startNode?.data.inputVariables ?? []).map((variable) => variable.name)
  const onViewportChange = useCallback((next: Viewport) => {
    viewportRef.current = next
  }, [])
  const saveNow = useCallback(() => {
    void flush()
  }, [flush])
  // The export dialog serializes whatever is on the canvas right now, unsaved
  // edits included, so what the download previews is what the file will hold.
  const draftGraph = useCallback(() => snapshot(viewportRef.current), [snapshot])

  // An external replace (a version restore) reseeds the draft and adopts the new
  // version as the guard for later writes and a later restore.
  const resetDraft = useCallback(
    (next: CloudWorkflow) => {
      draft.reset(next)
      versionRef.current = next.version
      setCommittedVersion(next.version)
      cancel()
    },
    [cancel, draft],
  )

  return {
    ...useWorkflowDraftHandlers(draft, markDirty, history, t),
    ...globalVariablesSaver,
    ...launchFieldsSaver,
    nodes,
    edges,
    globalVariables,
    launchFields,
    startVariableNames,
    viewport,
    selectedNode,
    status,
    saveNow,
    flushDraft: flush,
    committedVersion,
    resetDraft,
    draftGraph,
    onViewportChange,
    history: workflowHistoryControls(draft, history),
  }
}

/**
 * The published surface the toolbar mounts against. The undo/redo/jump actions
 * capture the draft at the moment of use, so a stale render can never restore
 * content that has already moved on.
 *
 * @param draft - The draft the actions capture from.
 * @param history - The session recorder backing the buttons.
 */
function workflowHistoryControls(
  draft: WorkflowDraft,
  history: ReturnType<typeof useWorkflowHistory>,
): WorkflowHistoryControls {
  const { canUndo, canRedo, past, future, currentEvent, currentMeta } = history
  return {
    canUndo,
    canRedo,
    past,
    future,
    currentEvent,
    currentMeta,
    clear: history.clear,
    undo: () => history.undo(draft.captureContent()),
    redo: () => history.redo(draft.captureContent()),
    jump: (direction, steps) => history.jump(draft.captureContent(), direction, steps),
  }
}

/** Routes a gesture's content through the session history. */
interface WorkflowHistoryBridge {
  /** Commits one completed discrete edit, coalescing under its group key. */
  record: (
    before: WorkflowHistorySnapshot,
    after: WorkflowHistorySnapshot,
    event: WorkflowHistoryEvent,
    changes?: { meta?: WorkflowHistoryMeta; group?: string },
  ) => void
  /** Opens a transaction for a multi-frame gesture (drag, resize, delete cascade). */
  beginTransaction: (
    before: WorkflowHistorySnapshot,
    event: WorkflowHistoryEvent,
    meta?: WorkflowHistoryMeta,
  ) => void
  /** Closes the open transaction as one history step. */
  commitTransaction: (after: WorkflowHistorySnapshot) => void
}

/**
 * Adapts the draft's mutators into the callbacks the canvas takes.
 *
 * Each one captures the authored content before and after its mutation, so the
 * history records a discrete step with context the panel can label. Dragged
 * nodes are the exception: the canvas opens a transaction at drag start and
 * closes it at drag stop, and the frame-resize stream does the same around its
 * own transaction, so one gesture undoes as one step.
 */
function useWorkflowDraftHandlers(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
): Omit<WorkflowEditorHandlers, 'onViewportChange'> {
  const nodeChanges = useWorkflowNodeChangesRecorder(draft, markDirty, history, t)
  const edgeChanges = useWorkflowEdgeChangesRecorder(draft, markDirty, history, t)
  const actions = useWorkflowActionRecorder(draft, markDirty, history, t)
  const nodeEdit = useWorkflowNodeEditRecorder(draft, markDirty, history, t)
  const beginNodeDrag = useCallback(
    () => history.beginTransaction(draft.captureContent(), 'node.move'),
    [draft, history],
  )
  const endNodeDrag = useCallback(
    () => history.commitTransaction(draft.captureContent()),
    [draft, history],
  )
  return {
    ...nodeChanges,
    ...edgeChanges,
    ...actions,
    ...nodeEdit,
    onHistoryNodeDragStart: beginNodeDrag,
    onHistoryNodeDragStop: endNodeDrag,
  }
}

/**
 * Records node-change batches: the frame-resize stream undo/redo as one step,
 * and a removal batch as one `node.delete` step. Positions only mark the draft
 * dirty, so a plain drag records nothing here.
 */
function useWorkflowNodeChangesRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const { captureContent, applyNodeChanges } = draft
  const { beginTransaction, commitTransaction, record } = history
  // Which iteration frame is mid-resize; the closing `resizing: false` frame
  // of the same id commits that transaction.
  const activeResizeRef = useRef<string | null>(null)

  const onNodesChange = useCallback(
    (changes: NodeChange<WorkflowCanvasNode>[]) => {
      const before = captureContent()
      const resizeBegin = changes.find(
        (change): change is Extract<NodeChange<WorkflowCanvasNode>, { type: 'dimensions' }> =>
          change.type === 'dimensions' && change.resizing === true,
      )
      if (resizeBegin !== undefined && activeResizeRef.current === null) {
        activeResizeRef.current = resizeBegin.id
        const target = before.nodes.find((node) => node.id === resizeBegin.id)
        beginTransaction(
          before,
          'iteration.resize',
          historyMetaOf(
            [resizeBegin.id],
            nodeSubjectOf(t, target),
            target === undefined ? undefined : target.data.kind,
          ),
        )
      }
      const frameResized = changes.some(
        (change) => change.type === 'dimensions' && change.resizing === true,
      )
      if (isAuthoredNodeChange(changes) || frameResized) {
        markDirty()
      }
      applyNodeChanges(changes)
      const after = captureContent()
      const resizeEnd = changes.find(
        (change): change is Extract<NodeChange<WorkflowCanvasNode>, { type: 'dimensions' }> =>
          change.type === 'dimensions' && change.resizing === false,
      )
      if (resizeEnd !== undefined && activeResizeRef.current === resizeEnd.id) {
        activeResizeRef.current = null
        commitTransaction(after)
      }
      const removed = changes.filter(
        (change): change is Extract<NodeChange<WorkflowCanvasNode>, { type: 'remove' }> =>
          change.type === 'remove',
      )
      if (removed.length > 0) {
        const nodeIds = removed.map((change) => change.id)
        const subject = before.nodes
          .filter((node) => nodeIds.includes(node.id))
          .map((node) => nodeSubjectOf(t, node))
          .filter((value): value is string => value !== undefined)
          .join('、')
        record(before, after, 'node.delete', { meta: { nodeIds, subject } })
      }
    },
    [applyNodeChanges, beginTransaction, captureContent, commitTransaction, markDirty, record, t],
  )

  return { onNodesChange }
}

/** Records edge removals through the session history; connects record elsewhere. */
function useWorkflowEdgeChangesRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const { captureContent, applyEdgeChanges } = draft
  const { record } = history

  const onEdgesChange = useCallback(
    (changes: EdgeChange<WorkflowCanvasEdge>[]) => {
      const before = captureContent()
      if (isAuthoredNodeChange(changes)) {
        markDirty()
      }
      applyEdgeChanges(changes)
      const after = captureContent()
      const removed = changes.filter(
        (change): change is Extract<EdgeChange<WorkflowCanvasEdge>, { type: 'remove' }> =>
          change.type === 'remove',
      )
      if (removed.length > 0) {
        const edgeIds = removed.map((change) => change.id)
        const subjects = removed.flatMap((change) => {
          const edge = before.edges.find((candidate) => candidate.id === change.id)
          if (edge === undefined) {
            return []
          }
          return [edgeSubjectOf(t, before, edge.source, edge.target)]
        })
        record(before, after, 'edge.delete', {
          meta: { edgeIds, subject: [...new Set(subjects)].join('、') },
        })
      }
    },
    [applyEdgeChanges, captureContent, markDirty, record, t],
  )

  return { onEdgesChange }
}

/**
 * Records the discrete one-shot edits: adding, connecting, folding, editing and
 * arranging. Field edits coalesce per changed key so typing one field is one
 * undo step, matching the draft's own per-field grouping.
 */
function useWorkflowActionRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const {
    captureContent,
    addNode,
    addLoopExit,
    addIterationMember,
    toggleIterationCollapsed,
    applyCorrection,
    connect,
    organize,
  } = draft
  const { record } = history

  const recordNodeAddition = useCallback(
    (kind: WorkflowNodeKind, mutate: () => void) => {
      const before = captureContent()
      markDirty()
      mutate()
      record(before, captureContent(), 'node.add', {
        meta: { nodeKind: kind, subject: t(`workflows.node.${kind}.label`) },
      })
    },
    [captureContent, markDirty, record, t],
  )

  const onAddNode = useCallback(
    (kind: WorkflowNodeKind, position: XYPosition) => {
      recordNodeAddition(kind, () => addNode(kind, position))
    },
    [addNode, recordNodeAddition],
  )

  const onAddLoopExit = useCallback(
    (loopId: string) => {
      recordNodeAddition('loopExit', () => addLoopExit(loopId))
    },
    [addLoopExit, recordNodeAddition],
  )

  const onIterationInsert = useCallback(
    (insertion: WorkflowIterationInsertion, kind: WorkflowNodeKind) => {
      recordNodeAddition(kind, () => addIterationMember(insertion, kind))
    },
    [addIterationMember, recordNodeAddition],
  )

  const onToggleIterationCollapsed = useCallback(
    (iterationId: string) => {
      const before = captureContent()
      markDirty()
      toggleIterationCollapsed(iterationId)
      const target = before.nodes.find((node) => node.id === iterationId)
      record(before, captureContent(), 'node.edit', {
        meta: historyMetaOf([iterationId], nodeSubjectOf(t, target), 'iteration'),
      })
    },
    [captureContent, markDirty, record, t, toggleIterationCollapsed],
  )

  // A drag correction runs at canvas drag stop, inside the move transaction, so
  // recording it separately would split one gesture into two steps.
  const onIterationCorrection = useCallback(
    (nodes: WorkflowCanvasNode[]) => {
      markDirty()
      applyCorrection(nodes)
    },
    [applyCorrection, markDirty],
  )

  const onConnect = useCallback(
    (connection: Connection) => {
      const before = captureContent()
      markDirty()
      connect(connection)
      const after = captureContent()
      const edge = after.edges.find(
        (candidate) =>
          candidate.source === connection.source && candidate.target === connection.target,
      )
      record(before, after, 'edge.connect', {
        meta: {
          edgeIds: edge === undefined ? [] : [edge.id],
          subject: edgeSubjectOf(t, before, connection.source, connection.target),
        },
      })
    },
    [captureContent, connect, markDirty, record, t],
  )

  const onOrganize = useCallback(() => {
    const before = captureContent()
    markDirty()
    organize()
    record(before, captureContent(), 'layout.organize')
  }, [captureContent, markDirty, organize, record])

  return {
    onAddNode,
    onAddLoopExit,
    onIterationInsert,
    onToggleIterationCollapsed,
    onIterationCorrection,
    onConnect,
    onOrganize,
  }
}

/**
 * Records one field edit against the selected node, coalescing per changed key
 * so typing one focused field undoes as a single step.
 */
function useWorkflowNodeEditRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const { captureContent, updateNodeData, selectedNode } = draft
  const { record } = history

  const onNodeDataUpdate = useCallback(
    (data: WorkflowNodeData) => {
      if (selectedNode === null) {
        return
      }
      const before = captureContent()
      const nodeBefore = before.nodes.find((node) => node.id === selectedNode.id)
      markDirty()
      updateNodeData(selectedNode.id, data)
      const after = captureContent()
      const changedKeys = changedFieldKeys(nodeBefore, data)
      const group =
        changedKeys.length === 0 ? selectedNode.id : `${selectedNode.id}:${changedKeys.join(',')}`
      record(before, after, 'node.edit', {
        meta: historyMetaOf(
          [selectedNode.id],
          nodeSubjectOf(t, nodeBefore) ??
            nodeSubjectOf(
              t,
              after.nodes.find((n) => n.id === selectedNode.id),
            ),
          data.kind,
        ),
        group,
      })
    },
    [captureContent, markDirty, record, selectedNode, t, updateNodeData],
  )

  return { onNodeDataUpdate }
}

/**
 * Records a replacement of the workflow-wide declarations as one discrete
 * `workflow.variables` step — the dialog saves the whole list atomically, so
 * there is nothing finer-grained to coalesce.
 */
function useWorkflowGlobalVariablesRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const { captureContent, replaceGlobalVariables } = draft
  const { record } = history

  const onGlobalVariablesSave = useCallback(
    (variables: WorkflowGlobalVariable[]) => {
      const before = captureContent()
      markDirty()
      replaceGlobalVariables(variables)
      record(before, captureContent(), 'workflow.variables', {
        meta: { subject: t('workflows.globalVariables.title') },
      })
    },
    [captureContent, markDirty, record, replaceGlobalVariables, t],
  )

  return { onGlobalVariablesSave }
}

/**
 * Records a replacement of the launch-field declaration as one discrete
 * `workflow.launchFields` step, for the same reason the globals recorder does:
 * the dialog commits the whole declaration at once.
 */
function useWorkflowLaunchFieldsRecorder(
  draft: WorkflowDraft,
  markDirty: () => void,
  history: WorkflowHistoryBridge,
  t: TFunction,
) {
  const { captureContent, replaceLaunchFields } = draft
  const { record } = history

  const onLaunchFieldsSave = useCallback(
    (fields: WorkflowLaunchField[]) => {
      const before = captureContent()
      markDirty()
      replaceLaunchFields(fields)
      record(before, captureContent(), 'workflow.launchFields', {
        meta: { subject: t('workflows.launchFields.title') },
      })
    },
    [captureContent, markDirty, record, replaceLaunchFields, t],
  )

  return { onLaunchFieldsSave }
}

/** Names one canvas node for a history step without exposing internal ids. */
function nodeSubjectOf(t: TFunction, node: WorkflowCanvasNode | undefined): string | undefined {
  if (node === undefined) {
    return undefined
  }
  return node.data.title === '' ? t('workflows.history.unknownNode') : node.data.title
}

/** Names an edge through its endpoints so a connection step is actionable. */
function edgeSubjectOf(
  t: TFunction,
  snapshot: WorkflowHistorySnapshot,
  sourceId: string,
  targetId: string,
): string {
  const sourceName =
    nodeSubjectOf(
      t,
      snapshot.nodes.find((node) => node.id === sourceId),
    ) ?? t('workflows.history.unknownNode')
  const targetName =
    nodeSubjectOf(
      t,
      snapshot.nodes.find((node) => node.id === targetId),
    ) ?? t('workflows.history.unknownNode')
  return `${sourceName} → ${targetName}`
}

/**
 * Builds a step's context, leaving an unresolved string absent rather than
 * `undefined`, which `exactOptionalPropertyTypes` would reject at the seam.
 */
function historyMetaOf(
  nodeIds: string[],
  subject?: string,
  nodeKind?: string,
): WorkflowHistoryMeta {
  const meta: WorkflowHistoryMeta = {}
  if (nodeIds.length > 0) {
    meta.nodeIds = nodeIds
  }
  if (subject !== undefined) {
    meta.subject = subject
  }
  if (nodeKind !== undefined) {
    meta.nodeKind = nodeKind
  }
  return meta
}

/** The data keys whose value changed between the pre-edit and new node data. */
function changedFieldKeys(
  beforeNode: WorkflowCanvasNode | undefined,
  data: WorkflowNodeData,
): string[] {
  if (beforeNode === undefined) {
    return []
  }
  return Object.keys(data).filter(
    (key) => beforeNode.data[key as keyof WorkflowNodeData] !== data[key as keyof WorkflowNodeData],
  )
}
