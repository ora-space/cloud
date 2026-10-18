import { Button } from '@/components/ui/button'

/** Compact columns show event order; time mode preserves proportional durations and gaps. */
export type TimelineLayout = 'compact' | 'time'

type ControlsProps = {
  layout: TimelineLayout
  onLayout: (layout: TimelineLayout) => void
  canZoom: boolean
  onZoom: () => void
  isZoomed: boolean
  onReset: () => void
}

/** Keeps layout choice separate from the real-time viewport and selected time range. */
export function TimelineControls({
  layout,
  onLayout,
  canZoom,
  onZoom,
  isZoomed,
  onReset,
}: ControlsProps) {
  return (
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div role="group" aria-label="时间轴布局" className="flex gap-1">
        {(['compact', 'time'] as const).map((mode) => (
          <Button
            key={mode}
            size="sm"
            variant={layout === mode ? 'secondary' : 'outline'}
            aria-pressed={layout === mode}
            onClick={() => onLayout(mode)}
          >
            {mode === 'compact' ? '紧凑事件' : '真实时间'}
          </Button>
        ))}
      </div>
      {layout === 'time' && (
        <div className="flex items-center gap-2">
          <span className="text-xs text-muted-foreground">
            {isZoomed ? '局部视窗' : '全程视窗'}
          </span>
          <Button size="sm" variant="outline" disabled={!canZoom} onClick={onZoom}>
            放大选区
          </Button>
          <Button size="sm" variant="outline" disabled={!isZoomed} onClick={onReset}>
            显示全程
          </Button>
        </div>
      )}
    </div>
  )
}
