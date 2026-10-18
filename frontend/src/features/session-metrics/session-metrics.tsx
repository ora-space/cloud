import type { SessionModelAttempt, SessionModelStatistics } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { MetricSummary } from './metric-summary'
import { RequestTable } from './request-table'
import { RequestTrend } from './request-trend'

interface MetricsProps {
  statistics: SessionModelStatistics
  attempts: SessionModelAttempt[]
  pageIndex: number
  pageSize: number
  hasNextPage: boolean
  isFetching: boolean
  onPrevious: () => void
  onNext: () => void
}

/** Renders server-observed metrics without reading archives, fetching data or estimating missing usage. */
export function SessionMetrics(props: MetricsProps) {
  const { statistics, attempts, pageIndex, pageSize, isFetching, hasNextPage, onPrevious, onNext } =
    props
  if (statistics.counts.total === 0 && attempts.length === 0) {
    return (
      <section
        aria-label="模型请求统计"
        className="rounded-xl border border-dashed p-8 text-center"
      >
        <h2 className="text-base font-medium">暂无模型请求观测</h2>
        <p className="mt-2 text-sm text-muted-foreground">
          此运行尚无 Gateway 记录的模型请求。历史用量不会根据会话文本推算，未知不代表零消耗。
        </p>
      </section>
    )
  }
  return (
    <section aria-label="模型请求统计" className="space-y-5">
      <MetricSummary statistics={statistics} />
      {attempts.length > 0 && <RequestTrend attempts={attempts} offset={pageIndex * pageSize} />}
      <RequestTable attempts={attempts} offset={pageIndex * pageSize} />
      <nav aria-label="模型请求分页" className="flex flex-wrap items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">
          第 {pageIndex + 1} 页 · 每页最多 {pageSize} 条 · 按开始时间从早到晚
        </p>
        <div className="flex gap-2">
          <Button
            variant="outline"
            size="sm"
            disabled={pageIndex === 0 || isFetching}
            onClick={onPrevious}
          >
            上一页请求
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={!hasNextPage || isFetching}
            onClick={onNext}
          >
            下一页请求
          </Button>
        </div>
      </nav>
    </section>
  )
}
