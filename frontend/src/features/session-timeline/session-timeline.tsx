import { useMemo, useState } from 'react'
import type { SessionSnapshot, SessionTimelineEvent } from '@/features/session-data'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { EventWorkspace } from './event-workspace'
import { TimelineStrip } from './timeline-strip'
import {
  clockLabel,
  durationLabel,
  intersectsWindow,
  lanes,
  matchesSearch,
} from './timeline-values'
import type { TimeWindow } from './use-time-brush'

function TimelineSession({ model }: { model: SessionSnapshot }) {
  const [kinds, setKinds] = useState<Set<SessionTimelineEvent['kind']>>(
    new Set(['user', 'llm', 'tool']),
  )
  const [keyword, setKeyword] = useState('')
  const [brush, setBrush] = useState<TimeWindow | null>(null)
  const events = useMemo(
    () =>
      model.timeline.events.filter((event) => {
        const withinTime = !brush || intersectsWindow(event, brush)
        return kinds.has(event.kind) && withinTime && matchesSearch(event, keyword)
      }),
    [model.timeline.events, kinds, keyword, brush],
  )
  const toggle = (kind: SessionTimelineEvent['kind']) =>
    setKinds((current) => {
      const next = new Set(current)
      if (next.has(kind)) next.delete(kind)
      else next.add(kind)
      return next
    })
  return (
    <section aria-label="会话时间轴" className="space-y-4">
      <div className="space-y-1">
        <h2 className="text-sm font-medium">消息时间轴</h2>
        <p className="text-xs text-muted-foreground">
          已加载 {model.timeline.events.length} 条核心事件 · 观测时间跨度{' '}
          {durationLabel(model.timeline.observedSpanMs)} · 输入 {model.stats.userMessageCount} ·
          模型文本 {model.stats.assistantTextCount} · 工具记录 {model.stats.toolCount}
        </p>
      </div>
      <TimelineStrip model={model} brush={brush} onBrush={setBrush} />
      <div className="flex flex-wrap items-center gap-2">
        <div role="group" aria-label="时间轴类型筛选" className="flex gap-1">
          {lanes.map((lane) => (
            <Button
              key={lane.kind}
              size="sm"
              variant={kinds.has(lane.kind) ? 'secondary' : 'outline'}
              aria-pressed={kinds.has(lane.kind)}
              onClick={() => toggle(lane.kind)}
            >
              {lane.label}
            </Button>
          ))}
        </div>
        <Input
          aria-label="搜索时间轴事件"
          placeholder="搜索名称、工具、内容或 ID"
          value={keyword}
          onChange={(event) => setKeyword(event.target.value)}
          className="w-full sm:max-w-80"
        />
        {brush && (
          <Button size="sm" variant="outline" onClick={() => setBrush(null)}>
            清除时间选择（{clockLabel(brush.startMs)} – {clockLabel(brush.endMs)}）
          </Button>
        )}
      </div>
      <p className="text-xs text-muted-foreground">
        三泳道展示输入、模型文本和工具；思考、元数据、生命周期与未知类型请在「全部记录」中查看。
      </p>
      <EventWorkspace events={events} allEvents={model.timeline.events} mode="timeline" />
    </section>
  )
}

/** Presents one normalized run with the original dashboard's lanes, brush, filters and inspector. */
export function SessionTimeline({ model }: { model: SessionSnapshot }) {
  return <TimelineSession key={model.runId} model={model} />
}
