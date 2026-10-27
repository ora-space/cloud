import { useCallback, useEffect, useRef, useState, type ReactNode, type RefObject } from 'react'
import { useTranslation } from 'react-i18next'
import { Save } from 'lucide-react'
import { useBlocker } from 'react-router-dom'
import type { Workflow as CloudWorkflow } from '@/api/generated.schemas'
import { Badge } from '@/components/ui/badge'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  ResizableHandle,
  ResizablePanel,
  ResizablePanelGroup,
  type ResizablePanelHandle,
} from '@/components/ui/resizable'
import type { WorkflowDraftSaveStatus } from '@/features/workflows/editor/use-workflow-draft-autosave'
import {
  useWorkflowEditorState,
  type WorkflowEditorState,
} from '@/features/workflows/editor/use-workflow-editor-state'
import { WorkflowCanvas } from '@/features/workflows/editor/workflow-canvas'
import { WorkflowDraftSaveStatusLabel } from '@/features/workflows/editor/workflow-draft-save-status'
import { WorkflowHistoryTools } from '@/features/workflows/editor/workflow-history-controls'
import { WorkflowInspector } from '@/features/workflows/editor/workflow-inspector'
import { WorkflowLaunchTools } from '@/features/workflows/editor/workflow-launch-tools'
import { WorkflowRunTools } from '@/features/workflows/editor/workflow-run-tools'
import { WorkflowTransferTools } from '@/features/workflows/editor/workflow-transfer-tools'
import { WorkflowVariablesTools } from '@/features/workflows/editor/workflow-variables-tools'
import { WorkflowVersionTools } from '@/features/workflows/editor/workflow-version-tools'

/** Width the configuration rail opens at, and the range it may be dragged within. */
const INSPECTOR_DEFAULT_WIDTH = '320px'
const INSPECTOR_MIN_WIDTH = '260px'
const INSPECTOR_MAX_WIDTH = '560px'

/** A region deletion waiting on the author's confirmation. */
interface PendingIterationDeletion {
  /** Members that would vanish with the container. */
  memberCount: number
  /** Settles React Flow's pending before-delete hook with the author's choice. */
  resolve: (confirmed: boolean) => void
}

/** Props for {@link WorkflowEditor}. */
export interface WorkflowEditorProps {
  /** Tenant the workflow belongs to; scopes every write. */
  tenantId: string
  /** The workflow being edited. Remount on a different id to switch drafts. */
  workflow: CloudWorkflow
  /** Receives the created workflow after an import; the page navigates to it. */
  onImported?: (workflowId: string) => void
  /** Enable route-exit protection when this editor is mounted by the application page. */
  guardNavigation?: boolean
}

/**
 * The workflow canvas and its configuration rail, wired to the API by autosave.
 *
 * The graph state lives in {@link useWorkflowEditorState}; this component owns
 * only the layout and the rail's collapsed state, which is a property of the
 * viewport rather than of the document.
 *
 * @param props - The tenant to write to and the workflow to open.
 */
