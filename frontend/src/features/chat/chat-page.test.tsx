import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { db } from '@/mocks/data/store'
import { renderAtRoute } from '@/test/render'
import { ChatPage } from './chat-page'

describe('standalone chat entry', () => {
  it.each(['/chat', '/chat/old-session'])(
    'keeps %s unavailable and links to real tasks',
    (suffix) => {
      renderAtRoute(
        '/w/:workspaceSlug/chat/:sessionId?',
        <ChatPage slug="real-workspace" />,
        `/w/real-workspace${suffix}`,
      )
      expect(screen.getByRole('heading', { name: '聊天暂不可用' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: '前往任务' })).toHaveAttribute(
        'href',
        '/w/real-workspace/issues',
      )
      expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
      for (const session of db.chatSessions) {
        expect(screen.queryByText(session.title)).not.toBeInTheDocument()
      }
    },
  )
})
