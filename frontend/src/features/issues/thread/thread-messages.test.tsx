import { act, fireEvent, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { ThreadEntry } from '@/api/generated.schemas'
import { ThreadMessages } from './thread-messages'
import { ConversationViewport } from './conversation-viewport'

function message(seq: number, text = `Reply ${seq}`): ThreadEntry {
  return {
    seq,
    kind: 'update',
    source: 'node',
    turnId: `turn-${seq}`,
    record: {
      update: {
        sessionUpdate: 'agent_message_chunk',
        content: { type: 'text', text },
      },
    },
    createdAt: '2026-10-01T00:00:00Z',
  }
}

function inlineMessages(entries: ThreadEntry[], liveRevision = 0, loadingOlder = false) {
  return (
    <ConversationViewport>
      <ThreadMessages
        entries={entries}
        hasOlder
        loadingOlder={loadingOlder}
        onLoadOlder={() => {}}
        liveRevision={liveRevision}
      />
    </ConversationViewport>
  )
}

function readInlineHistory() {
  const viewport = screen.getByLabelText('对话滚动区域')
  Object.defineProperties(viewport, {
    scrollHeight: { value: 1000, configurable: true },
    clientHeight: { value: 300, configurable: true },
  })
  fireEvent.scroll(viewport, { target: { scrollTop: 100 } })
  return viewport
}

describe('ThreadMessages reading position', () => {
  it.each([false, true])(
    'does not mark older inline entries unread, including merged text (%s)',
    async (merged) => {
      const current = message(2)
      const older = { ...message(1), turnId: merged ? 'turn-2' : 'turn-1' }
      const view = render(inlineMessages([current]))
      readInlineHistory()
      await act(async () => view.rerender(inlineMessages([current], 0, true)))
      expect(screen.getByRole('button', { name: '加载中…' })).toBeDisabled()
      expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
      await act(async () => view.rerender(inlineMessages([older, current])))
      expect(screen.getByText(merged ? 'Reply 1Reply 2' : 'Reply 1')).toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
    },
  )

  it('keeps genuine unread output through history loading and resumes following after returning to the bottom', async () => {
    const view = render(inlineMessages([message(2)]))
    const viewport = readInlineHistory()
    await act(async () => view.rerender(inlineMessages([message(2), message(3)])))
    expect(viewport.scrollTop).toBe(100)
    expect(screen.getByRole('button', { name: '有新内容 · 回到底部' })).toBeInTheDocument()
    await act(async () => view.rerender(inlineMessages([message(1), message(2), message(3)])))
    fireEvent.click(screen.getByRole('button', { name: '有新内容 · 回到底部' }))
    expect(viewport.scrollTop).toBe(1000)
    expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
    Object.defineProperty(viewport, 'scrollHeight', { value: 1200 })
    await act(async () =>
      view.rerender(inlineMessages([message(1), message(2), message(3), message(4)])),
    )
    expect(viewport.scrollTop).toBe(1200)
  })

  it('marks streaming output unread when its preview revision changes without a new public sequence', async () => {
    const view = render(inlineMessages([message(2, 'Partial')], 1))
    const viewport = readInlineHistory()
    await act(async () => view.rerender(inlineMessages([message(2, 'Partial reply')], 2)))
    expect(screen.getByRole('button', { name: '有新内容 · 回到底部' })).toBeInTheDocument()
    expect(viewport.scrollTop).toBe(100)
  })

  it('does not pull readers away from history when a new message arrives', async () => {
    const scroll = vi.spyOn(Element.prototype, 'scrollIntoView')
    const load = vi.fn<() => void>()
    const view = render(
      <ThreadMessages
        entries={[message(1)]}
        hasOlder={false}
        loadingOlder={false}
        onLoadOlder={load}
      />,
    )
    const container = screen.getByRole('list', { name: '会话消息' }).parentElement
    if (!container) throw new Error('Message scroller missing')
    Object.defineProperties(container, {
      scrollHeight: { value: 1000, configurable: true },
      clientHeight: { value: 300, configurable: true },
    })
    fireEvent.scroll(container, { target: { scrollTop: 100 } })
    scroll.mockClear()
    view.rerender(
      <ThreadMessages
        entries={[message(1), message(2)]}
        hasOlder={false}
        loadingOlder={false}
        onLoadOlder={load}
      />,
    )
    expect(scroll).not.toHaveBeenCalled()
    await userEvent.setup().click(screen.getByRole('button', { name: '有新内容 · 回到底部' }))
    expect(scroll).toHaveBeenCalledOnce()
    expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
    scroll.mockRestore()
  })

  it('preserves the viewport anchor when older messages are prepended', async () => {
    const load = vi.fn<() => void>()
    const view = render(
      <ThreadMessages entries={[message(2)]} hasOlder loadingOlder={false} onLoadOlder={load} />,
    )
    const container = screen.getByRole('list', { name: '会话消息' }).parentElement
    if (!container) throw new Error('Message scroller missing')
    Object.defineProperties(container, {
      scrollHeight: { value: 600, configurable: true },
      clientHeight: { value: 200, configurable: true },
    })
    fireEvent.scroll(container, { target: { scrollTop: 40 } })
    await userEvent.setup().click(screen.getByRole('button', { name: '加载更早' }))
    Object.defineProperty(container, 'scrollHeight', { value: 900 })
    view.rerender(
      <ThreadMessages
        entries={[message(1), message(2)]}
        hasOlder={false}
        loadingOlder={false}
        onLoadOlder={load}
      />,
    )
    expect(load).toHaveBeenCalledOnce()
    expect(container.scrollTop).toBe(340)
  })
})
