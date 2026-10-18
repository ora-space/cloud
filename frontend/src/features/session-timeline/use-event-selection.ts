import { useState } from 'react'
import type { SessionEvent } from '@/features/session-data'

/** Resolves selection from the complete model by stable ID, independent of filtered row positions. */
export function useEventSelection(events: readonly SessionEvent[]) {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const index = events.findIndex((event) => event.id === selectedId)
  return {
    selectedId,
    selected: events[index],
    previous: events[index - 1],
    select: (id: string) => setSelectedId((current) => (current === id ? null : id)),
    close: () => setSelectedId(null),
  }
}
