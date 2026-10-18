import { useMemo, useState } from 'react'
import type { SessionModelAttempt } from '@/api/generated.schemas'
import { knownValue, metricNumber, outcomeLabels, usageFields } from './metric-values'
import type { UsageField } from './metric-values'

interface TrendPoint {
  id: string
  ordinal: number
  outcome: SessionModelAttempt['outcome']
  value: number | null
  x: number
  y: number | null
}

function trendPoints(attempts: SessionModelAttempt[], field: UsageField, offset: number) {
  const values = attempts.map((attempt) => knownValue(attempt.usage[field]))
  const max = Math.max(1, ...values.map((value) => value ?? 0))
  const points = attempts.map((attempt, index): TrendPoint => {
    const value = values[index] ?? null
    return {
      id: attempt.id,
      ordinal: offset + index + 1,
      outcome: attempt.outcome,
      value,
      x: 50 + (index * 700) / Math.max(1, attempts.length - 1),
      y: value === null ? null : 165 - (value / max) * 135,
    }
  })
  return { points, max }
}

function TrendPlot({
  points,
  max,
  label,
  color,
}: {
  points: TrendPoint[]
  max: number
  label: string
  color: string
}) {
  const hasKnown = points.some((point) => point.value !== null)
  const hasMissing = points.some((point) => point.value === null)
  return (
    <svg
      viewBox="0 0 800 235"
      className="w-full min-w-[500px]"
      role="img"
      aria-label={`${label}每请求趋势`}
    >
      <title>
        {label}
        每请求趋势；圆点为已报告值，零值位于基线。虚线跨过未报告请求，仅连接已知观测，非插值。
      </title>
      {hasKnown && (
        <g>
          <path d="M50 20V165H760" fill="none" stroke="currentColor" className="text-border" />
          <text x="44" y="33" textAnchor="end" fontSize="11" fill="currentColor">
            {metricNumber(max)}
          </text>
          <text x="44" y="169" textAnchor="end" fontSize="11" fill="currentColor">
            0
          </text>
        </g>
      )}
      {hasMissing && (
        <g className="text-muted-foreground">
          <rect x="45" y="183" width="715" height="22" rx="4" fill="currentColor" opacity="0.06" />
          <text x="40" y="198" textAnchor="end" fontSize="10" fill="currentColor">
            未报告
          </text>
        </g>
      )}
      <TrendObservations points={points} color={color} label={label} />
      <text x="50" y="226" fontSize="11" fill="currentColor">
        #{points[0]?.ordinal ?? '—'}
      </text>
      <text x="750" y="226" textAnchor="end" fontSize="11" fill="currentColor">
        #{points.at(-1)?.ordinal ?? '—'}
      </text>
    </svg>
  )
}

function TrendObservations({
  points,
  color,
  label,
}: {
  points: TrendPoint[]
  color: string
  label: string
}) {
  const known = points.filter(
    (point): point is TrendPoint & { value: number; y: number } =>
      point.y !== null && point.value !== null,
  )
  return (
    <>
      {known.map((point, index) => {
        const previous = known[index - 1]
        const missing = previous ? point.ordinal - previous.ordinal - 1 : 0
        return (
          <g key={point.id}>
            {previous && (
              <line
                x1={previous.x}
                y1={previous.y}
                x2={point.x}
                y2={point.y}
                stroke={color}
                strokeWidth="2"
                strokeDasharray={missing ? '6 5' : undefined}
              >
                <title>
                  请求 #{previous.ordinal} → #{point.ordinal} ·{' '}
                  {missing ? `跨 ${missing} 次未报告用量，仅连接已知观测，非插值` : '相邻已知观测'}
                </title>
              </line>
            )}
            <circle cx={point.x} cy={point.y} r="4" fill={color}>
              <title>
                请求 #{point.ordinal} · {metricNumber(point.value)} · {point.id}
              </title>
            </circle>
          </g>
        )
      })}
      {points
        .filter((point) => point.value === null)
        .map((point) => (
          <path
            key={point.id}
            d={`M${point.x - 3} 191l6 6m-6 0l6 -6`}
            stroke="currentColor"
            strokeWidth="1.5"
            className="text-muted-foreground"
            aria-label={`请求 #${point.ordinal} 未报告${label}，不代表零 · ${outcomeLabels[point.outcome]}`}
          >
            <title>
              请求 #{point.ordinal} · {outcomeLabels[point.outcome]} · 未报告{label}，不代表零 ·{' '}
              {point.id}
            </title>
          </path>
        ))}
    </>
  )
}

/** Shows observed counters at every request position; dashed gaps never estimate missing usage. */
export function RequestTrend({
  attempts,
  offset,
}: {
  attempts: SessionModelAttempt[]
  offset: number
}) {
  const [field, setField] = useState<UsageField>('inputTokens')
  const selected = usageFields.find((item) => item.key === field) ?? usageFields[0]
  const { points, max } = useMemo(
    () => trendPoints(attempts, field, offset),
    [attempts, field, offset],
  )
  const known = points.filter((point) => point.value !== null).length
  return (
    <section aria-label="每请求 Token 趋势" className="space-y-3 rounded-xl border p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h3 className="text-sm font-medium">每请求 Token 趋势 · 当前页</h3>
        <label className="flex items-center gap-2 text-xs">
          统计字段
          <select
            value={field}
            className="h-8 rounded border bg-background px-2"
            onChange={(event) => {
              const next = usageFields.find((item) => item.key === event.target.value)
              if (next) setField(next.key)
            }}
          >
            {usageFields.map((item) => (
              <option key={item.key} value={item.key}>
                {item.label}
              </option>
            ))}
          </select>
        </label>
      </div>
      <p className="text-xs text-muted-foreground">
        按请求开始顺序展示，非累计曲线。当前页 {known} / {attempts.length}{' '}
        次请求有此字段；未报告不代表零。
      </p>
      <ul
        aria-label="趋势图例"
        className="flex flex-wrap gap-x-4 gap-y-2 text-xs text-muted-foreground"
      >
        <li className="flex items-center gap-2">
          <span
            className="w-5 border-t-2"
            style={{ borderColor: selected.color }}
            aria-hidden="true"
          />
          实线：相邻请求均已报告
        </li>
        <li className="flex items-center gap-2">
          <span
            className="w-5 border-t-2 border-dashed"
            style={{ borderColor: selected.color }}
            aria-hidden="true"
          />
          虚线：跨未报告请求，仅连接已知观测，非插值
        </li>
        <li>×：未报告，单独标记于数值轴下方</li>
      </ul>
      {!known && (
        <p className="text-center text-sm text-muted-foreground">当前页没有此字段的已知值。</p>
      )}
      <div className="overflow-x-auto">
        <TrendPlot points={points} max={max} label={selected.label} color={selected.color} />
      </div>
    </section>
  )
}
