import { QueryClientProvider, useQuery, useQueryClient } from '@tanstack/react-query'
import { MapIcon, MessageSquareIcon, PlayIcon, RotateCcwIcon, SquareIcon, TheaterIcon, WorkflowIcon } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { I18nextProvider, useTranslation } from 'react-i18next'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Toaster } from '@/components/ui/sonner'
import { cn } from '@/lib/utils'
import { createContractsClient, createHttpWorkflowContracts } from '@/lib/workflows/data/contracts-client'
import { ContractsClientProvider } from '@/lib/workflows/data/contracts-client-provider'
import { createWorkflowQueryClient } from '@/lib/workflows/data/query-client'
import { createWorkflowI18n } from '@/lib/workflows/host/i18n'
import { useDarkTheme } from '@/lib/workflows/host/theme'
import { toast } from '@/lib/workflows/host/ui'
import { ContractError } from '@/lib/workflows/contracts'
import { localizeContractError } from '@/lib/workflows/host/contract-error'
import { isTerminalRunStatus, type GraphWorkflowRun, type WorkflowArtifact, type WorkflowNodeConversationItem } from '@/lib/workflows/runtime'
import { WorkspaceNavigationProvider } from '@/components/workspace-panel/navigation'
import { useWorkspacePanel } from '@/components/workspace-panel/use-panel'
import { WorkspacePanel, WorkspaceToggle } from '@/components/workspace-panel/workspace-panel'
import { WorkspaceSplit } from '@/components/workspace-panel/workspace-split'
import { FadeText } from '../components/fade-text'
import { ShowSidebarButton } from '../components/sidebar'
import { emptyDraft, newChatKey, useChatStore } from '../store'
import { useRunInvalidations } from './run-events'
import { cancelRun, rerun, runKeys, runQuery, runSnapshotQuery, startRun } from './server-runs'
import { isLoopHistoryNode } from './loop-round-state'
import { resolveStageFocusNodeId, resolveTheaterFocus, shouldReleaseLivePinToFollow, shouldStealFocusForArtifactReveal, type TheaterFocusStatusSample } from './run-focus'
import { RunOverviewCanvas } from './run-overview-canvas'
import { runStatusLabel, runStatusTone } from './run-status-style'
import { RunTheater } from './run-theater'
import { workflowRunTranslations } from './translations'
import { WorkflowRunStartDialog } from './workflow-run-start-dialog'

/** The workflow bundles plus the run view's strings, for the components copied from desktop. */
const runI18n = createWorkflowI18n(['zh-CN'], { 'zh-CN': workflowRunTranslations['zh-CN'] })

/** Below this width, Overview → Theater skips auto-opening the act inspector, as in desktop. */
const NARROW_THEATER_INSPECTOR_AUTO_OPEN_WIDTH = 1_000

type ViewMode = 'theater' | 'overview'

const noArtifacts: WorkflowArtifact[] = []

/**
 * A workflow run in the chat page's main area, as desktop's run workspace: Theater plays the run
 * one act at a time, Overview shows the frozen graph with each node's status. v2-server executes
 * the run; the page reads it again each time its events stream announces a change, polling as a
 * fallback, until it ends. The page brings the tenant's
 * workflow client and query cache, which the run and the inspector's Agent configuration read.
 */
export default function RunPane({ runId }: { runId: string }) {
  const tenantId = useChatStore((s) => s.tenantId) ?? ''
  const client = useMemo(() => createContractsClient(createHttpWorkflowContracts(tenantId), tenantId), [tenantId])
  const [queryClient] = useState(createWorkflowQueryClient)
  // The sidebar's project run list lives in the page's cache, not the run page's own; a run this
  // page creates must reach it at once, since the list stops polling once every run has ended.
  const pageQueryClient = useQueryClient()
  const refreshProjectRuns = useCallback(
    (projectId: string) => void pageQueryClient.invalidateQueries({ queryKey: runKeys.project(tenantId, projectId) }),
    [pageQueryClient, tenantId],
  )
  return (
    <I18nextProvider i18n={runI18n}>
      <QueryClientProvider client={queryClient}>
        <ContractsClientProvider client={client}>
          <LoadedRun key={runId} tenantId={tenantId} runId={runId} refreshProjectRuns={refreshProjectRuns} />
          <RunToaster />
        </ContractsClientProvider>
      </QueryClientProvider>
    </I18nextProvider>
  )
}

