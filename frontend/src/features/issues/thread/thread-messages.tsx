import { useEffect, useRef } from 'react'
import type { ThreadEntry } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { threadItems, type ThreadItem } from './thread-entries'

function ThreadRow({ item }: { item: ThreadItem }) {
  switch (item.variant) {
    case 'prompt':
      return (
        <li className="rounded-md border border-dashed px-3 py-2 text-sm">
          <p className="text-xs font-medium text-muted-foreground">任务</p>
          <p className="whitespace-pre-wrap">{item.text}</p>
        </li>
      )
    case 'user':
      return (
        <li className="ml-8 rounded-lg bg-primary/10 px-3 py-2 text-sm">
          <p className="whitespace-pre-wrap">{item.text}</p>
          {item.status && <p className="mt-1 text-xs text-muted-foreground">{item.status}</p>}
        </li>
      )
    case 'agent':
      return (
        <li className="mr-8 rounded-lg bg-muted px-3 py-2 text-sm">
          <p className="whitespace-pre-wrap">{item.text}</p>
        </li>
      )
    case 'separator':
      return <li aria-hidden="true" className="border-t" />
    default:
      return <li className="text-xs text-muted-foreground">{item.text}</li>
  }
}

/**
 * The Thread's rows, oldest first. `hasOlder` shows the "加载更早" control,
 * which calls `onLoadOlder`; this component never fetches by itself. It keeps
 * the newest row in view whenever a newer entry arrives, but not when older
 * rows are prepended, so paging back does not yank the reader to the bottom.
 */
export function ThreadMessages({
  entries,
  hasOlder,
  loadingOlder,
  onLoadOlder,
}: {
  entries: readonly ThreadEntry[]
  hasOlder: boolean
  loadingOlder: boolean
  onLoadOlder: () => void
}) {
  const endRef = useRef<HTMLDivElement>(null)
  const lastSeq = entries.at(-1)?.seq
  useEffect(() => {
    if (lastSeq !== undefined) endRef.current?.scrollIntoView({ block: 'nearest' })
  }, [lastSeq])

  return (
    <div className="max-h-[60vh] min-h-0 space-y-2 overflow-y-auto">
      {hasOlder && (
        <Button
          type="button"
          variant="ghost"
          size="xs"
          className="w-full"
          disabled={loadingOlder}
          onClick={onLoadOlder}
        >
          {loadingOlder ? '加载中…' : '加载更早'}
        </Button>
      )}
      <ol aria-label="会话消息" className="space-y-2">
        {threadItems(entries).map((item) => (
          <ThreadRow key={item.key} item={item} />
        ))}
      </ol>
      {entries.length === 0 && <p className="text-sm text-muted-foreground">暂无消息</p>}
      <div ref={endRef} />
    </div>
  )
}
