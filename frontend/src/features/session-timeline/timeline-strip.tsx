import { useState } from 'react'
import type { SessionSnapshot, SessionTimelineEvent } from '@/features/session-data'
import {
  clockLabel,
  durationLabel,
  eventColor,
  eventTitle,
  intersectsWindow,
  lanes,
} from './timeline-values'
import { useTimeBrush, type TimeWindow } from './use-time-brush'
import { CompactTimeline } from './compact-timeline'
import { TimelineControls, type TimelineLayout } from './timeline-controls'

type StripProps = {
  model: SessionSnapshot
  brush: TimeWindow | null
  onBrush: (value: TimeWindow | null) => void
}

function percentage(value: number, bounds: TimeWindow): number {
  return Math.max(
    0,
    Math.min(100, ((value - bounds.startMs) / Math.max(1, bounds.endMs - bounds.startMs)) * 100),
  )
}

function Mark({ event, bounds }: { event: SessionTimelineEvent; bounds: TimeWindow }) {
  if (event.startMs === null || !intersectsWindow(event, bounds)) return null
  const left = percentage(event.startMs, bounds)
  const end = percentage(event.startMs + (event.durationMs ?? 0), bounds)
  return (
    <span
      role="img"
      aria-label={`${eventTitle(event)} · ${durationLabel(event.durationMs)}`}
      title={`${eventTitle(event)} · ${clockLabel(event.startMs, 'milliseconds')} · ${durationLabel(event.durationMs)}`}
      className={`absolute top-2 bottom-2 min-w-[3px] rounded-sm ${event.tool?.isError ? 'ring-2 ring-destructive' : ''}`}
      style={{
        left: `min(${left}%, calc(100% - 3px))`,
        width: `${Math.max(0, end - left)}%`,
        backgroundColor: eventColor(event),
      }}
    />
  )
}

function KeyboardRange({
  bounds,
  brush,
  onBrush,
}: Omit<StripProps, 'model'> & { bounds: TimeWindow }) {
  const selected = brush ?? bounds
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-xs text-muted-foreground">
      {(['startMs', 'endMs'] as const).map((edge) => (
        <label key={edge} className="flex items-center gap-2">
          {edge === 'startMs' ? '框选起点' : '框选终点'}
          <input
            type="range"
            min={bounds.startMs}
            max={bounds.endMs}
            step="any"
            value={selected[edge]}
            className="w-28 accent-primary"
            onChange={(event) => {
              const value = Number(event.target.value)
              const next =
                edge === 'startMs'
                  ? { startMs: Math.min(value, selected.endMs), endMs: selected.endMs }
                  : { startMs: selected.startMs, endMs: Math.max(value, selected.startMs) }
              onBrush(next)
            }}
          />
        </label>
      ))}
    </div>
  )
}

function StripChart({ model, bounds, brush, onBrush }: StripProps & { bounds: TimeWindow }) {
  const { draft, ...handlers } = useTimeBrush(bounds, onBrush)
  const selection = draft ?? brush
  const precision = bounds.endMs - bounds.startMs < 10000 ? 'milliseconds' : 'seconds'
  return (
    <div className="space-y-3">
      <div className="grid grid-cols-[4.5rem_minmax(0,1fr)] gap-x-2">
        <div />
        <div className="relative h-6 text-[10px] text-muted-foreground">
          {[0, 0.25, 0.5, 0.75, 1].map((fraction) => (
            <span
              key={fraction}
              className="absolute whitespace-nowrap"
              style={{ left: `${fraction * 100}%`, transform: `translateX(-${fraction * 100}%)` }}
            >
              {clockLabel(bounds.startMs + fraction * (bounds.endMs - bounds.startMs), precision)}
            </span>
          ))}
        </div>
        <div className="grid grid-rows-3 text-xs text-muted-foreground">
          {lanes.map((lane) => (
            <span key={lane.kind} className="flex h-10 items-center">
              {lane.label}
            </span>
          ))}
        </div>
        <div
          role="group"
          aria-label="时间轴框选区域"
          tabIndex={0}
          {...handlers}
          onKeyDown={(event) => {
            if (event.key === 'Escape') onBrush(null)
          }}
          className="relative touch-none cursor-crosshair rounded border outline-none select-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {lanes.map((lane) => (
            <div key={lane.kind} className="relative h-10 border-b last:border-b-0">
              {model.timeline.events
                .filter((event) => event.kind === lane.kind)
                .map((event) => (
                  <Mark key={event.id} event={event} bounds={bounds} />
                ))}
            </div>
          ))}
          {selection && (
            <div
              aria-hidden
              className="pointer-events-none absolute inset-y-0 border-x border-primary bg-primary/15"
              style={{
                left: `${percentage(selection.startMs, bounds)}%`,
                width: `${percentage(selection.endMs, bounds) - percentage(selection.startMs, bounds)}%`,
              }}
            />
          )}
        </div>
      </div>
      <KeyboardRange bounds={bounds} brush={brush} onBrush={onBrush} />
    </div>
  )
}

function viewportBounds(full: TimeWindow, viewport: TimeWindow | null): TimeWindow {
  if (!viewport) return full
  const startMs = Math.max(full.startMs, viewport.startMs)
  const endMs = Math.min(full.endMs, viewport.endMs)
  return startMs < endMs ? { startMs, endMs } : full
}

/** Offers compact event order and a zoomable real-time view without fabricating duration. */
export function TimelineStrip({ model, brush, onBrush }: StripProps) {
  const [layout, setLayout] = useState<TimelineLayout>('compact')
  const [viewport, setViewport] = useState<TimeWindow | null>(null)
  const startMs = model.timeline.minTimestampMs
  const endMs = model.timeline.maxTimestampMs
  const unknown = model.timeline.events.filter((event) => event.startMs === null).length
  const bounds = startMs === null || endMs === null ? null : { startMs, endMs }
  const zoom = () => {
    if (brush && brush.startMs < brush.endMs) setViewport(brush)
  }
  return (
    <section aria-label="三泳道时间轴" className="space-y-3 rounded-lg border bg-card p-4">
      {!bounds ? (
        <p className="text-sm text-muted-foreground">没有可定位的时间戳，事件仍可在下方查看。</p>
      ) : (
        <>
          <TimelineControls
            layout={layout}
            onLayout={setLayout}
            canZoom={brush !== null && brush.startMs < brush.endMs}
            onZoom={zoom}
            isZoomed={viewport !== null}
            onReset={() => {
              setViewport(null)
              onBrush(null)
            }}
          />
          {layout === 'compact' ? (
            <CompactTimeline events={model.timeline.events} brush={brush} />
          ) : (
            <>
              <StripChart
                model={model}
                bounds={viewportBounds(bounds, viewport)}
                brush={brush}
                onBrush={onBrush}
              />
              <p className="text-xs leading-relaxed text-muted-foreground">
                横轴按真实时间比例显示，长间隔会保留空白。拖拽或用滑块框选后可放大选区；单击空白或按
                Esc 清除框选。
              </p>
            </>
          )}
        </>
      )}
      <p className="text-xs leading-relaxed text-muted-foreground">
        时间未知的 {unknown} 条记录保留在列表中，不在泳道上补造位置。
      </p>
    </section>
  )
}
