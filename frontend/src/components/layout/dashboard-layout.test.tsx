import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it } from 'vitest'
import { createMemoryRouter, RouterProvider, useLocation } from 'react-router-dom'
import { db } from '@/mocks/data/store'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { DashboardLayout } from './dashboard-layout'

const currentUser = {
  id: '00000000-0000-4000-8000-000000000001',
  displayName: 'Wang Longan',
  status: 'active',
  version: 1,
  createdAt: '2026-09-21T00:00:00Z',
  deletedAt: null,
}

function LoginScreen() {
  const location = useLocation()
  return <div>Login screen {location.search}</div>
}

function renderRouter(initialPath: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      { path: '/login', element: <LoginScreen /> },
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

describe('DashboardLayout', () => {
  beforeEach(() => {
    server.use(http.get('/api/v1/me', () => HttpResponse.json(currentUser)))
    installCloudSpaceHandlers('owner')
  })

  it('redirects to /login with the complete target when there is no session', async () => {
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json(
          { code: 'unauthenticated', params: {}, requestId: 'request-1' },
          { status: 401 },
        ),
      ),
    )
    renderRouter(`/${db.workspace.slug}/issues?tab=mine#today`)
    expect(
      await screen.findByText(
        `Login screen ?returnTo=%2F${db.workspace.slug}%2Fissues%3Ftab%3Dmine%23today`,
      ),
    ).toBeInTheDocument()
  })

  it('redirects an unknown space slug to the first joined real space', async () => {
    renderRouter('/some-other-workspace/issues')
    expect(await screen.findByText('Projects screen')).toBeInTheDocument()
  })

  it('renders the matched child route once authenticated', async () => {
    renderRouter('/cloud-dev/issues')
    expect(await screen.findByText('Issues screen')).toBeInTheDocument()
  })

  it('shows a disabled account without redirecting to login', async () => {
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json(
          { code: 'user_disabled', params: {}, requestId: 'request-2' },
          { status: 403 },
        ),
      ),
    )
    renderRouter('/cloud-dev/issues')
    expect(await screen.findByText('账号已被停用')).toBeInTheDocument()
  })

  it('shows an empty state instead of demo data when the user joined no space', async () => {
    server.use(
      http.get('/api/v1/me/tenants', () =>
        HttpResponse.json({
          items: [{ id: TEST_TENANT_ID, name: '研发组织', status: 'active', role: 'admin' }],
          nextCursor: '',
        }),
      ),
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces`, () =>
        HttpResponse.json({ items: [], nextCursor: '' }),
      ),
    )
    renderRouter('/default/issues')
    expect(await screen.findByText(/尚未加入任何工作区/)).toBeInTheDocument()
    expect(screen.queryByText('Issues screen')).not.toBeInTheDocument()
  })
})
