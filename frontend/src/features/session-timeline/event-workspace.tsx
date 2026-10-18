import type { SessionEvent } from '@/features/session-data'
import { EventDetails } from './event-details'
import { EventList } from './event-list'
import { useEventSelection } from './use-event-selection'

/** Shares the list/inspector layout while keeping hidden selections tied to the complete source. */
export function EventWorkspace({
  events,
  allEvents,
  mode,
}: {
  events: readonly SessionEvent[]
  allEvents: readonly SessionEvent[]
  mode: 'timeline' | 'records'
}) {
  const selection = useEventSelection(allEvents)
  const hidden = selection.selected && !events.some((event) => event.id === selection.selectedId)
  return (
    <div className="space-y-2">
      {hidden && (
        <p role="status" className="text-xs text-muted-foreground">
          当前选中事件被筛选隐藏；详情仍对应同一条记录。
        </p>
      )}
      <div
        className={
          selection.selected ? 'grid min-w-0 gap-4 xl:grid-cols-[minmax(0,1fr)_24rem]' : 'min-w-0'
        }
      >
        <EventList
          events={events}
          selectedId={selection.selectedId}
          onSelect={selection.select}
          mode={mode}
        />
        {selection.selected && (
          <EventDetails
            key={selection.selected.id}
            event={selection.selected}
            previous={selection.previous}
            onClose={selection.close}
          />
        )}
      </div>
    </div>
  )
}
