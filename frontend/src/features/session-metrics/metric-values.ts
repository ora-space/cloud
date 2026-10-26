import type { SessionModelAttempt, SessionModelStatistics } from '@/api/generated.schemas'

/** Independent provider counters; cache and reasoning are never added to input or output. */
export const usageFields = [
  { key: 'inputTokens', label: '输入 Token', color: '#2563eb' },
  { key: 'outputTokens', label: '输出 Token', color: '#16a34a' },
  { key: 'totalTokens', label: '上游总 Token', color: '#64748b' },
  { key: 'cacheReadTokens', label: '缓存读取 Token', color: '#0d9488' },
  { key: 'cacheWriteTokens', label: '缓存写入 Token', color: '#9333ea' },
  { key: 'reasoningTokens', label: '推理 Token', color: '#d97706' },
] as const

/** One selectable counter in the request trend, without changing its provider semantics. */
export type UsageField = (typeof usageFields)[number]['key']

/** Server-provided coverage for the complete set of recorded attempts in this run. */
export type MetricAggregate = SessionModelStatistics['usage']['inputTokens']

/** Stable request outcomes remain distinct from protocol completion and usage availability. */
export const outcomeLabels: Record<SessionModelAttempt['outcome'], string> = {
  started: '待完成观测',
  succeeded: '成功',
  failed: '失败',
  cancelled: '已取消',
  interrupted: '已中断',
  incomplete: '未完整结束',
}

/** Rejects invalid measurements while preserving a reported zero. */
export function knownValue(value: number | null | undefined): number | null {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null
}

/** Formats known counters without making absent values look like measured zero. */
export function metricNumber(value: number | null | undefined): string {
  const known = knownValue(value)
  return known === null ? '未知' : known.toLocaleString('zh-CN', { maximumFractionDigits: 1 })
}

/** Formats gateway-observed output speed after first output, never model-internal decode timing. */
export function metricDecodeRate(attempt: SessionModelAttempt): string {
  if (
    !attempt.streamed ||
    attempt.outcome !== 'succeeded' ||
    !attempt.terminalObserved ||
    !attempt.finishedAt ||
    attempt.usage.availability !== 'complete'
  ) {
    return '未知'
  }
  const output = knownValue(attempt.usage.outputTokens)
  const duration = knownValue(attempt.durationMs)
  const firstOutput = knownValue(attempt.firstOutputMs)
  if (output === null || duration === null || firstOutput === null) return '未知'
  if (![output, duration, firstOutput].every(Number.isSafeInteger) || duration <= firstOutput) {
    return '未知'
  }
  const rate = (output * 1000) / (duration - firstOutput)
  // A positive observation must not round down to a measured zero.
  if (rate > 0 && rate < 0.1) return '< 0.1'
  return metricNumber(rate)
}

/** Formats measured milliseconds, including a real zero and subsecond first output. */
export function metricDuration(value: number | null | undefined): string {
  const known = knownValue(value)
  if (known === null) return '未知'
  if (known < 1000) return `${metricNumber(known)}ms`
  return `${(known / 1000).toLocaleString('zh-CN', { maximumFractionDigits: 2 })}s`
}

/** Displays observation instants in the browser locale and leaves absent dates unknown. */
export function metricTime(value: string | null): string {
  if (!value) return '未知'
  const parsed = new Date(value)
  return Number.isFinite(parsed.getTime()) ? parsed.toLocaleString() : '未知'
}

/** Explains the provider's input accounting convention without guessing billable totals. */
export function inputDescription(value: string): string {
  if (value === 'includes_cache') return '输入包含缓存读取；缓存数不再加到输入。'
  if (value === 'excludes_cache') return '输入不含缓存读写；保留上游分别报告的数值。'
  return '输入是否包含缓存未知，或不同请求口径不一致；请查看逐请求口径。'
}

/** Identifies partial totals as sums of known observations, never complete historical usage. */
export function coverageLabel(metric: MetricAggregate): string {
  const ratio = `${metric.knownCount} / ${metric.totalCount} 次请求已知`
  if (metric.availability === 'unavailable') return `${ratio} · 未提供`
  if (metric.availability === 'partial') return `${ratio} · 部分数据或未终结快照`
  return `${ratio} · 已记录请求全部覆盖`
}
