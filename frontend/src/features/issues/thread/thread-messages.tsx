import { useEffect, useRef, useState } from 'react'
import type { ThreadEntry } from '@/api/generated.schemas'
import { Button } from '@/components/ui/button'
import { threadItems } from './thread-entries'
import { ThreadRow } from './thread-row'
import { useInlineThread } from './conversation-viewport'

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
  onQuote,
  liveRevision = 0,
  userName = '用户',
}: {
  entries: readonly (ThreadEntry & { livePreview?: boolean })[]
  hasOlder: boolean
  loadingOlder: boolean
  onLoadOlder: () => void
  onQuote?: ((text: string) => void) | undefined
  userName?: string
  liveRevision?: number
}) {
  const inline = useInlineThread()
  const endRef = useRef<HTMLDivElement>(null)
  const scrollRef = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  const olderAnchor = useRef<{ height: number; top: number } | null>(null)
  const [unread, setUnread] = useState(false)
  const lastSeq = entries.at(-1)?.seq
  const firstSeq = entries.at(0)?.seq
  useEffect(() => {
    if (inline) return
    if (lastSeq === undefined && liveRevision === 0) return
    if (following.current) endRef.current?.scrollIntoView({ block: 'nearest' })
    else setUnread(true)
  }, [lastSeq, liveRevision, inline])
  useEffect(() => {
    if (inline) return
    if (firstSeq === undefined) return
    const element = scrollRef.current
    const anchor = olderAnchor.current
    if (element && anchor) {
      element.scrollTop = anchor.top + element.scrollHeight - anchor.height
      olderAnchor.current = null
    }
  }, [firstSeq, inline])

  return (
    <div
      ref={scrollRef}
      data-conversation-output={lastSeq === undefined ? undefined : `${lastSeq}:${liveRevision}`}
      className={
        inline
          ? 'min-w-0 space-y-4'
          : 'relative max-h-[65vh] min-h-0 space-y-4 overflow-y-auto overscroll-contain p-1'
      }
      onScroll={(event) => {
        const element = event.currentTarget
        following.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80
        if (following.current) setUnread(false)
      }}
    >
      {hasOlder && (
        <Button
          type="button"
          variant="ghost"
          size="xs"
          className="w-full"
          disabled={loadingOlder}
          onClick={() => {
            const element = scrollRef.current
            if (element)
              olderAnchor.current = { height: element.scrollHeight, top: element.scrollTop }
            onLoadOlder()
          }}
        >
          {loadingOlder ? '加载中…' : '加载更早'}
        </Button>
      )}
      <MessageRows entries={entries} onQuote={onQuote} userName={userName} />
      <div ref={endRef} />
      {unread && (
        <Button
          type="button"
          variant="secondary"
          size="sm"
          className="sticky bottom-2 mx-auto flex"
          onClick={() => {
            following.current = true
            setUnread(false)
            endRef.current?.scrollIntoView({ block: 'nearest' })
          }}
        >
          有新内容 · 回到底部
        </Button>
      )}
    </div>
  )
}

function MessageRows({
  entries,
  onQuote,
  userName,
}: {
  entries: readonly ThreadEntry[]
  onQuote: ((text: string) => void) | undefined
  userName: string
}) {
  const times = new Map(entries.map((entry) => [entry.seq, entry.createdAt]))
  return (
    <>
      <ol aria-label="会话消息" className="space-y-4">
        {threadItems(entries).map((item) => (
          <ThreadRow
            key={item.key}
            item={item}
            onQuote={onQuote}
            userName={userName}
            at={times.get(item.key)}
          />
        ))}
      </ol>
      {entries.length === 0 && <p className="text-sm text-muted-foreground">暂无消息</p>}
    </>
  )
}
