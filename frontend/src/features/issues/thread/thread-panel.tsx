import { useId } from 'react'
import { useInlineThread } from './conversation-viewport'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { useRuns } from '@/features/issues/api'
import type { IssueRun, TenantMember } from '@/features/issues/types'
import { memberNameById } from '@/features/issues/present'
import { RunDelivery } from './run-delivery'
import { RunResume } from './run-resume'
import { RunPreparation, SessionFailure } from './run-preparation'
import { useLoadOlderThread, useThread, type ThreadRef, type ThreadSnapshot } from './thread-api'
import { EndSessionControl, ThreadComposer } from './thread-composer'
import { isThreadClosed, latestAgentRun, threadStateLabel } from './thread-entries'
import { ThreadMessages } from './thread-messages'
import { useThreadPreview } from '@/features/thread-preview/use-thread-preview'
import { mergeThreadPreview } from '@/features/thread-preview/preview'

const PREVIEW_STATUS = {
  live: '实时输出已连接',
  disconnected: '实时连接已断开，正在恢复；完整记录仍会保存',
  connecting: '等待实时预览；当前显示已保存记录',
  unavailable: '实时预览未启用；当前显示已保存记录',
} as const

function chosenAgentRun(
  runs: readonly IssueRun[],
  selectedRunId: string | undefined,
): IssueRun | undefined {
  if (selectedRunId)
    return runs.find((run) => run.id === selectedRunId && run.executorType === 'agent')
  return latestAgentRun(runs)
}

function taskMayEndThread(
  snapshot: Extract<ThreadSnapshot, { declared: true }>,
  readOnly: boolean,
  showEndControl: boolean,
  endActorId: string | undefined,
): boolean {
  return (
    readOnly &&
    showEndControl &&
    !!endActorId &&
    endActorId === snapshot.initiatorUserId &&
    snapshot.canEnd &&
    !isThreadClosed(snapshot.threadState)
  )
}

function isHistoricalSelection(
  selectedRunId: string | undefined,
  latestRunId: string | undefined,
  showEndControl: boolean,
): boolean {
  return !!selectedRunId && selectedRunId !== latestRunId && !showEndControl
}

function threadAuthor(
  id: string | null,
  current: string | undefined,
  names: ReadonlyMap<string, string>,
) {
  if (id && id === current) return '你'
  return names.get(id ?? '') ?? '用户'
}

function DeclaredThread({
  threadRef,
  snapshot,
  names,
  preparation,
  readOnly,
  showEndControl,
  endActorId,
  onQuote,
}: {
  threadRef: ThreadRef
  snapshot: Extract<ThreadSnapshot, { declared: true }>
  names: ReadonlyMap<string, string>
  preparation: IssueRun['preparation']
  readOnly: boolean
  showEndControl: boolean
  endActorId: string | undefined
  onQuote?: ((text: string) => void) | undefined
}) {
  const loadOlder = useLoadOlderThread(threadRef)
  const preview = useThreadPreview(threadRef, snapshot.threadState !== 'ended')
  const oldest = snapshot.entries[0]?.seq
  return (
    <>
      {snapshot.failureCode && <SessionFailure code={snapshot.failureCode} />}
      {snapshot.threadState === 'pending' && preparation && (
        <RunPreparation progress={preparation} />
      )}
      <ThreadMessages
        userName={threadAuthor(snapshot.initiatorUserId, endActorId, names)}
        entries={mergeThreadPreview(snapshot.entries, preview.snapshot)}
        liveRevision={preview.snapshot.version}
        hasOlder={oldest !== undefined && oldest > 1}
        loadingOlder={loadOlder.isPending}
        onLoadOlder={() => {
          if (oldest !== undefined) loadOlder.mutate(oldest)
        }}
        onQuote={onQuote}
      />
      {snapshot.threadState !== 'ended' && (
        <p role="status" className="text-xs text-muted-foreground">
          {PREVIEW_STATUS[preview.connection]}
          {preview.snapshot.incomplete ? ' · 部分内容需等待完整记录' : ''}
        </p>
      )}
      {loadOlder.isError && <p className="text-xs text-destructive">加载更早的消息失败</p>}
      <details className="text-xs text-muted-foreground">
        <summary className="cursor-pointer">执行身份与模型</summary>
        <div className="mt-2 space-y-1">
          <p className="text-xs text-muted-foreground">
            发起者：
            {snapshot.initiatorUserId
              ? (names.get(snapshot.initiatorUserId) ?? '用户')
              : '历史会话'}
          </p>
          {snapshot.model && (
            <p className="break-all text-xs text-muted-foreground">
              {snapshot.model.connectionName} · {snapshot.model.modelName}（{snapshot.model.modelId}
              ）
            </p>
          )}
        </div>
      </details>
      {!readOnly && (
        <ThreadComposer
          threadRef={threadRef}
          threadState={snapshot.threadState}
          canAppend={snapshot.canAppend}
          canEnd={snapshot.canEnd}
        />
      )}
      {taskMayEndThread(snapshot, readOnly, showEndControl, endActorId) && (
        <EndSessionControl threadRef={threadRef} closed={false} />
      )}
    </>
  )
}

