import type { SessionEvent } from '@/features/session-data'
import { Button } from '@/components/ui/button'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { adjacentGap, categoryLabels, durationLabel, eventTitle, jsonText } from './timeline-values'

type DetailsProps = {
  event: SessionEvent
  previous: SessionEvent | undefined
  onClose: () => void
}

function Values({ rows }: { rows: readonly (readonly [string, string])[] }) {
  return (
    <dl className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-4 gap-y-2 text-xs">
      {rows.map(([label, value]) => (
        <div key={label} className="contents">
          <dt className="text-muted-foreground">{label}</dt>
          <dd className="min-w-0 break-words whitespace-pre-wrap">{value}</dd>
        </div>
      ))}
    </dl>
  )
}

function ContentBlock({ title, value }: { title: string; value: string }) {
  return (
    <section className="space-y-2">
      <h4 className="text-xs font-medium">{title}</h4>
      <pre className="max-h-96 overflow-auto rounded-md bg-muted/50 p-3 text-xs break-words whitespace-pre-wrap">
        {value || '（空内容）'}
      </pre>
    </section>
  )
}

function Summary({ event }: { event: SessionEvent }) {
  const tool = event.tool
  return (
    <div className="space-y-4">
      <Values
        rows={[
          ['类型', categoryLabels[event.category]],
          ['记录 ID', event.id],
          ['轮次', event.turn?.ordinal === undefined ? '未知' : String(event.turn.ordinal)],
          ['记录时间', event.timestamp ?? '未知'],
          ['模型绑定', event.model?.id ?? '未知'],
          ['Tokens', '未知'],
          ['费用', '未知'],
        ]}
      />
      {event.text && <ContentBlock title="记录内容" value={event.text} />}
      {tool && (
        <Values
          rows={[
            ['工具类别', tool.kind ?? '未知'],
            ['原始操作标题', tool.title ?? '未知'],
            ['原生工具名', tool.nativeName ?? '未知'],
            ['Tool ID', tool.id ?? '未知'],
            ['状态', tool.status ?? '未知'],
            ['实际耗时', durationLabel(tool.durationMs)],
          ]}
        />
      )}
      {!event.text && !tool && <ContentBlock title="记录内容" value={jsonText(event.record)} />}
    </div>
  )
}

function payload(event: SessionEvent): string {
  if (event.tool)
    return event.tool.rawInputPresent ? jsonText(event.tool.rawInput) : '未记录工具请求'
  if (event.category === 'user' || event.category === 'user_echo')
    return event.text ?? '未记录输入文本'
  return jsonText(event.record)
}

function result(event: SessionEvent): string {
  if (event.tool?.rawOutputPresent) return jsonText(event.tool.rawOutput)
  if (event.tool) return event.tool.output ?? '未记录工具结果'
  if (event.category === 'assistant' || event.category === 'thought')
    return event.text ?? '未记录输出文本'
  return '该记录没有独立输出；原始内容见 JSON。'
}

function Timing({ event, previous }: Omit<DetailsProps, 'onClose'>) {
  return (
    <div className="space-y-4">
      <Values
        rows={[
          ['记录时间', event.timestamp ?? '未知'],
          ['工具开始', event.tool?.startedAt ?? '未知'],
          ['工具实际耗时', durationLabel(event.tool?.durationMs ?? null)],
          ['模型 API 延迟', durationLabel(event.latencyMs)],
          ['与前一记录间隔', durationLabel(adjacentGap(event, previous))],
        ]}
      />
      <p className="text-xs leading-relaxed text-muted-foreground">
        记录时间是 Ora 观察或落盘时间。相邻记录间隔不代表模型请求耗时；缺失或倒序的时间不会补成
        0ms。
      </p>
    </div>
  )
}

/** Read-only inspector retains the original dashboard's five tabs and never estimates absent usage. */
export function EventDetails({ event, previous, onClose }: DetailsProps) {
  return (
    <aside
      aria-label="事件详情"
      className="min-w-0 rounded-lg border bg-background lg:sticky lg:top-0 lg:self-start"
    >
      <header className="flex items-start justify-between gap-2 border-b p-3">
        <h3 className="min-w-0 text-sm font-medium break-words">{eventTitle(event)}</h3>
        <Button variant="ghost" size="xs" onClick={onClose} aria-label="关闭事件详情">
          关闭
        </Button>
      </header>
      <Tabs defaultValue="summary" className="p-3">
        <TabsList className="w-full">
          <TabsTrigger value="summary">摘要</TabsTrigger>
          <TabsTrigger value="payload">Payload</TabsTrigger>
          <TabsTrigger value="result">Result</TabsTrigger>
          <TabsTrigger value="timing">Timing</TabsTrigger>
          <TabsTrigger value="json">JSON</TabsTrigger>
        </TabsList>
        <TabsContent value="summary">
          <Summary event={event} />
        </TabsContent>
        <TabsContent value="payload">
          <ContentBlock title="请求 / 记录负载" value={payload(event)} />
        </TabsContent>
        <TabsContent value="result">
          <ContentBlock title="结果" value={result(event)} />
        </TabsContent>
        <TabsContent value="timing">
          <Timing event={event} previous={previous} />
        </TabsContent>
        <TabsContent value="json">
          <ContentBlock title="完整原始记录" value={jsonText(event.raw)} />
        </TabsContent>
      </Tabs>
    </aside>
  )
}
