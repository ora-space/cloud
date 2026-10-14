import { QueryClient, QueryClientProvider, useQueryClient } from '@tanstack/react-query'
import { act, renderHook, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { AXIOS_INSTANCE } from '@/lib/api-client'
import { PENDING_JOIN_PATH_KEY } from '@/lib/paths'
import { installSignedInSession, TEST_USER } from '@/test/cloud-handlers'
import { installFakeNavigation } from '@/test/navigation'
import { renderRoutes, renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { getGetApiV1MeModelConnectionsQueryKey } from '@/api/me/me'
import {
  fetchLoginProviders,
  fetchSessionUser,
  GITHUB_SIGN_OUT_URL,
  logoutSession,
  startLogin,
} from './api'
import { LoginPage } from './login-page'
import { RequireSession } from './require-session'
import { SessionProvider, useSession } from './session'

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return (
    <QueryClientProvider client={queryClient}>
      <SessionProvider>{children}</SessionProvider>
    </QueryClientProvider>
  )
}

describe('gateway auth api', () => {
  it('starts a login through the gateway and sends the browser to the provider', async () => {
    let body: unknown = null
    server.use(
      http.post('/auth/login', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ authorizationUrl: 'https://github.com/login/oauth/authorize?x' })
      }),
    )
    const navigation = installFakeNavigation()

    await startLogin('github', '/acme/issues')

    expect(body).toEqual({ provider: 'github', returnTo: '/acme/issues' })
    expect(navigation.destinations).toEqual(['https://github.com/login/oauth/authorize?x'])
  })

  it('does not leave the page when the gateway refuses the login start', async () => {
    server.use(
      http.post('/auth/login', () =>
        HttpResponse.json(
          { code: 'origin_forbidden', params: {}, requestId: 'r' },
          { status: 403 },
        ),
      ),
    )
    const navigation = installFakeNavigation()

    await expect(startLogin('github', '/')).rejects.toBeDefined()
    expect(navigation.destinations).toEqual([])
  })

  it('lists the providers the gateway offers and drops ones this build cannot present', async () => {
    server.use(
      http.get('/auth/providers', () =>
        HttpResponse.json({ providers: ['saml-future', 'dev', 'github'] }),
      ),
    )
    await expect(fetchLoginProviders()).resolves.toEqual(['github', 'dev'])
  })

  it('classifies the session probe: 401 signed out, 403 disabled, anything else an error', async () => {
    await expect(fetchSessionUser()).resolves.toEqual({ kind: 'signed-out' })

    installSignedInSession()
    await expect(fetchSessionUser()).resolves.toEqual({ kind: 'signed-in', user: TEST_USER })

    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'user_disabled', params: {}, requestId: 'r' }, { status: 403 }),
      ),
    )
    await expect(fetchSessionUser()).resolves.toEqual({ kind: 'disabled' })

    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'internal_error', params: {}, requestId: 'r' }, { status: 500 }),
      ),
    )
    await expect(fetchSessionUser()).rejects.toBeDefined()
  })

  it('logs out through the gateway', async () => {
    let called = 0
    server.use(
      http.post('/auth/logout', () => {
        called += 1
        return new HttpResponse(null, { status: 204 })
      }),
    )
    await logoutSession()
    expect(called).toBe(1)
  })
})