function RunToaster() {
  const dark = useDarkTheme()
  return <Toaster position="bottom-right" theme={dark ? 'dark' : 'light'} closeButton />
}

/**
 * Reads the run's frozen graph once, then the run itself until it ends: again whenever its events
 * stream says it changed, with polling as the fallback.
 */
function LoadedRun({ tenantId, runId, refreshProjectRuns }: { tenantId: string; runId: string; refreshProjectRuns: (projectId: string) => void }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const snapshot = useQuery({ ...runSnapshotQuery(tenantId, runId), enabled: tenantId !== '' })
  const [followed, setFollowed] = useState(false)
  const run = useQuery(runQuery(tenantId, runId, snapshot.data, followed))
  const projectId = run.data?.projectId
  useRunInvalidations(tenantId, runId, run.data !== undefined && !isTerminalRunStatus(run.data.status), {
    invalidated: () => {
      void queryClient.invalidateQueries({ queryKey: runKeys.run(tenantId, runId) })
      if (projectId !== undefined) refreshProjectRuns(projectId)
    },
    connected: setFollowed,
  })
  const error = snapshot.error ?? run.error
  if (run.data) return <RunWorkspace tenantId={tenantId} run={run.data} refreshProjectRuns={refreshProjectRuns} />
  return (
    <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
      <p>{error === null ? '正在读取运行…' : isMissing(error) ? '这次运行已经不在了。' : localizeContractError(error, t)}</p>
    </div>
  )
}

const isMissing = (error: unknown) => error instanceof ContractError && error.code === 'workflow_run_not_found'

/** Theater's per-node conversation, from the answers the run projection carries. */
function runConversations(run: GraphWorkflowRun): WorkflowNodeConversationItem[] {
  return Object.values(run.nodeStates).flatMap((state) => state.conversation ?? [])
}

