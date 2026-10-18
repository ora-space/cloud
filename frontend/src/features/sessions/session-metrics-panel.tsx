import { SessionMetrics } from '@/features/session-metrics'
import type { useSessionContent } from './api'
import type { useSessionMetrics } from './metrics-api'
import { sessionFailure } from './presentation'

/** Keeps metrics available when an archive is missing while hiding failed or unauthorized cached reads. */
export function SessionMetricsPanel({
  content,
  metrics,
}: {
  content: ReturnType<typeof useSessionContent>
  metrics: ReturnType<typeof useSessionMetrics>
}) {
  const error = content.run.error ?? metrics.statistics.error ?? metrics.calls.error
  if (error)
    return (
      <p role="alert" className="text-sm text-destructive">
        {sessionFailure(error)}
      </p>
    )
  if (content.run.data && content.run.data.executorType !== 'agent')
    return <p>首期仅支持 Agent 任务会话。</p>
  if (content.run.isPending || metrics.statistics.isPending || metrics.calls.isPending) {
    return <p role="status">正在读取模型请求观测…</p>
  }
  if (!metrics.statistics.data || !metrics.calls.data) return null
  return (
    <SessionMetrics
      statistics={metrics.statistics.data}
      attempts={metrics.calls.data.items}
      pageIndex={metrics.pageIndex}
      pageSize={metrics.pageSize}
      hasNextPage={Boolean(metrics.calls.data.nextCursor)}
      isFetching={metrics.calls.isFetching || metrics.statistics.isFetching}
      onPrevious={metrics.previousPage}
      onNext={metrics.nextPage}
    />
  )
}
