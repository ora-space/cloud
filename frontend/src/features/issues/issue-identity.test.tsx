import { useQueryClient } from '@tanstack/react-query'
import { fireEvent, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { Navigate } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { RequireSession } from '@/features/auth/require-session'
import { SESSION_QUERY_KEY, useSession } from '@/features/auth/session'
import { CreateIssueDialog } from '@/features/issues/components/create-issue-dialog'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import {
  installCloudSpaceHandlers,
  TEST_SPACE_ID,
  TEST_TENANT_ID,
  TEST_USER,
} from '@/test/cloud-handlers'
import { renderRoutes } from '@/test/render'
import { server } from '@/test/msw-server'

function SignedInTasks() {
  const { signOut } = useSession()
  return (
    <RequireSession>
      <CurrentSpaceProvider slug="cloud-dev">
        <button
          onClick={() => {
            void signOut()
          }}
        >
          Sign out
        </button>
        <CreateIssueDialog slug={TEST_TENANT_ID} />
      </CurrentSpaceProvider>
    </RequireSession>
  )
}

function TestLogin() {
  const cache = useQueryClient()
  const { session } = useSession()
  async function signIn() {
    await cache.invalidateQueries({ queryKey: SESSION_QUERY_KEY })
  }
  if (session.status === 'signed-in') return <Navigate to="/w/cloud-dev/issues" replace />
  return (
    <button
      onClick={() => {
        void signIn()
      }}
    >
      Sign in as Bob
    </button>
  )
}

describe('task draft identity isolation', () => {
  it('drops an open draft on logout and starts blank for another member in the same space', async () => {
    installCloudSpaceHandlers('admin')
    let member: typeof TEST_USER | undefined = TEST_USER
    server.use(
      http.get('/api/v1/me', () => {
        return member
          ? HttpResponse.json(member)
          : HttpResponse.json(
              { code: 'unauthenticated', params: {}, requestId: 'r' },
              { status: 401 },
            )
      }),
      http.post('/auth/logout', () => {
        member = undefined
        return new HttpResponse(null, { status: 204 })
      }),
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/issues`, () =>
        HttpResponse.json({ items: [], nextCursor: '' }),
      ),
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
        HttpResponse.json({ items: [], nextCursor: '' }),
      ),
    )
    const user = userEvent.setup()
    renderRoutes(
      [
        { path: '/w/cloud-dev/issues', element: <SignedInTasks /> },
        { path: '/login', element: <TestLogin /> },
      ],
      '/w/cloud-dev/issues',
    )
    await user.click(await screen.findByRole('button', { name: '新建任务' }))
    await user.click(screen.getByPlaceholderText('任务标题'))
    await user.paste('First member private draft')
    fireEvent.click(screen.getByText('Sign out'))
    await screen.findByRole('button', { name: 'Sign in as Bob' })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    member = { ...TEST_USER, id: 'other-member', displayName: 'Bob' }
    await user.click(screen.getByRole('button', { name: 'Sign in as Bob' }))
    await user.click(await screen.findByRole('button', { name: '新建任务' }))
    expect(screen.getByPlaceholderText('任务标题')).toHaveValue('')
  })
})
