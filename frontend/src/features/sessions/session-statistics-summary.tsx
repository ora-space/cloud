import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { getApiV1TenantsTidIssuesIidRunsRidStatistics } from '@/api/tenants/tenants'
import { Button } from '@/components/ui/button'
import {
  coverageLabel,
  metricDuration,
  metricNumber,
} from '@/features/session-metrics/metric-values'
import type { SessionRef } from './api'

/** Fetches only the selected run's aggregates, preserving unknown measurements and coverage. */
export function SessionStatisticsSummary({
  sessionRef,
  detailsHref,
}: {
  sessionRef: SessionRef
  detailsHref: string | undefined
}) {
  const statistics = useQuery({
    queryKey: ['session-model-statistics', sessionRef],
    queryFn: ({ signal }) =>
      getApiV1TenantsTidIssuesIidRunsRidStatistics(
        sessionRef.tid,
        sessionRef.issueId,
        sessionRef.runId,
        undefined,
        signal,
      ),
    retry: false,
  })
  const data = statistics.isError ? undefined : statistics.data
  return (
    <section aria-label="所选执行统计" className="space-y-2 border-t pt-4">
      <h2 className="font-medium">所选执行统计</h2>
      <p className="text-xs text-muted-foreground">仅本次执行 · Gateway 已记录请求</p>
      {statistics.isPending && <p className="text-xs">加载统计中…</p>}
      {statistics.isError && (
        <p role="alert" className="text-xs">
          统计暂不可用
        </p>
      )}
      {data?.counts.total === 0 && (
        <p className="text-xs text-muted-foreground">暂无观测，不代表历史消耗为零。</p>
      )}
      {data && (
        <dl className="space-y-2 text-xs">
          <div className="flex justify-between">
            <dt>已记录请求</dt>
            <dd>{data.counts.total}</dd>
          </div>
          {(['inputTokens', 'outputTokens', 'totalTokens'] as const).map((key, index) => (
            <div key={key} className="grid grid-cols-[1fr_auto] gap-x-2">
              <dt>{['输入 Token', '输出 Token', '上游总 Token'][index]}</dt>
              <dd>{metricNumber(data.usage[key].value)}</dd>
              <dd className="col-span-2 mt-0.5 text-muted-foreground">
                {coverageLabel(data.usage[key])}
              </dd>
            </div>
          ))}
          <div className="grid grid-cols-[1fr_auto] gap-x-2">
            <dt>平均请求时长</dt>
            <dd>{metricDuration(data.timing.meanDurationMs.value)}</dd>
            <dd className="col-span-2 text-muted-foreground">
              {coverageLabel(data.timing.meanDurationMs)}
            </dd>
          </div>
        </dl>
      )}
      <div className="flex gap-3 text-xs">
        <Button
          size="xs"
          variant="ghost"
          disabled={statistics.isFetching}
          onClick={() => void statistics.refetch()}
        >
          刷新统计
        </Button>
        {detailsHref && (
          <Link className="self-center underline" to={detailsHref}>
            完整时间线与统计
          </Link>
        )}
      </div>
    </section>
  )
}
