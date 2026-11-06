import { useEffect, useRef, useState } from 'react'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { ChevronDown, FileText } from 'lucide-react'
import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

/**
 * The run's evidence, rendered beside the decision list rather than above it: it is context,
 * not the work. Collapsed it keeps a few lines; the control only appears when there is more.
 */
export function RunReportPanel({ report, count }: { report: string; count: number }) {  const [expanded, setExpanded] = useState(false)
  const [overflowing, setOverflowing] = useState(false)
  const body = useRef<HTMLDivElement>(null)

  // scrollHeight is the full content height even while the collapsed box clips it; keep the
  // last answer once expanded so the control does not disappear under the reader's cursor.
  useEffect(() => {
    const element = body.current
    if (!element) return
    if (!expanded) setOverflowing(element.scrollHeight > element.clientHeight + 8)
  }, [report, expanded, count])

  return <aside className="min-w-0 lg:sticky lg:top-0" aria-label="巡检报告">
    <div className="overflow-hidden rounded-xl border bg-card">
      <div className="flex items-center gap-2 border-b bg-muted/25 px-4 py-2.5">
        <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-brand-soft text-brand"><FileText className="size-3.5" aria-hidden /></span>
        <div className="min-w-0 flex-1">
          <h3 className="text-sm font-semibold">报告</h3>
          <p className="truncate text-[11px] text-muted-foreground">本次运行的证据与结论</p>
        </div>
        {count > 0 && <Badge variant="outline" className="shrink-0 font-mono text-[10px] tabular-nums">{count} 项</Badge>}
      </div>
      <div className="relative">
        <div
          ref={body}
          className={cn(
            'px-4 py-3 text-sm leading-6',
            !expanded && 'max-h-40 overflow-hidden lg:max-h-52',
            '[&_h1]:mb-2 [&_h1]:text-base [&_h1]:font-semibold [&_h2]:my-2 [&_h2]:text-sm [&_h2]:font-semibold [&_h3]:my-1.5 [&_h3]:text-sm [&_h3]:font-medium',
            '[&_li]:my-0.5 [&_ol]:list-decimal [&_ol]:pl-5 [&_ul]:list-disc [&_ul]:pl-5 [&_p]:my-1.5 [&_pre]:overflow-x-auto [&_pre]:rounded-lg [&_pre]:bg-muted [&_pre]:p-2.5 [&_pre]:text-xs',
            '[&_table]:my-2 [&_table]:w-full [&_table]:border-collapse [&_table]:text-[11px] [&_td]:border [&_td]:px-1.5 [&_td]:py-1 [&_td]:align-top [&_th]:border [&_th]:bg-muted/50 [&_th]:px-1.5 [&_th]:py-1 [&_th]:text-left [&_td:first-child]:whitespace-nowrap [&_th:first-child]:whitespace-nowrap',
            '[&_a]:underline [&_a]:underline-offset-2 [&_hr]:my-3',
          )}
        >
          <ReactMarkdown remarkPlugins={[remarkGfm]}>{report}</ReactMarkdown>
        </div>
        {!expanded && overflowing && <div className="pointer-events-none absolute inset-x-0 bottom-0 h-12 bg-gradient-to-t from-card to-transparent" aria-hidden />}
      </div>
      {overflowing && <button
        type="button"
        aria-expanded={expanded}
        onClick={() => setExpanded(value => !value)}
        className="flex w-full items-center justify-center gap-1 border-t bg-muted/10 px-3 py-1.5 text-[11px] font-medium text-muted-foreground transition-colors hover:bg-muted/25 hover:text-foreground"
      >
        {expanded ? '收起报告' : '展开完整报告'}
        <ChevronDown className={cn('size-3.5 transition-transform', expanded && 'rotate-180')} aria-hidden />
      </button>}
    </div>
  </aside>
}
