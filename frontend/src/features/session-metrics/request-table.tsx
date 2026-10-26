import type { SessionModelAttempt } from '@/api/generated.schemas'
import { Badge } from '@/components/ui/badge'
import {
  metricDecodeRate,
  metricDuration,
  metricNumber,
  metricTime,
  outcomeLabels,
  usageFields,
} from './metric-values'

function usageStatus(value: string): string {
  if (value === 'complete') return '用量完整'
  if (value === 'partial') return '用量部分提供或尚未终结'
  return '用量未提供'
}

function inputLabel(value: string): string {
  if (value === 'includes_cache') return '输入含缓存'
  if (value === 'excludes_cache') return '输入不含缓存'
  return '输入口径未知'
}

function AttemptIdentity({ attempt, ordinal }: { attempt: SessionModelAttempt; ordinal: number }) {
  return (
    <div className="max-w-72 space-y-1">
      <p className="font-medium">
        #{ordinal} · {attempt.modelId || '模型未知'}
      </p>
      <p className="break-all font-mono text-[11px] text-muted-foreground">{attempt.id}</p>
      <p className="text-muted-foreground">
        {attempt.protocol} · {attempt.streamed ? '流式' : '非流式'}
      </p>
      <p className="text-muted-foreground">开始：{metricTime(attempt.startedAt)}</p>
      <p className="text-muted-foreground">结束：{metricTime(attempt.finishedAt)}</p>
    </div>
  )
}

function AttemptOutcome({ attempt }: { attempt: SessionModelAttempt }) {
  return (
    <div className="space-y-1">
      <Badge variant={attempt.outcome === 'failed' ? 'destructive' : 'secondary'}>
        {outcomeLabels[attempt.outcome]}
      </Badge>
      <p className="text-muted-foreground">HTTP {attempt.httpStatus ?? '未知'}</p>
      <p className="text-muted-foreground">
        {attempt.terminalObserved ? '已观测协议结束' : '未观测协议结束'}
      </p>
      <p className="text-muted-foreground">{usageStatus(attempt.usage.availability)}</p>
      <p className="text-muted-foreground">{inputLabel(attempt.usage.inputSemantics)}</p>
      {attempt.errorCode && <p className="break-all text-destructive">{attempt.errorCode}</p>}
    </div>
  )
}

/** Lists individual HTTP attempts; retries, incomplete streams and unknown counters remain visible. */
export function RequestTable({
  attempts,
  offset,
}: {
  attempts: SessionModelAttempt[]
  offset: number
}) {
  return (
    <section aria-label="模型请求明细" className="space-y-3">
      <h3 className="text-sm font-medium">请求明细 · 当前页 {attempts.length} 条</h3>
      <p className="text-xs text-muted-foreground">
        平均 Decode 速率（估算）= 输出 Token ÷（请求时长 − 首次有效输出耗时），单位 Token/s。
        仅统计成功结束且用量完整的流式请求；非流式、观测不完整、数据无效或时间差不为正时显示“未知”。
        网关观测包含网络与协议收尾耗时，不代表模型内部纯 Decode 性能。
      </p>
      <div className="overflow-x-auto rounded-xl border">
        <table className="w-full min-w-[1200px] text-left text-xs">
          <caption className="sr-only">
            模型请求明细，所有 Token 均为上游报告值，未知不计为零。
          </caption>
          <thead className="bg-muted text-muted-foreground">
            <tr>
              {[
                '请求',
                '状态与口径',
                '请求时长',
                '首次有效输出',
                '平均 Decode 速率（Token/s，估算）',
                ...usageFields.map((field) => field.label),
              ].map((label) => (
                <th key={label} className="p-3 font-medium">
                  {label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y">
            {attempts.map((attempt, index) => (
              <tr key={attempt.id} className="align-top">
                <td className="p-3">
                  <AttemptIdentity attempt={attempt} ordinal={offset + index + 1} />
                </td>
                <td className="p-3">
                  <AttemptOutcome attempt={attempt} />
                </td>
                <td className="p-3 tabular-nums">{metricDuration(attempt.durationMs)}</td>
                <td className="p-3 tabular-nums">{metricDuration(attempt.firstOutputMs)}</td>
                <td className="p-3 tabular-nums">{metricDecodeRate(attempt)}</td>
                {usageFields.map((field) => (
                  <td key={field.key} className="p-3 tabular-nums">
                    {metricNumber(attempt.usage[field.key])}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}
