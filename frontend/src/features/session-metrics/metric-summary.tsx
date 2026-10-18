import type { SessionModelStatistics } from '@/api/generated.schemas'
import {
  coverageLabel,
  inputDescription,
  metricDuration,
  metricNumber,
  metricTime,
  outcomeLabels,
  usageFields,
} from './metric-values'
import type { MetricAggregate } from './metric-values'

const statuses = [
  'started',
  'succeeded',
  'failed',
  'cancelled',
  'interrupted',
  'incomplete',
] as const

function CoveredMetric({
  label,
  metric,
  duration = false,
}: {
  label: string
  metric: MetricAggregate
  duration?: boolean
}) {
  return (
    <div className="rounded-lg border bg-card p-3">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd className="mt-1 text-xl font-semibold tabular-nums">
        {duration ? metricDuration(metric.value) : metricNumber(metric.value)}
      </dd>
      <dd className="mt-1 text-xs text-muted-foreground">{coverageLabel(metric)}</dd>
    </div>
  )
}

/** Summarizes all recorded gateway attempts, independently of the request table's page. */
export function MetricSummary({ statistics }: { statistics: SessionModelStatistics }) {
  return (
    <section aria-label="模型请求汇总" className="space-y-4">
      <div className="rounded-xl border bg-muted/30 p-4 text-sm">
        <p className="font-medium">Gateway 观测 · 仅本次运行中已记录的 HTTP 请求</p>
        <p className="mt-1 text-xs text-muted-foreground">
          一次 HTTP 尝试计为一次请求，重试分别计数。采集启用前或绕过 Gateway 的请求不在此范围内。
          汇总覆盖已记录请求的全部分页，不代表完整历史。待完成观测可能仍在进行，也可能已结束但未采集到完成记录。
        </p>
        <p className="mt-2 text-xs text-muted-foreground">
          记录范围：{metricTime(statistics.firstRecordedAt)} –{' '}
          {metricTime(statistics.lastRecordedAt)}
        </p>
      </div>
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-4 xl:grid-cols-7">
        <div className="rounded-lg border bg-card p-3">
          <dt className="text-xs text-muted-foreground">已记录请求</dt>
          <dd className="mt-1 text-2xl font-semibold">{statistics.counts.total}</dd>
        </div>
        {statuses.map((status) => (
          <div key={status} className="rounded-lg border bg-card p-3">
            <dt className="text-xs text-muted-foreground">{outcomeLabels[status]}</dt>
            <dd className="mt-1 text-2xl font-semibold">{statistics.counts[status]}</dd>
          </div>
        ))}
      </dl>
      <div className="space-y-2">
        <h3 className="text-sm font-medium">上游报告的用量</h3>
        <p className="text-xs text-muted-foreground">
          {inputDescription(statistics.inputSemantics)} 推理 Token 是输出明细，不再与输出相加。
          未知值不计入已知用量合计；这里没有按文本估算或费用换算。
        </p>
        <dl className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {usageFields.map((field) => (
            <CoveredMetric
              key={field.key}
              label={field.label}
              metric={statistics.usage[field.key]}
            />
          ))}
        </dl>
      </div>
      <div className="space-y-2">
        <h3 className="text-sm font-medium">Gateway 观测时长</h3>
        <dl className="grid gap-3 sm:grid-cols-3">
          <CoveredMetric label="平均请求时长" metric={statistics.timing.meanDurationMs} duration />
          <CoveredMetric label="P90 请求时长" metric={statistics.timing.p90DurationMs} duration />
          <CoveredMetric
            label="平均首次有效输出延迟"
            metric={statistics.timing.meanFirstOutputMs}
            duration
          />
        </dl>
        <p className="text-xs text-muted-foreground">
          首次有效输出从 Gateway 发出请求开始计时，忽略心跳和空片段；不等同于模型内部生成耗时。
          时长只统计已知样本，包含真实的 0ms。
        </p>
      </div>
    </section>
  )
}
