import { createContext, useContext, useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'

const InlineThread = createContext(false)

/** Threads embedded in a task share its viewport instead of owning nested scrolling. */
export function useInlineThread() {
  return useContext(InlineThread)
}

function outputSnapshot(body: Element) {
  return new Map(
    [...body.querySelectorAll('[data-conversation-output]')].map((element) => [
      element,
      element.getAttribute('data-conversation-output'),
    ]),
  )
}

function hasNewOutput(previous: Map<Element, string | null>, current: Map<Element, string | null>) {
  const tail = [...previous.keys()].findLast((element) => current.has(element))
  return [...current].some(([element, revision]) => {
    if (previous.has(element)) return previous.get(element) !== revision
    // Historical runs mount before the retained tail, even when their Threads
    // arrive later. Only additions after that boundary are new output.
    return !!tail && !!(tail.compareDocumentPosition(element) & Node.DOCUMENT_POSITION_FOLLOWING)
  })
}

/** Owns task scrolling, preserving a visible message anchor while older content is inserted. */
export function ConversationViewport({
  children,
  followOutput = true,
}: {
  children: React.ReactNode
  followOutput?: boolean
}) {
  const viewport = useRef<HTMLDivElement>(null)
  const content = useRef<HTMLDivElement>(null)
  const following = useRef(true)
  const anchor = useRef<{ element: Element; top: number } | null>(null)
  const [unread, setUnread] = useState(false)
  useEffect(() => {
    const element = viewport.current
    const body = content.current
    if (!element || !body || !followOutput) return undefined
    let output = outputSnapshot(body)
    function remember() {
      if (!element || !body) return
      const top = element.getBoundingClientRect().top
      const row = [...body.querySelectorAll('[data-message-anchor]')].find(
        (candidate) =>
          !candidate.querySelector('[data-message-anchor]') &&
          candidate.getBoundingClientRect().bottom > top,
      )
      anchor.current = row ? { element: row, top: row.getBoundingClientRect().top } : null
    }
    const onScroll = () => {
      following.current = element.scrollHeight - element.scrollTop - element.clientHeight < 80
      if (following.current) setUnread(false)
      output = outputSnapshot(body)
      remember()
    }
    const update = () => {
      // Hidden tab panels must not consume the reader's follow state.
      if (!element.clientHeight) return
      const nextOutput = outputSnapshot(body)
      if (following.current) element.scrollTop = element.scrollHeight
      else {
        const previous = anchor.current
        if (previous?.element.isConnected)
          element.scrollTop += previous.element.getBoundingClientRect().top - previous.top
        if (hasNewOutput(output, nextOutput)) setUnread(true)
      }
      output = nextOutput
      remember()
    }
    const resize = new ResizeObserver(update)
    const mutation = new MutationObserver(update)
    resize.observe(body)
    mutation.observe(body, {
      childList: true,
      subtree: true,
      characterData: true,
      attributes: true,
      attributeFilter: ['data-conversation-output'],
    })
    element.addEventListener('scroll', onScroll)
    update()
    return () => {
      resize.disconnect()
      mutation.disconnect()
      element.removeEventListener('scroll', onScroll)
    }
  }, [followOutput])
  return (
    <div className="relative min-h-0 flex-1">
      <div
        ref={viewport}
        aria-label="对话滚动区域"
        className="h-full overflow-y-auto overscroll-contain [overflow-anchor:none]"
      >
        <div ref={content} className="mx-auto w-full max-w-[840px] space-y-5 px-4 py-5 sm:px-6">
          <InlineThread.Provider value>{children}</InlineThread.Provider>
        </div>
      </div>
      {unread && (
        <Button
          className="absolute bottom-3 left-1/2 -translate-x-1/2"
          variant="secondary"
          size="sm"
          onClick={() => {
            following.current = true
            setUnread(false)
            if (viewport.current) viewport.current.scrollTop = viewport.current.scrollHeight
          }}
        >
          有新内容 · 回到底部
        </Button>
      )}
    </div>
  )
}