export function WorkflowEditor(props: WorkflowEditorProps) {
  const { tenantId, workflow, onImported, guardNavigation } = props
  const state = useWorkflowEditorState(tenantId, workflow)
  const inspectorRef = useRef<ResizablePanelHandle | null>(null)
  const iterationDeletion = useIterationDeletionConfirmation()

  useInspectorAutoExpand(inspectorRef, state.selectedNode?.id ?? null)

  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      {guardNavigation && <DraftNavigationGuard draft={state} />}
      <EditorToolbar
        nodeCount={state.nodes.length}
        edgeCount={state.edges.length}
        version={state.committedVersion}
        status={state.status}
        onSave={state.saveNow}
        historyTools={<WorkflowHistoryTools history={state.history} />}
        variablesTools={
          <WorkflowVariablesTools
            variables={state.globalVariables}
            onSave={state.onGlobalVariablesSave}
          />
        }
        launchTools={
          <WorkflowLaunchTools
            tenantId={tenantId}
            workflowId={workflow.id}
            fields={state.launchFields}
            startVariableNames={state.startVariableNames}
            onSave={state.onLaunchFieldsSave}
          />
        }
        versionTools={
          <WorkflowVersionTools
            tenantId={tenantId}
            workflowId={workflow.id}
            flushDraft={state.flushDraft}
          />
        }
        transferTools={
          <WorkflowTransferTools
            tenantId={tenantId}
            workflow={workflow}
            draftGraph={state.draftGraph}
            onImported={(workflowId) => onImported?.(workflowId)}
          />
        }
        runTools={<WorkflowRunTools tenantId={tenantId} workflowId={workflow.id} />}
      />
      <ResizablePanelGroup orientation="horizontal" className="min-h-0 flex-1">
        <ResizablePanel>
          <WorkflowCanvas
            nodes={state.nodes}
            edges={state.edges}
            viewport={state.viewport}
            onNodesChange={state.onNodesChange}
            onEdgesChange={state.onEdgesChange}
            onConnect={state.onConnect}
            onAddNode={state.onAddNode}
            onOrganize={state.onOrganize}
            onViewportChange={state.onViewportChange}
            onIterationInsert={state.onIterationInsert}
            onToggleIterationCollapsed={state.onToggleIterationCollapsed}
            onIterationCorrection={state.onIterationCorrection}
            onConfirmIterationDeletion={iterationDeletion.confirm}
            onHistoryNodeDragStart={state.onHistoryNodeDragStart}
            onHistoryNodeDragStop={state.onHistoryNodeDragStop}
          />
        </ResizablePanel>
        <ResizableHandle withHandle />
        <ResizablePanel
          panelRef={inspectorRef}
          collapsible
          collapsedSize="0px"
          defaultSize={INSPECTOR_DEFAULT_WIDTH}
          minSize={INSPECTOR_MIN_WIDTH}
          maxSize={INSPECTOR_MAX_WIDTH}
        >
          <WorkflowInspector
            node={state.selectedNode}
            nodes={state.nodes}
            edges={state.edges}
            globalVariables={state.globalVariables}
            onUpdate={state.onNodeDataUpdate}
            onAddLoopExit={state.onAddLoopExit}
            onClose={() => inspectorRef.current?.collapse()}
          />
        </ResizablePanel>
      </ResizablePanelGroup>
      {iterationDeletion.pending !== null && (
        <IterationDeletionDialog
          pending={iterationDeletion.pending}
          onSettle={iterationDeletion.settle}
        />
      )}
    </div>
  )
}

