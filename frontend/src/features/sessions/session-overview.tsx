import type { IssueRun } from '@/api/generated.schemas'
import type { SessionSnapshot } from '@/features/session-data'
import { sessionStatuses, sessionTime } from './presentation'

/** Overview reports loaded observations; unavailable billing and latency are explicit. */
export function SessionOverview({ model, run }: { model: SessionSnapshot; run: IssueRun }) {
  const cards = [
    ['事件记录', String(model.stats.recordCount)],
    ['工具记录', String(model.stats.toolCount)],
    ['模型文本', String(model.stats.assistantTextCount)],
    ['思考记录', String(model.stats.thoughtCount)],
  ]
  return (
    <section aria-label="会话概览" className="space-y-5">
      <div className="grid grid-cols-2 gap-3 xl:grid-cols-4">
        {cards.map(([label, value]) => (
          <div key={label} className="rounded-xl border bg-card p-4">
            <p className="text-xs text-muted-foreground">{label}</p>
            <p className="mt-2 text-2xl font-semibold tabular-nums">{value}</p>
          </div>
        ))}
      </div>
      <dl className="grid gap-4 rounded-xl border p-4 text-sm md:grid-cols-2">
        <div>
          <dt className="text-muted-foreground">运行状态</dt>
          <dd>{sessionStatuses[run.status]}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">运行时固定模型</dt>
          <dd className="break-all">{model.frozenModel?.id ?? '未知'}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">创建时间</dt>
          <dd>{sessionTime(run.createdAt)}</dd>
        </div>
        <div>
          <dt className="text-muted-foreground">完成时间</dt>
          <dd>{sessionTime(run.completedAt)}</dd>
        </div>
      </dl>
      <div className="rounded-xl border border-dashed p-4 text-sm text-muted-foreground">
        <p>模型请求次数、Token 与请求耗时请查看「模型请求」；费用暂不可用。</p>
        <p className="mt-1 text-xs">仅展示 Gateway 已记录请求的上游用量与覆盖率。</p>
        <p className="mt-1 text-xs">工具耗时只采用日志报告值；模型文本数量不代表 API 调用次数。</p>
      </div>
      {model.diagnostics.length > 0 && (
        <p role="status" className="text-xs text-muted-foreground">
          发现 {model.diagnostics.length}{' '}
          条未知类型、缺失时间或格式提示，原始内容保留在「完整事件」。
        </p>
      )}
    </section>
  )
}
