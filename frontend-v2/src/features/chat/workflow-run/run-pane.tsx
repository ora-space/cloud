import { QueryClientProvider } from '@tanstack/react-query'
import { FlaskConicalIcon, MapIcon, PlayIcon, RotateCcwIcon, SquareIcon, TheaterIcon, WorkflowIcon } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
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
import { isTerminalRunStatus, type GraphWorkflowRun, type WorkflowArtifact, type WorkflowNodeConversationItem } from '@/lib/workflows/runtime'
import { FadeText } from '../components/fade-text'
import { ShowSidebarButton } from '../components/sidebar'
import { useChatStore } from '../store'
import { cancelDemoRun, rerunDemoRun, startDemoRun, useDemoRuns } from './demo-runs'
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
 * one act at a time, Overview shows the frozen graph with each node's status. The run is played
 * by the in-browser engine, so the header says it is a demo. The page brings the tenant's workflow
 * client and query cache, which the Agent configuration in the inspector reads.
 */
export default function RunPane({ runId }: { runId: string }) {
  const tenantId = useChatStore((s) => s.tenantId) ?? ''
  const client = useMemo(() => createContractsClient(createHttpWorkflowContracts(tenantId)), [tenantId])
  const [queryClient] = useState(createWorkflowQueryClient)
  const run = useDemoRuns((s) => s.runs[runId])
  return (
    <I18nextProvider i18n={runI18n}>
      <QueryClientProvider client={queryClient}>
        <ContractsClientProvider client={client}>
          {run ? (
            <RunWorkspace key={runId} run={run} />
          ) : (
            <div className="flex flex-1 items-center justify-center text-sm text-muted-foreground">
              <p>这次运行已经不在了。</p>
            </div>
          )}
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

function RunWorkspace({ run }: { run: GraphWorkflowRun }) {
  const { t } = useTranslation()
  const conversations = useDemoRuns((s) => s.conversations[run.id])
  const artifacts = useDemoRuns((s) => s.artifacts[run.id]) ?? noArtifacts
  const revealedArtifactId = useDemoRuns((s) => s.revealedArtifactIds[run.id]) ?? null
  const selectRun = useChatStore((s) => s.selectRun)
  // A live run opens on its Theater; a pending or finished one on Overview, as desktop primes it.
  const [viewMode, setViewMode] = useState<ViewMode>(run.status === 'running' || run.status === 'awaiting_input' ? 'theater' : 'overview')
  const [focusNodeId, setFocusNodeId] = useState<string | null>(null)
  /** Node whose conversation is open; it pins the act until closed. */
  const [conversationNodeId, setConversationNodeId] = useState<string | null>(null)
  const [openInspectorOnTheaterEnter, setOpenInspectorOnTheaterEnter] = useState(false)
  const [overviewFitRequestKey, setOverviewFitRequestKey] = useState(0)
  const [startOpen, setStartOpen] = useState(false)
  const [busy, setBusy] = useState(false)
  const stageAreaRef = useRef<HTMLDivElement>(null)
  const focusStatusSampleRef = useRef<TheaterFocusStatusSample | null>(null)
  const conversationByNodeId = useConversationByNode(conversations)

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
      setFocusNodeId(null)
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
  useEffect(() => {
    if (focusNodeId === null || isLoopHistoryNode(run, focusNodeId)) {
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
  }, [run, focusNodeId, conversationNodeId])

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

  const act = async (action: () => Promise<void>) => {
    setBusy(true)
    try {
      await action()
    } finally {
      setBusy(false)
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
    const stageWidth = stageAreaRef.current?.getBoundingClientRect().width ?? Number.POSITIVE_INFINITY
    setOpenInspectorOnTheaterEnter(!waiting && stageWidth >= NARROW_THEATER_INSPECTOR_AUTO_OPEN_WIDTH)
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
    <>
      <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4">
        <ShowSidebarButton />
        <WorkflowIcon className="size-4 shrink-0 text-muted-foreground" />
        <h2 className="flex min-w-0 text-sm font-medium">
          <FadeText text={run.name} />
        </h2>
        <Badge variant="outline" className={cn('gap-1.5', tone.badge)}>
          <span className={cn('size-1.5 rounded-full', tone.dot)} />
          {runStatusLabel(run.status)}
        </Badge>
        <Badge variant="outline" className="gap-1 text-muted-foreground" title="后端还不能执行工作流，节点由浏览器里的演示引擎按顺序推进">
          <FlaskConicalIcon />
          演示运行
        </Badge>
        <div className="ml-auto flex shrink-0 items-center gap-1.5">
          <div role="group" aria-label={t('workflowRun.viewMode.label')} className="inline-flex rounded-lg border p-0.5">
            <Button size="sm" variant={viewMode === 'theater' ? 'secondary' : 'ghost'} className="h-7 gap-1.5 px-2.5 text-xs" aria-pressed={viewMode === 'theater'} onClick={enterTheaterFromHeader}>
              <TheaterIcon className="size-3.5" />
              {t('workflowRun.viewMode.theater')}
            </Button>
            <Button
              size="sm"
              variant={viewMode === 'overview' ? 'secondary' : 'ghost'}
              className="h-7 gap-1.5 px-2.5 text-xs"
              aria-pressed={viewMode === 'overview'}
              onClick={() => {
                setOpenInspectorOnTheaterEnter(false)
                setViewMode('overview')
                setOverviewFitRequestKey((key) => key + 1)
              }}
            >
              <MapIcon className="size-3.5" />
              {t('workflowRun.viewMode.overview')}
            </Button>
          </div>
          {run.status === 'pending' && (
            <Button size="sm" className="h-7 px-2.5 text-xs" onClick={() => setStartOpen(true)} disabled={busy}>
              <PlayIcon />
              启动
            </Button>
          )}
          {(run.status === 'running' || run.status === 'awaiting_input') && (
            <Button variant="outline" size="sm" className="h-7 px-2.5 text-xs" onClick={() => act(() => cancelDemoRun(run.id))} disabled={busy}>
              <SquareIcon />
              停止
            </Button>
          )}
          {finished && (
            <Button
              variant="outline"
              size="sm"
              className="h-7 px-2.5 text-xs"
              disabled={busy}
              onClick={() =>
                act(async () => {
                  const next = await rerunDemoRun(run.id)
                  selectRun(next.id)
                })
              }
            >
              <RotateCcwIcon />
              再跑一次
            </Button>
          )}
        </div>
      </header>
      <div ref={stageAreaRef} aria-label={viewMode === 'theater' ? '运行舞台' : undefined} className="relative flex min-h-0 min-w-0 flex-1 flex-col">
        {run.status === 'pending' && viewMode === 'overview' && (
          <p className="absolute inset-x-0 top-3 z-10 mx-auto w-fit rounded-md border bg-background/90 px-3 py-1.5 text-xs text-muted-foreground shadow-sm">
            还没开始：点右上角「启动」，填写输入后运行
          </p>
        )}
        {viewMode === 'theater' ? (
          <RunTheater
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
      <WorkflowRunStartDialog
        open={startOpen}
        runName={run.name}
        initialPrompt={startNode?.data.input ?? ''}
        variables={startNode?.data.inputVariables ?? []}
        busy={busy}
        onOpenChange={setStartOpen}
        onStart={async (prompt, variables) => {
          await act(() => startDemoRun(run.id, prompt, variables))
          setStartOpen(false)
          setViewMode('theater')
        }}
      />
    </>
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