function UndeclaredRun({ run }: { run: IssueRun }) {
  const settled = ['completed', 'failed', 'cancelled'].includes(run.status)
  if (!settled || run.preparation) return <RunPreparation progress={run.preparation ?? null} />
  return (
    <p role="status" className="text-sm text-muted-foreground">
      运行已结束，未产生会话记录。执行状态：{run.status === 'failed' ? '失败' : '已结束'}
    </p>
  )
}

function RunThread({
  threadRef,
  run,
  names,
  runs,
  readOnly,
  showEndControl,
  endActorId,
  onQuote,
}: {
  threadRef: ThreadRef
  run: IssueRun
  names: ReadonlyMap<string, string>
  runs: readonly IssueRun[]
  readOnly: boolean
  showEndControl: boolean
  endActorId: string | undefined
  onQuote?: ((text: string) => void) | undefined
}) {
  const inline = useInlineThread()
  const settled = ['completed', 'failed', 'cancelled'].includes(run.status)
  const thread = useThread(threadRef, settled)
  const headingId = useId()
  const snapshot = thread.isError ? undefined : thread.data

  return (
    <section
      aria-labelledby={headingId}
      className={inline ? 'space-y-3' : 'space-y-3 rounded-lg border p-3'}
    >
      <div className={inline ? 'sr-only' : 'flex items-center justify-between gap-2'}>
        <h2 id={headingId} className="text-sm font-semibold">
          Agent 会话
        </h2>
        {snapshot?.declared && (
          <Badge variant="secondary" aria-live="polite">
            {snapshot.failureCode ? '运行失败' : threadStateLabel(snapshot.threadState)}
          </Badge>
        )}
      </div>
      <RunResume run={run} runs={runs} />
      {thread.isPending && <Skeleton className="h-16 w-full" />}
      {thread.isError && <p className="text-sm text-destructive">会话加载失败</p>}
      {snapshot?.declared === false && <UndeclaredRun run={run} />}
      {snapshot?.declared && (
        <DeclaredThread
          threadRef={threadRef}
          snapshot={snapshot}
          names={names}
          preparation={run.preparation}
          readOnly={readOnly}
          showEndControl={showEndControl}
          endActorId={endActorId}
          onQuote={onQuote}
        />
      )}
      <RunDelivery
        run={run}
        threadEnded={snapshot?.declared === true && snapshot.threadState === 'ended'}
      />
    </section>
  )
}

/**
 * The issue page's "Agent 会话" column: the Thread of the issue's most recent
 * agent run, with paging, live updates and the composer. It renders nothing
 * when the issue has no agent run, so issues worked by people, teams or
 * workflows keep their layout. The Thread is keyed by run, so a newer agent
 * run starts from its own tail instead of inheriting the previous one's rows.
 */
export function IssueThreadPanel({
  tid,
  issueId,
  members,
  selectedRunId,
  readOnly = false,
  showEndControl = false,
  endActorId,
  onQuote,
}: {
  tid: string
  issueId: string
  members?: TenantMember[]
  /** A historical run chosen by the task page; never silently falls back to another run. */
  selectedRunId?: string | undefined
  /** A task-level composer owns sends while multiple run transcripts are shown. */
  readOnly?: boolean
  /** Permit ending the latest task Thread while its task-level composer owns all sends. */
  showEndControl?: boolean
  /** Current task user; only the original session initiator receives the task end action. */
  endActorId?: string | undefined
  onQuote?: ((text: string, agentId: string) => void) | undefined
}) {
  const { data: runs = [], isPending, isError } = useRuns(tid, issueId)
  const inline = useInlineThread()
  const latest = latestAgentRun(runs)
  const run = chosenAgentRun(runs, selectedRunId)
  if (isError) return <p role="alert">执行记录加载失败，请稍后重试。</p>
  if (selectedRunId && !isPending && !run) return <p role="alert">该执行记录不存在或已不可访问。</p>
  if (!run) return null
  return (
    <div className="min-w-0 w-full">
      {isHistoricalSelection(selectedRunId, latest?.id, showEndControl || inline) && (
        <p className="mb-3 text-xs text-muted-foreground">
          正在查看历史执行；继续对话请返回最新执行。
        </p>
      )}
      <RunThread
        key={run.id}
        run={run}
        runs={runs}
        threadRef={{ tid, issueId, runId: run.id }}
        names={memberNameById(members)}
        readOnly={readOnly || isHistoricalSelection(selectedRunId, latest?.id, false)}
        showEndControl={showEndControl}
        endActorId={endActorId}
        onQuote={onQuote ? (text) => onQuote(text, run.executorId) : undefined}
      />
    </div>
  )
}