describe('SessionProvider', () => {
  it('reports signed out when the probe answers 401', async () => {
    const { result } = renderHook(() => useSession(), { wrapper })
    expect(result.current.session).toEqual({ status: 'loading' })
    await waitFor(() => expect(result.current.session).toEqual({ status: 'signed-out' }))
  })

  it('reports the member when the probe succeeds', async () => {
    installSignedInSession()
    const { result } = renderHook(() => useSession(), { wrapper })
    await waitFor(() =>
      expect(result.current.session).toEqual({ status: 'signed-in', user: TEST_USER }),
    )
  })

  it('reports a disabled account distinctly from signed out', async () => {
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'user_disabled', params: {}, requestId: 'r' }, { status: 403 }),
      ),
    )
    const { result } = renderHook(() => useSession(), { wrapper })
    await waitFor(() => expect(result.current.session).toEqual({ status: 'disabled' }))
  })

  it('reports unavailable, not signed out, when the probe fails for another reason', async () => {
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'internal_error', params: {}, requestId: 'r' }, { status: 500 }),
      ),
    )
    const { result } = renderHook(() => useSession(), { wrapper })
    await waitFor(() => expect(result.current.session).toEqual({ status: 'unavailable' }))
  })

  it('ends an expired session and drops the previous member private model cache', async () => {
    installSignedInSession()
    server.use(
      http.get('/api/v1/me/tenants', () =>
        HttpResponse.json({ code: 'unauthenticated', params: {}, requestId: 'r' }, { status: 401 }),
      ),
    )
    const { result } = renderHook(() => ({ ...useSession(), queryClient: useQueryClient() }), {
      wrapper,
    })
    await waitFor(() => expect(result.current.session.status).toBe('signed-in'))
    const key = getGetApiV1MeModelConnectionsQueryKey()
    result.current.queryClient.setQueryData(key, [{ name: 'Previous member private model' }])

    await act(async () => {
      await AXIOS_INSTANCE.get('/api/v1/me/tenants').catch(() => undefined)
    })

    await waitFor(() => expect(result.current.session).toEqual({ status: 'signed-out' }))
    expect(result.current.queryClient.getQueryData(key)).toBeUndefined()
  })

  it('signs out through the gateway and forgets the member', async () => {
    installSignedInSession()
    server.use(http.post('/auth/logout', () => new HttpResponse(null, { status: 204 })))
    const { result } = renderHook(() => useSession(), { wrapper })
    await waitFor(() => expect(result.current.session.status).toBe('signed-in'))

    await act(() => result.current.signOut())

    await waitFor(() => expect(result.current.session).toEqual({ status: 'signed-out' }))
  })

  it("signs out of Ora, then opens GitHub's sign-out page in a new tab", async () => {
    installSignedInSession()
    let loggedOut = false
    server.use(
      http.post('/auth/logout', () => {
        loggedOut = true
        return new HttpResponse(null, { status: 204 })
      }),
    )
    const navigation = installFakeNavigation()
    const { result } = renderHook(() => useSession(), { wrapper })
    await waitFor(() => expect(result.current.session.status).toBe('signed-in'))

    await act(() => result.current.signOutOfGitHub())

    expect(loggedOut).toBe(true)
    await waitFor(() => expect(result.current.session).toEqual({ status: 'signed-out' }))
    expect(navigation.openedTabs).toEqual([GITHUB_SIGN_OUT_URL])
    expect(navigation.destinations).toEqual([])
  })
})

describe('RequireSession', () => {
  const routes = [
    { path: '/login', element: <div>Login screen</div> },
    {
      path: '/private',
      element: (
        <RequireSession>
          <div>Private screen</div>
        </RequireSession>
      ),
    },
  ]

  it('redirects a signed-out tab to the login page carrying returnTo', async () => {
    renderRoutes(
      [{ path: '/login', element: <LocationEcho /> }, ...routes.slice(1)],
      '/private?tab=2',
    )
    expect(await screen.findByText('/login?returnTo=%2Fprivate%3Ftab%3D2')).toBeInTheDocument()
  })

  it('keeps a private invitation token out of the Gateway login return path', async () => {
    const token = 'a'.repeat(43)
    const path = `/join/invite/${token}`
    renderRoutes(
      [
        { path: '/login', element: <LocationEcho /> },
        { path: '/join/invite/:token', element: <RequireSession>Private join</RequireSession> },
      ],
      path,
    )
    expect(await screen.findByText('/login?returnTo=%2Fjoin%2Fcontinue')).toBeInTheDocument()
    expect(sessionStorage.getItem(PENDING_JOIN_PATH_KEY)).toBe(path)
    sessionStorage.removeItem(PENDING_JOIN_PATH_KEY)
  })

  it('renders the protected screen for a member', async () => {
    installSignedInSession()
    renderRoutes(routes, '/private')
    expect(await screen.findByText('Private screen')).toBeInTheDocument()
  })

  it('reports a disabled account in place instead of redirecting to login', async () => {
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'user_disabled', params: {}, requestId: 'r' }, { status: 403 }),
      ),
    )
    renderRoutes(routes, '/private')
    expect(await screen.findByText(/账号已被停用/)).toBeInTheDocument()
    expect(screen.queryByText('Login screen')).not.toBeInTheDocument()
  })

  it('reports an unreachable backend instead of redirecting', async () => {
    server.use(http.get('/api/v1/me', () => HttpResponse.error()))
    renderRoutes(routes, '/private')
    expect(await screen.findByText(/无法连接服务端/)).toBeInTheDocument()
    expect(screen.queryByText('Login screen')).not.toBeInTheDocument()
  })
})

