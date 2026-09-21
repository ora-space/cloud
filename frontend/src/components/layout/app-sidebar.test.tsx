import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it } from 'vitest'
import { http, HttpResponse } from 'msw'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { DashboardLayout } from '@/components/layout/dashboard-layout'
import { TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'

const currentUser = {
  id: '00000000-0000-4000-8000-000000000001',
  displayName: 'Wang Longan',
  status: 'active',
  version: 1,
  createdAt: '2026-09-21T00:00:00Z',
  deletedAt: null,
}

function renderDashboard(initialPath: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      { path: '/login', element: <div>Login screen</div> },
      {
        path: '/:workspaceSlug',
        element: <DashboardLayout />,
        children: [
          { path: 'issues', element: <div>Issues screen</div> },
          { path: 'projects', element: <div>Projects screen</div> },
        ],
      },
    ],
    { initialEntries: [initialPath] },
  )
  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  )
}

describe('AppSidebar workspace switcher', () => {
  beforeEach(() => {
    server.use(
      http.get('/api/v1/me', () => HttpResponse.json(currentUser)),
      http.get('/api/v1/me/tenants', () =>
        HttpResponse.json({
          items: [{ id: TEST_TENANT_ID, name: '研发组织', status: 'active', role: 'admin' }],
          nextCursor: '',
        }),
      ),
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces`, () =>
        HttpResponse.json({
          items: [
            { id: 'space-1', name: 'Cloud Dev', slug: 'cloud-dev', role: 'owner' },
            { id: 'space-2', name: 'Platform', slug: 'platform', role: 'member' },
          ],
          nextCursor: '',
        }),
      ),
    )
  })

  it('opens without crashing and lists every workspace', async () => {
    const user = userEvent.setup()
    renderDashboard('/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(await screen.findByRole('button', { name: /Cloud Dev/ }))

    const menu = await screen.findByRole('menu')
    expect(await screen.findByRole('menuitem', { name: /Cloud Dev/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /Platform/ })).toBeInTheDocument()
    expect(screen.getByText('Wang Longan')).toBeInTheDocument()
    expect(menu).toBeInTheDocument()
  })

  it('switches to a different workspace and lands on its projects page', async () => {
    const user = userEvent.setup()
    renderDashboard('/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(await screen.findByRole('button', { name: /Cloud Dev/ }))
    await user.click(await screen.findByRole('menuitem', { name: /Platform/ }))

    await waitFor(() => {
      expect(screen.getByText('Projects screen')).toBeInTheDocument()
    })
  })

  it('revokes the Gateway session before returning to login', async () => {
    let logoutCalls = 0
    server.use(
      http.post('/auth/logout', () => {
        logoutCalls += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    renderDashboard('/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(await screen.findByRole('button', { name: /Cloud Dev/ }))
    await user.click(await screen.findByRole('menuitem', { name: '退出登录' }))

    expect(await screen.findByText('Login screen')).toBeInTheDocument()
    expect(logoutCalls).toBe(1)
  })
})