function RunWorkspace({ tenantId, run, refreshProjectRuns }: { tenantId: string; run: GraphWorkflowRun; refreshProjectRuns: (projectId: string) => void }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const conversations = useMemo(() => runConversations(run), [run])
  const artifacts = noArtifacts
  const revealedArtifactId: string | null = null
  const selectRun = useChatStore((s) => s.selectRun)
  const formNode = run.definitionSnapshot.nodes.find((node) =>
    node.data.kind === 'form' && ['awaiting_input', 'succeeded', 'cancelled'].includes(run.nodeStates[node.id]?.status ?? ''),
  )
  // A live run opens on its Theater; a pending or finished one on Overview, as desktop primes it.
  // A confirmation node is the run's result surface: open it on load, but keep every other node
  // reachable from the path rail.
  const [viewMode, setViewMode] = useState<ViewMode>(run.status === 'running' || run.status === 'awaiting_input' || formNode !== undefined ? 'theater' : 'overview')
  const [focusNodeId, setFocusNodeId] = useState<string | null>(formNode?.id ?? null)
  /** Node whose conversation is open; it pins the act until closed. */
  const [conversationNodeId, setConversationNodeId] = useState<string | null>(null)
  const [openInspectorOnTheaterEnter, setOpenInspectorOnTheaterEnter] = useState(false)
  const [overviewFitRequestKey, setOverviewFitRequestKey] = useState(0)
  const [startOpen, setStartOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const stageAreaRef = useRef<HTMLDivElement>(null)
  const focusStatusSampleRef = useRef<TheaterFocusStatusSample | null>(null)
  const conversationByNodeId = useConversationByNode(conversations)
  // The checkout every Agent node of the run shares; read again whenever a node or the run moves on.
  const workspace = useWorkspacePanel()
  const workspaceSource = useMemo(() => ({ kind: 'run' as const, tenantId, runId: run.id }), [tenantId, run.id])
  const workspaceRevision = `${run.status}:${Object.values(run.nodeStates)
    .map((state) => state.status)
    .join(',')}`

  const nodes = run.definitionSnapshot.nodes
  const startNode = nodes.find((n) => n.data.kind === 'start')
  const tone = runStatusTone(run.status)
  const finished = isTerminalRunStatus(run.status)
  const stageFocusNodeId = resolveStageFocusNodeId(conversationNodeId, focusNodeId)
  if (conversationNodeId !== null && focusNodeId !== conversationNodeId) setFocusNodeId(conversationNodeId)

  // Run edges desktop hears from its run stream: a finished run drops every pin so its result act
  // owns the stage, and a new human-in-the-loop gate asks for attention.
  const [previousStatus, setPreviousStatus] = useState(run.status)
  const [seenHitls, setSeenHitls] = useState(() => new Set(run.openHitls.map((h) => h.id)))
  if (previousStatus !== run.status) {
    setPreviousStatus(run.status)
    if (finished && !isTerminalRunStatus(previousStatus)) {
      setFocusNodeId(formNode?.id ?? null)
      setConversationNodeId(null)
    }
  }
  const freshHitls = run.openHitls.filter((h) => !seenHitls.has(h.id))
  if (freshHitls.length > 0) setSeenHitls(new Set(run.openHitls.map((h) => h.id)))
  const announcedFinish = useRef(finished)
  useEffect(() => {
    if (!finished || announcedFinish.current) return
    announcedFinish.current = true
    if (viewMode === 'overview') {
      toast.message(t('workflowRun.result.finishedToastTitle'), {
        description: t('workflowRun.result.finishedToastDescription'),
        action: { label: t('workflowRun.result.finishedToastAction'), onClick: () => setViewMode('theater') },
      })
    }
  }, [finished, viewMode, t])
  const freshHitl = freshHitls[0]
  useEffect(() => {
    if (!freshHitl) return
    toast.message(t('workflowRun.hitl.toastTitle'), {
      description: t(freshHitl.schema.kind === 'clarify' ? 'workflowRun.hitl.toastClarifyDescription' : 'workflowRun.hitl.toastDescription'),
      action: {
        label: t('workflowRun.hitl.toastAction'),
        onClick: () => {
          setFocusNodeId(freshHitl.nodeId)
          setOpenInspectorOnTheaterEnter(false)
          setViewMode('theater')
        },
      },
    })
  }, [freshHitl, t])

  // Live pin release: once the focused act itself finishes, Theater follows the run again.
  // Form and task results stay pinned: they own the run's outcome and must remain readable.
  useEffect(() => {
    const focusKind = focusNodeId === null ? undefined : nodes.find((node) => node.id === focusNodeId)?.data.kind
    if (focusNodeId === null || isLoopHistoryNode(run, focusNodeId) || focusKind === 'form' || focusKind === 'createTasks') {
      focusStatusSampleRef.current = null
      return
    }
    const status = run.nodeStates[focusNodeId]?.status
    if (shouldReleaseLivePinToFollow(conversationNodeId, focusStatusSampleRef.current, focusNodeId, status)) {
      focusStatusSampleRef.current = null
      setFocusNodeId(null)
      return
    }
    if (status !== undefined) focusStatusSampleRef.current = { nodeId: focusNodeId, status }
  }, [run, focusNodeId, conversationNodeId, nodes])

  // A new artifact moves Theater to the act that produced it, unless the reader is elsewhere on purpose.
  const [handledReveal, setHandledReveal] = useState<string | null>(null)
  const revealed = !finished && viewMode === 'theater' && revealedArtifactId !== handledReveal ? artifacts.find((item) => item.id === revealedArtifactId) : undefined
  if (revealed) {
    setHandledReveal(revealed.id)
    const stagePrimaryId = resolveTheaterFocus(run, stageFocusNodeId).primaryId
    const onLoopHistory = focusNodeId !== null && isLoopHistoryNode(run, focusNodeId)
    if (!onLoopHistory && shouldStealFocusForArtifactReveal({ conversationNodeId, stagePrimaryId, artifactNodeId: revealed.nodeId })) setFocusNodeId(revealed.nodeId)
  }

  // Esc from Theater returns to Overview.
  useEffect(() => {
    if (viewMode !== 'theater') return
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape' || event.defaultPrevented) return
      setOpenInspectorOnTheaterEnter(false)
      setViewMode('overview')
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [viewMode])

  /** Runs one command; a refusal shows as a toast, and the run is read again either way. */
  const act = async (action: () => Promise<void>): Promise<boolean> => {
    setBusy(true)
    try {
      await action()
      return true
    } catch (error) {
      toast.error(localizeContractError(error, t))
      return false
    } finally {
      setBusy(false)
      void queryClient.invalidateQueries({ queryKey: runKeys.run(tenantId, run.id) })
    }
  }

  function focusNode(nodeId: string) {
    if (conversationNodeId !== null && conversationNodeId !== nodeId) setConversationNodeId(null)
    setFocusNodeId(nodeId)
  }

  /** An Overview click opens that node's act, with its inspector when the stage is wide enough. */
  function focusNodeFromOverview(nodeId: string) {
    setConversationNodeId(null)
    setFocusNodeId(nodeId)
    const waiting = run.nodeStates[nodeId]?.status === 'awaiting_input'
    const kind = nodes.find((node) => node.id === nodeId)?.data.kind
    const stageWidth = stageAreaRef.current?.getBoundingClientRect().width ?? Number.POSITIVE_INFINITY
    setOpenInspectorOnTheaterEnter(!waiting && kind !== 'form' && kind !== 'createTasks' && stageWidth >= NARROW_THEATER_INSPECTOR_AUTO_OPEN_WIDTH)
    setViewMode('theater')
  }

  /** The header's Theater button; pressed again on a finished run, it goes back to the result act. */
  function enterTheaterFromHeader() {
    if (viewMode === 'theater' && finished && focusNodeId !== null) {
      setFocusNodeId(null)
      setConversationNodeId(null)
      return
    }
    setOpenInspectorOnTheaterEnter(false)
    setViewMode('theater')
  }

  return (
    <WorkspaceNavigationProvider value={workspace.navigation}>
      <header className="flex min-h-12 shrink-0 flex-wrap items-center gap-2 border-b px-3 py-2 sm:h-12 sm:flex-nowrap sm:px-4 sm:py-0">
        <ShowSidebarButton />
        <WorkflowIcon className="size-4 shrink-0 text-muted-foreground" />
        <h2 className="flex min-w-0 flex-1 text-sm font-medium">
          <FadeText text={run.name} />
        </h2>
        <Badge variant="outline" className={cn('gap-1.5', tone.badge)}>
          <span className={cn('size-1.5 rounded-full', tone.dot)} />
          {runStatusLabel(run.status)}
        </Badge>
        <div className="ml-auto flex shrink-0 items-center gap-1.5">
          <div role="group" aria-label={t('workflowRun.viewMode.label')} className="inline-flex rounded-lg border p-0.5">
            <Button size="sm" variant={viewMode === 'theater' ? 'secondary' : 'ghost'} className="h-7 gap-1.5 px-2.5 text-xs" aria-pressed={viewMode === 'theater'} aria-label={t('workflowRun.viewMode.theater')} onClick={enterTheaterFromHeader}>
              <TheaterIcon className="size-3.5" />
              <span className="hidden sm:inline">{t('workflowRun.viewMode.theater')}</span>
            </Button>
            <Button
              size="sm"
              variant={viewMode === 'overview' ? 'secondary' : 'ghost'}
              className="h-7 gap-1.5 px-2.5 text-xs"
              aria-pressed={viewMode === 'overview'}
              aria-label={t('workflowRun.viewMode.overview')}
              onClick={() => {
                setOpenInspectorOnTheaterEnter(false)
                setViewMode('overview')
                setOverviewFitRequestKey((key) => key + 1)
              }}
            >
              <MapIcon className="size-3.5" />
              <span className="hidden sm:inline">{t('workflowRun.viewMode.overview')}</span>
            </Button>
          </div>
          {run.status === 'pending' && (
            <Button size="sm" className="h-7 px-2.5 text-xs" aria-label="启动" onClick={() => setStartOpen(true)} disabled={busy}>
              <PlayIcon />
              <span className="hidden sm:inline">启动</span>
            </Button>
          )}
          {(run.status === 'running' || run.status === 'awaiting_input') && (
            <Button variant="outline" size="sm" className="h-7 px-2.5 text-xs" aria-label="停止" onClick={() => act(() => cancelRun(tenantId, run.id))} disabled={busy}>
              <SquareIcon />
              <span className="hidden sm:inline">停止</span>
            </Button>
          )}
          {finished && (
            <Button
              variant="outline"
              size="sm"
              className="h-7 px-2.5 text-xs"
              aria-label="再跑一次"
              disabled={busy}
              onClick={() =>
                act(async () => {
                  const next = await rerun(tenantId, run)
                  refreshProjectRuns(run.projectId)
                  selectRun(next)
                })
              }
            >
              <RotateCcwIcon />
              <span className="hidden sm:inline">再跑一次</span>
            </Button>
          )}
          {run.status === 'succeeded' && run.finalOutput && (
            <Button variant="outline" size="sm" className="h-7 px-2.5 text-xs" aria-label="在对话中追问" onClick={() => {
              const store = useChatStore.getState()
              const draft = store.drafts[newChatKey] ?? emptyDraft
              store.startNewChat({ projectId: run.projectId })
              store.setDraft(newChatKey, {
                ...draft,
                text: `${draft.text ? `${draft.text}\n\n` : ''}关于工作流「${run.name}」的交付结果，我想了解：\n\n--- 工作流上下文 ---\n本对话使用独立沙盒；以下是工作流的交付说明，不代表当前目录已包含工作流的修改。\n运行 ID：${run.id}\n需求：${run.kickoffInput ?? ''}\n交付结果：\n${run.finalOutput}`,
              })
            }}>
              <MessageSquareIcon />
              <span className="hidden sm:inline">在对话中追问</span>
            </Button>
          )}
          <WorkspaceToggle open={workspace.open} toggle={() => workspace.setOpen(!workspace.open)} />
        </div>
      </header>
      <WorkspaceSplit
        id="run"
        panel={
          workspace.open && <WorkspacePanel source={workspaceSource} revision={workspaceRevision} request={workspace.request} onClose={() => workspace.setOpen(false)} />
        }
      >
        <div ref={stageAreaRef} aria-label={viewMode === 'theater' ? '运行舞台' : undefined} className="relative flex min-h-0 min-w-0 flex-1 flex-col">
          {run.status === 'pending' && viewMode === 'overview' && (
            <p className="absolute inset-x-0 top-3 z-10 mx-auto w-fit rounded-md border bg-background/90 px-3 py-1.5 text-xs text-muted-foreground shadow-sm">
              还没开始：点右上角「启动」，填写输入后运行
            </p>
          )}
          {viewMode === 'theater' ? (
            <RunTheater
              tenantId={tenantId}
              run={run}
              focusNodeId={stageFocusNodeId}
              onFocusNode={focusNode}
              onClearFocus={() => {
                setFocusNodeId(null)
                setConversationNodeId(null)
              }}
              artifacts={artifacts}
              conversationByNodeId={conversationByNodeId}
              revealedArtifactId={revealedArtifactId}
              openInspectorOnMount={openInspectorOnTheaterEnter}
              onOpenInspectorOnMountConsumed={() => setOpenInspectorOnTheaterEnter(false)}
              sessionConversationNodeId={conversationNodeId}
              onSessionConversationNodeIdChange={(nodeId) => {
                setConversationNodeId(nodeId)
                if (nodeId !== null) setFocusNodeId(nodeId)
              }}
              onShowOverview={() => {
                setOpenInspectorOnTheaterEnter(false)
                setViewMode('overview')
              }}
            />
          ) : (
            <RunOverviewCanvas run={run} focusedNodeId={stageFocusNodeId} onFocusNode={focusNodeFromOverview} artifacts={artifacts} fitRequestKey={overviewFitRequestKey} />
          )}
        </div>
      </WorkspaceSplit>
      <WorkflowRunStartDialog
        open={startOpen}
        runName={run.name}
        initialPrompt={startNode?.data.input ?? ''}
        variables={startNode?.data.inputVariables ?? []}
        busy={busy}
        onOpenChange={setStartOpen}
        onStart={async (prompt, variables) => {
          if (!(await act(() => startRun(tenantId, run.id, prompt, variables)))) return
          setStartOpen(false)
          setViewMode('theater')
        }}
      />
    </WorkspaceNavigationProvider>
  )
}

/** Groups the run's conversation by node, the shape desktop's live run query hands Theater. */
function useConversationByNode(items: WorkflowNodeConversationItem[] | undefined): Map<string, WorkflowNodeConversationItem[]> {
  return useMemo(() => {
    const byNode = new Map<string, WorkflowNodeConversationItem[]>()
    for (const item of items ?? []) byNode.set(item.nodeId, [...(byNode.get(item.nodeId) ?? []), item])
    return byNode
  }, [items])
}
