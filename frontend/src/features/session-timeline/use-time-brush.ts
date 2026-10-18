import { useRef, useState, type PointerEvent } from 'react'

/** Inclusive observed-time window; unknown timestamps remain discoverable outside the chart. */
export type TimeWindow = { startMs: number; endMs: number }

type Drag = { pointerId: number; startX: number; left: number; width: number; bounds: TimeWindow }

function atPosition(x: number, drag: Drag): number {
  const fraction = Math.max(0, Math.min(1, (x - drag.left) / drag.width))
  return drag.bounds.startMs + fraction * (drag.bounds.endMs - drag.bounds.startMs)
}

function windowFor(x: number, drag: Drag): TimeWindow {
  const first = atPosition(drag.startX, drag)
  const last = atPosition(x, drag)
  return { startMs: Math.min(first, last), endMs: Math.max(first, last) }
}

/** Owns pointer capture and cancellation; the coordinate system is frozen for each drag. */
export function useTimeBrush(bounds: TimeWindow, onChange: (value: TimeWindow | null) => void) {
  const drag = useRef<Drag | null>(null)
  const [draft, setDraft] = useState<TimeWindow | null>(null)
  const cancel = () => {
    drag.current = null
    setDraft(null)
  }
  return {
    draft,
    onPointerDown: (event: PointerEvent<HTMLDivElement>) => {
      if (event.button !== 0 || drag.current) return
      const rect = event.currentTarget.getBoundingClientRect()
      if (rect.width <= 0) return
      drag.current = {
        pointerId: event.pointerId,
        startX: event.clientX,
        left: rect.left,
        width: rect.width,
        bounds,
      }
      event.currentTarget.setPointerCapture?.(event.pointerId)
      setDraft(windowFor(event.clientX, drag.current))
    },
    onPointerMove: (event: PointerEvent<HTMLDivElement>) => {
      if (drag.current?.pointerId !== event.pointerId) return
      setDraft(windowFor(event.clientX, drag.current))
    },
    onPointerUp: (event: PointerEvent<HTMLDivElement>) => {
      const active = drag.current
      if (!active || active.pointerId !== event.pointerId) return
      onChange(
        Math.abs(event.clientX - active.startX) < 5 ? null : windowFor(event.clientX, active),
      )
      cancel()
      if (event.currentTarget.hasPointerCapture?.(event.pointerId))
        event.currentTarget.releasePointerCapture(event.pointerId)
    },
    onPointerCancel: cancel,
    onLostPointerCapture: cancel,
  }
}
