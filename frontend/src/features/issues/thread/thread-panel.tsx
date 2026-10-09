import { useId } from 'react'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { useRuns } from '@/features/issues/api'
import { useLoadOlderThread, useThread, type ThreadRef, type ThreadSnapshot } from './thread-api'
import { ThreadComposer } from './thread-composer'
import { latestAgentRun, threadStateLabel } from './thread-entries'
import { ThreadMessages } from './thread-messages'

function DeclaredThread({
  threadRef,
  snapshot,
}: {
  threadRef: ThreadRef
  snapshot: Extract<ThreadSnapshot, { declared: true }>
}) {
  const loadOlder = useLoadOlderThread(threadRef)
  const oldest = snapshot.entries[0]?.seq
  return (
    <>
      <ThreadMessages
        entries={snapshot.entries}
        hasOlder={oldest !== undefined && oldest > 1}
        loadingOlder={loadOlder.isPending}
        onLoadOlder={() => {
          if (oldest !== undefined) loadOlder.mutate(oldest)
        }}
      />
      {loadOlder.isError && <p className="text-xs text-destructive">加载更早的消息失败</p>}
      <ThreadComposer threadRef={threadRef} threadState={snapshot.threadState} />
    </>
  )
}

function RunThread({ threadRef }: { threadRef: ThreadRef }) {
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
            {threadStateLabel(snapshot.threadState)}
          </Badge>
        )}
      </div>
      {thread.isPending && <Skeleton className="h-16 w-full" />}
      {thread.isError && <p className="text-sm text-destructive">会话加载失败</p>}
      {snapshot?.declared === false && (
        <p role="status" className="text-sm text-muted-foreground">
          等待 Agent 会话启动…
        </p>
      )}
      {snapshot?.declared && <DeclaredThread threadRef={threadRef} snapshot={snapshot} />}
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
export function IssueThreadPanel({ tid, issueId }: { tid: string; issueId: string }) {
  const { data: runs = [] } = useRuns(tid, issueId)
  const run = latestAgentRun(runs)
  if (!run) return null
  return (
    <aside className="w-full shrink-0 md:w-80">
      <RunThread key={run.id} threadRef={{ tid, issueId, runId: run.id }} />
    </aside>
  )
}
