import type { SessionSnapshot } from '@/features/session-data'
import { durationLabel } from './timeline-values'

/** Groups observed tool records by ACP kind; unknown measurements never inflate duration totals. */
export function ToolSummary({ model }: { model: SessionSnapshot }) {
  return (
    <section aria-label="工具统计" className="space-y-3">
      <h2 className="text-sm font-medium">工具统计</h2>
      <p className="text-xs text-muted-foreground">
        按 ACP 工具类别汇总记录，不推断原生工具名。平均耗时仅使用有计时的记录，包含真实的 0ms。
      </p>
      <div className="overflow-auto rounded-lg border">
        <table className="w-full min-w-[650px] text-left text-xs">
          <thead className="bg-muted text-muted-foreground">
            <tr>
              {[
                '工具类别',
                '记录数',
                '完成',
                '失败',
                '未完成',
                '未知状态',
                '有计时',
                '平均耗时',
                '最长耗时',
              ].map((name) => (
                <th key={name} className="p-3 font-medium">
                  {name}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y">
            {model.tools.map((tool) => (
              <tr key={tool.kind}>
                <td className="p-3 font-medium">{tool.kind}</td>
                <td className="p-3">{tool.count}</td>
                <td className="p-3">{tool.completedCount}</td>
                <td className="p-3">{tool.failedCount}</td>
                <td className="p-3">{tool.incompleteCount}</td>
                <td className="p-3">{tool.unknownStatusCount}</td>
                <td className="p-3">
                  {tool.durationKnownCount} / {tool.count}
                </td>
                <td className="p-3 tabular-nums">{durationLabel(tool.averageDurationMs)}</td>
                <td className="p-3 tabular-nums">{durationLabel(tool.maxDurationMs)}</td>
              </tr>
            ))}
          </tbody>
        </table>
        {model.tools.length === 0 && (
          <p className="p-6 text-center text-sm text-muted-foreground">
            已加载记录中没有工具事件。
          </p>
        )}
      </div>
    </section>
  )
}
