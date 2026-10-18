import { Fragment, useState } from 'react'
import { Button } from '@/components/ui/button'
import type { SessionEvent } from '@/features/session-data'
import {
  categoryLabels,
  clockLabel,
  durationLabel,
  eventColor,
  eventTitle,
} from './timeline-values'

type ListProps = {
  events: readonly SessionEvent[]
  selectedId: string | null
  onSelect: (id: string) => void
  mode: 'timeline' | 'records'
}

const depths: Partial<Record<SessionEvent['category'], number>> = { user: 0, assistant: 1, tool: 2 }

function EventRow({
  event,
  selectedId,
  onSelect,
}: Omit<ListProps, 'events' | 'mode'> & { event: SessionEvent }) {
  const depth = depths[event.category] ?? 0
  return (
    <tr
      data-event-id={event.id}
      className={selectedId === event.id ? 'bg-accent' : 'hover:bg-muted/50'}
    >
      <td className="max-w-0 py-2 pr-2" style={{ paddingLeft: 12 + depth * 12 }}>
        <button
          type="button"
          onClick={() => onSelect(event.id)}
          aria-pressed={selectedId === event.id}
          className="flex w-full items-center gap-2 rounded text-left outline-none focus-visible:ring-2 focus-visible:ring-ring"
          title={eventTitle(event)}
        >
          <span
            className="size-2 shrink-0 rounded-full"
            style={{ backgroundColor: eventColor(event) }}
          />
          <span className="truncate">{eventTitle(event)}</span>
        </button>
      </td>
      <td className="p-2 text-muted-foreground">{categoryLabels[event.category]}</td>
      <td className="p-2">
        {event.tool?.isError && <span className="mr-1 text-destructive">失败</span>}
        {event.tool?.status ?? '—'}
      </td>
      <td className="p-2 tabular-nums">{durationLabel(event.tool?.durationMs ?? null)}</td>
      <td className="p-2 tabular-nums">
        {clockLabel(event.tool?.startedAtMs ?? event.timestampMs)}
      </td>
    </tr>
  )
}

/** Bounded rendering keeps all filtered events reachable; row and detail selection use event IDs. */
export function EventList({ events, selectedId, onSelect, mode }: ListProps) {
  const [limit, setLimit] = useState(200)
  const shown = events.slice(0, limit)
  return (
    <div className="min-w-0 space-y-2">
      <div className="max-h-[65vh] overflow-auto rounded-lg border">
        <table
          aria-label={mode === 'timeline' ? '时间轴事件' : '全部会话事件'}
          className="w-full min-w-[600px] table-fixed text-xs"
        >
          <colgroup>
            <col className="w-[44%]" />
            <col className="w-[12%]" />
            <col className="w-[16%]" />
            <col className="w-[12%]" />
            <col className="w-[16%]" />
          </colgroup>
          <thead className="sticky top-0 z-10 bg-muted text-left text-muted-foreground">
            <tr>
              <th className="p-3">名称</th>
              <th className="p-2">类型</th>
              <th className="p-2">状态</th>
              <th className="p-2">耗时</th>
              <th className="p-2">时间</th>
            </tr>
          </thead>
          <tbody className="divide-y">
            {shown.map((event, index) => (
              <Fragment key={event.id}>
                {mode === 'timeline' &&
                  (index === 0 || shown[index - 1]?.turn?.id !== event.turn?.id) && (
                    <tr className="bg-muted/40">
                      <td colSpan={5} className="px-3 py-2 font-medium text-muted-foreground">
                        {event.turn ? `Turn ${event.turn.ordinal}` : '轮次未知'}
                      </td>
                    </tr>
                  )}
                <EventRow event={event} selectedId={selectedId} onSelect={onSelect} />
              </Fragment>
            ))}
          </tbody>
        </table>
        {events.length === 0 && (
          <p className="p-6 text-center text-sm text-muted-foreground">没有匹配的事件。</p>
        )}
      </div>
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span aria-live="polite">
          显示 {shown.length} / {events.length} 条
        </span>
        {shown.length < events.length && (
          <Button variant="outline" size="sm" onClick={() => setLimit((value) => value + 200)}>
            显示更多记录
          </Button>
        )}
      </div>
    </div>
  )
}