/** Keeps the page editor mounted until its draft reaches the query cache or a failed save is shown. */
function DraftNavigationGuard({
  draft,
}: {
  draft: Pick<WorkflowEditorState, 'status' | 'flushDraft' | 'discardDraft'>
}) {
  const { status, flushDraft, discardDraft } = draft
  const { t } = useTranslation()
  const blocker = useBlocker(status !== 'clean')
  const settlingNavigation = useRef(false)
  const [saveFailed, setSaveFailed] = useState(false)

  useEffect(() => {
    if (blocker.state !== 'blocked' || settlingNavigation.current) return
    if (status === 'error') return
    settlingNavigation.current = true
    // Every in-app exit first waits for the write. A failed write needs a choice: silently
    // resetting the blocker would trap the member on every later navigation as well.
    void (async () => {
      try {
        if (await flushDraft()) blocker.proceed()
        else setSaveFailed(true)
      } catch {
        setSaveFailed(true)
      } finally {
        settlingNavigation.current = false
      }
    })()
  }, [blocker, flushDraft, status])

  return (
    <AlertDialog
      open={blocker.state === 'blocked' && (status === 'error' || saveFailed)}
      onOpenChange={(open) => {
        if (!open) stay()
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{t('workflows.draftNavigation.title')}</AlertDialogTitle>
          <AlertDialogDescription>
            {t('workflows.draftNavigation.description')}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{t('workflows.draftNavigation.stay')}</AlertDialogCancel>
          <AlertDialogAction variant="destructive" onClick={leave}>
            {t('workflows.draftNavigation.leave')}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )

  function stay(): void {
    setSaveFailed(false)
    if (blocker.state === 'blocked') blocker.reset()
  }

  function leave(): void {
    if (blocker.state !== 'blocked') return
    discardDraft()
    setSaveFailed(false)
    blocker.proceed()
  }
}

/**
 * Turns the region-deletion question into something the canvas can wait on.
 *
 * React Flow's before-delete hook is synchronous, so the question is answered through a promise the
 * dialog settles: the canvas holds the deletion until the author has said yes or no.
 */
function useIterationDeletionConfirmation(): {
  pending: PendingIterationDeletion | null
  confirm: (memberCount: number) => Promise<boolean>
  settle: (confirmed: boolean) => void
} {
  const [pending, setPending] = useState<PendingIterationDeletion | null>(null)
  const confirm = useCallback(
    (memberCount: number) =>
      new Promise<boolean>((resolve) => setPending({ memberCount, resolve })),
    [],
  )

  function settle(confirmed: boolean): void {
    pending?.resolve(confirmed)
    setPending(null)
  }

  return { pending, confirm, settle }
}

/**
 * Expands the configuration rail when a fresh node is selected, leaving it
 * closed when the same node is re-selected.
 *
 * Collapsing the rail is a deliberate gesture, so only a change of selection
 * brings it back; re-rendering with the same node must leave it closed.
 */
function useInspectorAutoExpand(
  inspectorRef: RefObject<ResizablePanelHandle | null>,
  selectedNodeId: string | null,
): void {
  useEffect(() => {
    const panel = inspectorRef.current
    if (selectedNodeId === null || panel === null || !panel.isCollapsed()) {
      return
    }
    panel.expand()
  }, [inspectorRef, selectedNodeId])
}

/**
 * The confirmation asked before a region with members is deleted.
 *
 * The dialog is already open when it mounts, so it resolves the pending
 * promise with whatever the author chooses next.
 *
 * @param props.pending - The member count and the promise to settle.
 * @param props.onSettle - Receives the author's choice.
 */
function IterationDeletionDialog({
  pending,
  onSettle,
}: {
  pending: PendingIterationDeletion
  onSettle: (confirmed: boolean) => void
}) {
  const { t } = useTranslation()
  return (
    <Dialog open onOpenChange={(open) => !open && onSettle(false)}>
      <DialogContent showCloseButton={false}>
        <DialogHeader>
          <DialogTitle>{t('workflows.iteration.deleteRegionTitle')}</DialogTitle>
          <DialogDescription>
            {t('workflows.iteration.deleteRegionMessage', {
              total: pending.memberCount,
            })}
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" onClick={() => onSettle(false)}>
            {t('workflows.iteration.cancelDelete')}
          </Button>
          <Button variant="destructive" onClick={() => onSettle(true)}>
            {t('workflows.iteration.confirmDelete')}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

/** The strip between the page header and the canvas: counts, version, save state. */
function EditorToolbar({
  nodeCount,
  edgeCount,
  version,
  status,
  onSave,
  historyTools,
  variablesTools,
  launchTools,
  runTools,
  versionTools,
  transferTools,
}: {
  /** Nodes currently on the canvas. */
  nodeCount: number
  /** Edges currently on the canvas. */
  edgeCount: number
  /** Version of the document as the server last confirmed it. */
  version: number
  /** Autosave state driving the indicator and the save button. */
  status: WorkflowDraftSaveStatus
  /** Writes the draft immediately instead of waiting out the debounce. */
  onSave: () => void
  /** Undo, redo and the change-history list for this editing session. */
  historyTools: ReactNode
  /** Workflow-wide variable editing for the current document. */
  variablesTools: ReactNode
  /** The `@` form's launch-field declaration for the current document. */
  launchTools: ReactNode
  /** Publish and version-history controls for the current document. */
  versionTools: ReactNode
  /** Import and export controls for the current document. */
  transferTools: ReactNode
  /** Run-now and run-history controls for the current document. */
  runTools: ReactNode
}) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center gap-2 border-b border-border px-3 py-1.5">
      <span className="text-xs text-muted-foreground">
        {t('workflows.detail.nodeCount', { count: nodeCount })} ·{' '}
        {t('workflows.detail.edgeCount', { count: edgeCount })}
      </span>
      <Badge variant="secondary">{t('workflows.detail.version', { version })}</Badge>
      <div className="ml-auto flex items-center gap-2">
        {historyTools}
        {variablesTools}
        {launchTools}
        {runTools}
        {transferTools}
        {versionTools}
        <WorkflowDraftSaveStatusLabel status={status} />
        <Button
          variant="outline"
          size="sm"
          title={t('workflows.editor.saveHint')}
          disabled={status === 'clean' || status === 'saving'}
          onClick={onSave}
        >
          <Save className="size-3.5" />
          {t('workflows.editor.save')}
        </Button>
      </div>
    </div>
  )
}