function LocationEcho() {
  const location = useLocation()
  return <div>{`${location.pathname}${location.search}`}</div>
}

describe('LoginPage', () => {
  it('starts the only external login by itself with the requested returnTo', async () => {
    let calls = 0
    let body: unknown = null
    server.use(
      http.post('/auth/login', async ({ request }) => {
        calls += 1
        body = await request.json()
        return HttpResponse.json({ authorizationUrl: 'https://github.com/login/oauth/authorize' })
      }),
    )
    const navigation = installFakeNavigation()
    renderWithProviders(<LoginPage />, { route: '/login?returnTo=%2Fw%2Facme%2Fprojects' })

    await waitFor(() => expect(navigation.destinations).toHaveLength(1))
    expect(body).toEqual({ provider: 'github', returnTo: '/w/acme/projects' })
    expect(calls).toBe(1)
    expect(screen.getByText('正在跳转到 GitHub…')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /登录/ })).not.toBeInTheDocument()
  })

  it('auto-starts the corporate login for an IDaaS-only gateway', async () => {
    let body: unknown = null
    server.use(
      http.get('/auth/providers', () =>
        HttpResponse.json({ providers: ['huawei-idaas'], default: 'huawei-idaas' }),
      ),
      http.post('/auth/login', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({
          authorizationUrl: 'https://uniportal.huawei.com/saaslogin1/oauth2/v1/authorize?x',
        })
      }),
    )
    const navigation = installFakeNavigation()
    renderWithProviders(<LoginPage />, { route: '/login' })

    await waitFor(() => expect(navigation.destinations).toHaveLength(1))
    expect(body).toEqual({ provider: 'huawei-idaas', returnTo: '/' })
    expect(screen.getByText('正在跳转华为统一登录…')).toBeInTheDocument()
  })

  it('falls back to / for a returnTo that is not a same-origin path', async () => {
    let body: unknown = null
    server.use(
      http.post('/auth/login', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ authorizationUrl: 'https://github.com/login/oauth/authorize' })
      }),
    )
    installFakeNavigation()
    renderWithProviders(<LoginPage />, { route: '/login?returnTo=//evil.example' })

    await waitFor(() => expect(body).toEqual({ provider: 'github', returnTo: '/' }))
  })

  it('stops after one failed automatic start and offers an explicit retry', async () => {
    let calls = 0
    server.use(
      http.post('/auth/login', () => {
        calls += 1
        return HttpResponse.error()
      }),
    )
    const navigation = installFakeNavigation()
    const user = userEvent.setup()
    renderWithProviders(<LoginPage />, { route: '/login' })

    expect(await screen.findByText(/无法开始登录/)).toBeInTheDocument()
    expect(calls).toBe(1)
    const retry = screen.getByRole('button', { name: '重新登录' })
    expect(retry).toBeEnabled()

    server.use(
      http.post('/auth/login', () =>
        HttpResponse.json({ authorizationUrl: 'https://github.com/login/oauth/authorize' }),
      ),
    )
    await user.click(retry)
    await waitFor(() => expect(navigation.destinations).toHaveLength(1))
  })

  it('tells a disabled account so instead of starting another login', async () => {
    let calls = 0
    server.use(
      http.get('/api/v1/me', () =>
        HttpResponse.json({ code: 'user_disabled', params: {}, requestId: 'r' }, { status: 403 }),
      ),
      http.post('/auth/login', () => {
        calls += 1
        return HttpResponse.json({ authorizationUrl: 'https://github.com/login/oauth/authorize' })
      }),
    )
    const navigation = installFakeNavigation()
    renderWithProviders(<LoginPage />, { route: '/login' })

    expect(await screen.findByText('账号已被停用')).toBeInTheDocument()
    expect(calls).toBe(0)
    expect(navigation.destinations).toEqual([])
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
  })

  it('offers a choice, and starts nothing by itself, when the developer login is registered', async () => {
    let body: unknown = null
    server.use(
      http.get('/auth/providers', () =>
        HttpResponse.json({ providers: ['dev', 'github'], default: 'github' }),
      ),
      http.post('/auth/login', async ({ request }) => {
        body = await request.json()
        return HttpResponse.json({ authorizationUrl: 'http://localhost:5173/auth/dev/authorize?s' })
      }),
    )
    const navigation = installFakeNavigation()
    const user = userEvent.setup()
    renderWithProviders(<LoginPage />, { route: '/login?returnTo=%2Fw%2Facme%2Fissues' })

    const dev = await screen.findByRole('button', { name: '开发者登录（仅本地）' })
    expect(screen.getByRole('button', { name: '使用 GitHub 登录' })).toBeInTheDocument()
    expect(navigation.destinations).toEqual([])

    await user.click(dev)

    await waitFor(() => expect(navigation.destinations).toHaveLength(1))
    expect(body).toEqual({ provider: 'dev', returnTo: '/w/acme/issues' })
  })

  it('offers to sign out of GitHub when GitHub login exists', async () => {
    server.use(
      http.get('/auth/providers', () =>
        HttpResponse.json({ providers: ['dev', 'github'], default: 'github' }),
      ),
      http.post('/auth/logout', () => new HttpResponse(null, { status: 204 })),
    )
    const navigation = installFakeNavigation()
    const user = userEvent.setup()
    renderWithProviders(<LoginPage />, { route: '/login' })

    await user.click(await screen.findByRole('button', { name: '先退出 GitHub' }))
    await waitFor(() => expect(navigation.openedTabs).toEqual([GITHUB_SIGN_OUT_URL]))
    // This tab stays on the login screen, ready for the other account.
    expect(navigation.destinations).toEqual([])
    expect(screen.getByRole('button', { name: '使用 GitHub 登录' })).toBeInTheDocument()
  })

  it('has no GitHub sign-out when the gateway offers only the developer login', async () => {
    server.use(http.get('/auth/providers', () => HttpResponse.json({ providers: ['dev'] })))
    renderWithProviders(<LoginPage />, { route: '/login' })
    expect(await screen.findByRole('button', { name: /开发者登录/ })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '先退出 GitHub' })).not.toBeInTheDocument()
  })

  it('reports an unreachable gateway when the provider list cannot be loaded', async () => {
    server.use(http.get('/auth/providers', () => HttpResponse.error()))
    renderWithProviders(<LoginPage />, { route: '/login' })
    expect(await screen.findByText(/认证网关不可用/)).toBeInTheDocument()
    expect(screen.queryByRole('button')).not.toBeInTheDocument()
  })

  it('sends an already signed-in tab to returnTo', async () => {
    installSignedInSession()
    renderRoutes(
      [
        { path: '/login', element: <LoginPage /> },
        { path: '/w/acme/issues', element: <div>Issues screen</div> },
      ],
      '/login?returnTo=%2Fw%2Facme%2Fissues',
    )
    expect(await screen.findByText('Issues screen')).toBeInTheDocument()
  })
})
