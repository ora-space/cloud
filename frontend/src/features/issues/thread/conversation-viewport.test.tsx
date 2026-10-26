import { act, fireEvent, render, screen } from '@testing-library/react'
import { expect, it } from 'vitest'
import { ConversationViewport } from './conversation-viewport'

it('preserves an anchored message without marking prepended history as unread', async () => {
  const { rerender } = render(
    <ConversationViewport>
      <p key="current" data-message-anchor data-conversation-output="current">
        原来的消息
      </p>
    </ConversationViewport>,
  )
  const viewport = screen.getByLabelText('对话滚动区域')
  let height = 1000
  let anchorTop = 20
  Object.defineProperties(viewport, {
    clientHeight: { configurable: true, get: () => 400 },
    scrollHeight: { configurable: true, get: () => height },
  })
  const anchor = screen.getByText('原来的消息')
  anchor.getBoundingClientRect = () => new DOMRect(0, anchorTop, 200, 80)
  viewport.scrollTop = 300
  fireEvent.scroll(viewport)
  height = 1100
  anchorTop = 120
  await act(async () => {
    rerender(
      <ConversationViewport>
        <p key="older" data-message-anchor data-conversation-output="older">
          更早的消息
        </p>
        <p key="current" data-message-anchor data-conversation-output="current">
          原来的消息
        </p>
      </ConversationViewport>,
    )
  })
  // Prepending 100px keeps the original message at the same viewport position.
  expect(viewport.scrollTop).toBe(400)
  expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
})

it('does not auto-follow an execution list when its content changes', async () => {
  const { rerender } = render(
    <ConversationViewport followOutput={false}>
      <p>执行记录</p>
    </ConversationViewport>,
  )
  const viewport = screen.getByLabelText('对话滚动区域')
  Object.defineProperties(viewport, {
    clientHeight: { get: () => 400 },
    scrollHeight: { get: () => 1000 },
  })
  viewport.scrollTop = 80
  await act(async () => {
    rerender(
      <ConversationViewport followOutput={false}>
        <p>执行记录</p>
        <p>新增运行</p>
      </ConversationViewport>,
    )
  })
  expect(viewport.scrollTop).toBe(80)
  expect(screen.queryByRole('button', { name: '有新内容 · 回到底部' })).not.toBeInTheDocument()
})
