import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { DashboardLayout } from '@/components/layout/dashboard-layout'
import { GITHUB_SIGN_OUT_URL } from '@/features/auth/api'
import { RequireSession } from '@/features/auth/require-session'
import {
  installSignedInSession,
  TEST_SPACE_ID,
  TEST_TENANT_ID,
  TEST_USER_ID,
} from '@/test/cloud-handlers'
import { installFakeNavigation } from '@/test/navigation'
import { renderRoutes } from '@/test/render'
import { server } from '@/test/msw-server'

const SECOND_SPACE_ID = '77777777-7777-7777-7777-777777777777'

function space(id: string, name: string, slug: string) {
  return {
    id,
    tenantId: TEST_TENANT_ID,
    name,
    slug,
    description: '',
    createdBy: TEST_USER_ID,
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    updatedAt: '2026-09-20T10:00:00+08:00',
    archivedAt: null,
    role: 'owner',
  }
}

function installTwoSpaces() {
  installSignedInSession()
  server.use(
    http.get('/api/v1/me/tenants', () =>
      HttpResponse.json({
        items: [{ id: TEST_TENANT_ID, name: '研发组织', status: 'active', role: 'admin' }],
        nextCursor: '',
      }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces`, () =>
      HttpResponse.json({
        items: [
          space(TEST_SPACE_ID, 'Cloud Dev', 'cloud-dev'),
          space(SECOND_SPACE_ID, 'Ops', 'ops'),
        ],
        nextCursor: '',
      }),
    ),
  )
}

function renderDashboard(initialPath: string) {
  return renderRoutes(
    [
      { path: '/login', element: <div>Login screen</div> },
      {
        path: '/w/:workspaceSlug',
        element: (
          <RequireSession>
            <DashboardLayout />
          </RequireSession>
        ),
        children: [
          { path: 'issues', element: <div>Issues screen</div> },
          { path: 'plugins', element: <div>Plugins screen</div> },
        ],
      },
    ],
    initialPath,
  )
}

describe('AppSidebar workspace switcher', () => {
  it('shows the current space and the signed-in member, and lists every joined space', async () => {
    installTwoSpaces()
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(screen.getByRole('button', { name: /Cloud Dev/ }))

    expect(await screen.findByRole('menu')).toBeInTheDocument()
    expect(screen.getByText('Alice')).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /Cloud Dev/ })).toBeInTheDocument()
    expect(await screen.findByRole('menuitem', { name: /Ops/ })).toBeInTheDocument()
    expect(screen.getByRole('menuitem', { name: /新建工作区/ })).toBeInTheDocument()
  })

  it('switches to a different space and lands on its issues page', async () => {
    installTwoSpaces()
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(screen.getByRole('button', { name: /Cloud Dev/ }))
    await user.click(await screen.findByRole('menuitem', { name: /Ops/ }))

    await waitFor(() => {
      expect(screen.getByRole('button', { name: /Ops/ })).toBeInTheDocument()
    })
  })

  it('offers the plugin entry beside skills and routes to the plugin page', async () => {
    installTwoSpaces()
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    const skills = screen.getByRole('link', { name: /技能/ })
    const plugins = screen.getByRole('link', { name: /插件/ })
    expect(plugins).toHaveAttribute('href', '/w/cloud-dev/plugins')
    expect(plugins.closest('[data-sidebar="group"]')).toBe(skills.closest('[data-sidebar="group"]'))

    await user.click(plugins)
    expect(await screen.findByText('Plugins screen')).toBeInTheDocument()
  })

  it('signs out through the gateway and returns to the login screen', async () => {
    installTwoSpaces()
    let loggedOut = false
    server.use(
      http.post('/auth/logout', () => {
        loggedOut = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(screen.getByRole('button', { name: /Cloud Dev/ }))
    await user.click(await screen.findByRole('menuitem', { name: /退出登录/ }))

    expect(await screen.findByText('Login screen')).toBeInTheDocument()
    expect(loggedOut).toBe(true)
  })

  it('offers a GitHub sign-out that revokes the Ora session first, only when GitHub login exists', async () => {
    installTwoSpaces()
    let loggedOut = false
    server.use(
      http.post('/auth/logout', () => {
        loggedOut = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const navigation = installFakeNavigation()
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(screen.getByRole('button', { name: /Cloud Dev/ }))
    await user.click(await screen.findByRole('menuitem', { name: /退出并注销 GitHub/ }))

    // Ora signs out in this tab (back to the login screen); GitHub's page opens beside it.
    expect(await screen.findByText('Login screen')).toBeInTheDocument()
    await waitFor(() => expect(navigation.openedTabs).toEqual([GITHUB_SIGN_OUT_URL]))
    expect(navigation.destinations).toEqual([])
    expect(loggedOut).toBe(true)
  })

  it('hides the GitHub sign-out when the gateway has no GitHub login', async () => {
    installTwoSpaces()
    server.use(http.get('/auth/providers', () => HttpResponse.json({ providers: ['dev'] })))
    const user = userEvent.setup()
    renderDashboard('/w/cloud-dev/issues')
    await screen.findByText('Issues screen')

    await user.click(screen.getByRole('button', { name: /Cloud Dev/ }))
    expect(await screen.findByRole('menuitem', { name: /退出登录/ })).toBeInTheDocument()
    expect(screen.queryByRole('menuitem', { name: /退出并注销 GitHub/ })).not.toBeInTheDocument()
  })
})
