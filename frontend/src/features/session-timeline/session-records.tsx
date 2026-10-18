import { useMemo, useState } from 'react'
import type { SessionCategory, SessionSnapshot } from '@/features/session-data'
import { Input } from '@/components/ui/input'
import { EventWorkspace } from './event-workspace'
import { categoryLabels, jsonText, matchesSearch } from './timeline-values'

type RecordFilter = SessionCategory | 'all'

function isRecordFilter(value: string): value is RecordFilter {
  return value === 'all' || Object.hasOwn(categoryLabels, value)
}

function RecordSession({ model }: { model: SessionSnapshot }) {
  const [category, setCategory] = useState<RecordFilter>('all')
  const [keyword, setKeyword] = useState('')
  const events = useMemo(
    () =>
      model.events.filter(
        (event) =>
          (category === 'all' || event.category === category) && matchesSearch(event, keyword),
      ),
    [model.events, category, keyword],
  )
  return (
    <section aria-label="全部记录" className="space-y-4">
      <div className="space-y-1">
        <h2 className="text-sm font-medium">全部记录</h2>
        <p className="text-xs text-muted-foreground">
          按序号保留最后版本，共 {model.events.length} 条；原始输入 {model.rawRecords.length}{' '}
          条。时间缺失的记录仍保留。
        </p>
      </div>
      <div className="flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-2 text-xs">
          记录类型
          <select
            value={category}
            className="h-8 rounded-md border bg-background px-2 text-sm"
            onChange={(event) => {
              const value = event.target.value
              if (isRecordFilter(value)) setCategory(value)
            }}
          >
            <option value="all">所有类型</option>
            {Object.entries(categoryLabels).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </label>
        <Input
          aria-label="搜索全部记录"
          placeholder="搜索完整记录内容或 ID"
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
          className="w-full sm:max-w-96"
        />
      </div>
      <EventWorkspace events={events} allEvents={model.events} mode="records" />
      <details className="rounded-lg border p-3">
        <summary className="cursor-pointer text-xs font-medium">
          查看重放前的全部原始记录（含被替代版本）
        </summary>
        <pre
          aria-label="重放前原始记录 JSON"
          className="mt-3 max-h-[60vh] overflow-auto rounded bg-muted/50 p-3 text-xs whitespace-pre-wrap"
        >
          {jsonText(model.rawRecords)}
        </pre>
      </details>
    </section>
  )
}

/** Exposes every normalized category and the complete original input, without a timestamp filter. */
export function SessionRecords({ model }: { model: SessionSnapshot }) {
  return <RecordSession key={model.runId} model={model} />
}
