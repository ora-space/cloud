import { useEffect, useRef, useState } from 'react'
import { PanelRight } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Sheet, SheetContent, SheetHeader, SheetTitle } from '@/components/ui/sheet'

/** Measures available task width, so the global navigation also counts toward the breakpoint. */
function useTaskWidth() {
  const ref = useRef<HTMLDivElement>(null)
  const [wide, setWide] = useState(true)
  useEffect(() => {
    const element = ref.current
    if (!element) return undefined
    const observer = new ResizeObserver(([entry]) => {
      if (entry && entry.contentRect.width > 0) setWide(entry.contentRect.width >= 1024)
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [])
  return { ref, wide }
}

/** Keeps details independently scrollable on desktop and accessible in a modal on narrow screens. */
export function TaskLayout({
  children,
  sidebar,
}: {
  children: React.ReactNode
  sidebar: React.ReactNode
}) {
  const { ref, wide } = useTaskWidth()
  const [expanded, setExpanded] = useState(true)
  const [drawer, setDrawer] = useState(false)
  const trigger = (
    <Button
      variant="ghost"
      size="sm"
      aria-label="任务详情"
      aria-expanded={wide ? expanded : drawer}
      onClick={() => (wide ? setExpanded((value) => !value) : setDrawer(true))}
    >
      <PanelRight className="size-4" />
      详情
    </Button>
  )
  return (
    <div ref={ref} className="relative flex min-h-0 flex-1 overflow-hidden">
      <main className="flex min-h-0 min-w-0 flex-1 flex-col">
        <div className="flex shrink-0 justify-end border-b px-3 py-1">{trigger}</div>
        {children}
      </main>
      {wide && expanded && (
        <aside
          aria-label="任务详情侧栏"
          className="w-[300px] shrink-0 space-y-5 overflow-y-auto border-l px-4 py-5 text-sm"
        >
          {sidebar}
        </aside>
      )}
      {!wide && (
        <Sheet open={drawer} onOpenChange={setDrawer}>
          <SheetContent className="w-[min(360px,100vw)]! overflow-y-auto">
            <SheetHeader>
              <SheetTitle>任务详情</SheetTitle>
            </SheetHeader>
            <div className="space-y-5 px-4 pb-6">{sidebar}</div>
          </SheetContent>
        </Sheet>
      )}
    </div>
  )
}
