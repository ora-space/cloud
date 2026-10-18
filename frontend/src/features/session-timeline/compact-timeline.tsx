import type { SessionTimelineEvent } from '@/features/session-data'
import {
  clockLabel,
  durationLabel,
  eventColor,
  eventTitle,
  intersectsWindow,
  lanes,
} from './timeline-values'
import type { TimeWindow } from './use-time-brush'

type PositionedEvent = {
  event: SessionTimelineEvent
  startMs: number
  sourceIndex: number
}

function orderedEvents(events: readonly SessionTimelineEvent[]): PositionedEvent[] {
  return events
    .flatMap<PositionedEvent>((event, sourceIndex) => {
      if (event.startMs === null || !Number.isFinite(event.startMs)) return []
      return [{ event, startMs: event.startMs, sourceIndex }]
    })
    .toSorted((left, right) => left.startMs - right.startMs || left.sourceIndex - right.sourceIndex)
}

function CompactMark({
  positioned,
  brush,
}: {
  positioned: PositionedEvent
  brush: TimeWindow | null
}) {
  const { event, startMs } = positioned
  const title = eventTitle(event)
  const duration = durationLabel(event.durationMs)
  const outside = brush !== null && !intersectsWindow(event, brush)
  return (
    <span
      role="img"
      aria-label={`${title} · ${duration}`}
      title={`${title} · ${clockLabel(startMs, 'milliseconds')} · ${duration}`}
      className={`flex h-12 w-14 shrink-0 flex-col justify-center rounded px-1 text-white ${outside ? 'opacity-25' : ''} ${event.tool?.isError ? 'ring-2 ring-destructive' : ''}`}
      style={{ backgroundColor: eventColor(event) }}
    >
      <span className="truncate text-[11px] leading-4">{title}</span>
      <span className="truncate text-[10px] leading-4 tabular-nums">{duration}</span>
    </span>
  )
}

function EventColumn({
  positioned,
  ordinal,
  brush,
}: {
  positioned: PositionedEvent
  ordinal: number
  brush: TimeWindow | null
}) {
  return (
    <div className="min-w-0 border-r last:border-r-0">
      <div className="flex h-10 flex-col items-center justify-center border-b text-[10px] text-muted-foreground tabular-nums">
        <span>#{ordinal}</span>
        <span>{clockLabel(positioned.startMs)}</span>
      </div>
      {lanes.map((lane) => (
        <div
          key={lane.kind}
          className="flex h-16 items-center justify-center border-b px-1 even:bg-muted/20 last:border-b-0"
        >
          {positioned.event.kind === lane.kind && (
            <CompactMark positioned={positioned} brush={brush} />
          )}
        </div>
      ))}
    </div>
  )
}

/** Places known-time events in stable chronological columns; equal widths never imply duration. */
export function CompactTimeline({
  events,
  brush,
}: {
  events: readonly SessionTimelineEvent[]
  brush: TimeWindow | null
}) {
  const positioned = orderedEvents(events)
  if (positioned.length === 0) {
    return <p className="text-sm text-muted-foreground">没有可定位的时间戳，事件仍可在下方查看。</p>
  }
  return (
    <div className="space-y-2">
      <div
        role="group"
        aria-label="紧凑事件时间轴"
        className="grid grid-cols-[4.5rem_minmax(0,1fr)] gap-x-2"
      >
        <div className="text-xs text-muted-foreground">
          <div className="h-10" />
          {lanes.map((lane) => (
            <span key={lane.kind} className="flex h-16 items-center">
              {lane.label}
            </span>
          ))}
        </div>
        <div className="min-w-0 overflow-x-auto rounded border">
          <div
            className="grid"
            style={{ gridTemplateColumns: `repeat(${positioned.length}, minmax(64px, 1fr))` }}
          >
            {positioned.map((item, index) => (
              <EventColumn
                key={item.event.id}
                positioned={item}
                ordinal={index + 1}
                brush={brush}
              />
            ))}
          </div>
        </div>
      </div>
      <p className="text-xs text-muted-foreground">
        按真实开始时间等距排列，块宽不表示耗时；框选范围外事件淡化，未知时间事件保留在列表中。
      </p>
    </div>
  )
}
