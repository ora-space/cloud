import { useId } from 'react'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { useRuns } from '@/features/issues/api'
import type { IssueRun, TenantMember } from '@/features/issues/types'
import { memberNameById } from '@/features/issues/present'
import { RunDelivery } from './run-delivery'
import { RunResume } from './run-resume'
import { RunPreparation, SessionFailure } from './run-preparation'
import { useLoadOlderThread, useThread, type ThreadRef, type ThreadSnapshot } from './thread-api'
import { ThreadComposer } from './thread-composer'
import { latestAgentRun, threadStateLabel } from './thread-entries'
import { ThreadMessages } from './thread-messages'

function DeclaredThread({
  threadRef,
  snapshot,
  names,
  preparation,
}: {
  threadRef: ThreadRef
  snapshot: Extract<ThreadSnapshot, { declared: true }>
  names: ReadonlyMap<string, string>
  preparation: IssueRun['preparation']
}) {
  const loadOlder = useLoadOlderThread(threadRef)
  const oldest = snapshot.entries[0]?.seq
  return (
    <>
      {snapshot.failureCode && <SessionFailure code={snapshot.failureCode} />}
      {snapshot.threadState === 'pending' && preparation && (
        <RunPreparation progress={preparation} />
      )}
      <ThreadMessages
        entries={snapshot.entries}
        hasOlder={oldest !== undefined && oldest > 1}
        loadingOlder={loadOlder.isPending}
        onLoadOlder={() => {
          if (oldest !== undefined) loadOlder.mutate(oldest)
        }}
      />
      {loadOlder.isError && <p className="text-xs text-destructive">加载更早的消息失败</p>}
      <p className="text-xs text-muted-foreground">
        发起者：
        {snapshot.initiatorUserId ? (names.get(snapshot.initiatorUserId) ?? '用户') : '历史会话'}
      </p>
      {snapshot.model && (
        <p className="break-all text-xs text-muted-foreground">
          {snapshot.model.connectionName} · {snapshot.model.modelName}（{snapshot.model.modelId}）
        </p>
      )}
      <ThreadComposer
        threadRef={threadRef}
        threadState={snapshot.threadState}
        canAppend={snapshot.canAppend}
        canEnd={snapshot.canEnd}
      />
    </>
  )
}

function RunThread({
  threadRef,
  run,
  names,
  runs,
}: {
  threadRef: ThreadRef
  run: IssueRun
  names: ReadonlyMap<string, string>
  runs: readonly IssueRun[]
}) {
  const thread = useThread(threadRef)
  const headingId = useId()
  const snapshot = thread.data

  return (
    <section aria-labelledby={headingId} className="space-y-3 rounded-lg border p-3">
      <div className="flex items-center justify-between gap-2">
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
      {snapshot?.declared === false && <RunPreparation progress={run.preparation ?? null} />}
      {snapshot?.declared && (
        <DeclaredThread
          threadRef={threadRef}
          snapshot={snapshot}
          names={names}
          preparation={run.preparation}
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
}: {
  tid: string
  issueId: string
  members?: TenantMember[]
}) {
  const { data: runs = [] } = useRuns(tid, issueId)
  const run = latestAgentRun(runs)
  if (!run) return null
  return (
    <aside className="w-full shrink-0 md:w-80">
      <RunThread
        key={run.id}
        run={run}
        runs={runs}
        threadRef={{ tid, issueId, runId: run.id }}
        names={memberNameById(members)}
      />
    </aside>
  )
}
