import { http, HttpResponse } from 'msw'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import { Route, Routes } from 'react-router-dom'
import { LoginPage } from '@/features/auth/login-page'
import { server } from '@/test/msw-server'
import { renderWithProviders } from '@/test/render'

const fault = { code: 'unauthenticated', params: {}, requestId: 'request-1' }
const currentUser = {
  id: '00000000-0000-4000-8000-000000000001',
  displayName: 'Wang Longan',
  status: 'active',
  version: 1,
  createdAt: '2026-09-21T00:00:00Z',
  deletedAt: null,
}

function installCurrentUserFailure(status: number, code: string): () => number {
  let starts = 0
  server.use(
    http.get('/api/v1/me', () => HttpResponse.json({ ...fault, code }, { status })),
    http.post('/auth/login', () => {
      starts += 1
      return HttpResponse.json({ authorizationUrl: 'https://example.com' })
    }),
  )
  return () => starts
}

describe('LoginPage', () => {
  it('automatically starts IDaaS login and preserves the safe target', async () => {
    let requestedReturnTo = ''
    server.use(
      http.get('/api/v1/me', () => HttpResponse.json(fault, { status: 401 })),
      http.post('/auth/login', async ({ request }) => {
        const body: unknown = await request.json()
        if (typeof body !== 'object' || body === null || !('returnTo' in body)) {
          return HttpResponse.json({ code: 'invalid_request' }, { status: 400 })
        }
        requestedReturnTo = String(body.returnTo)
        return HttpResponse.json({ authorizationUrl: 'https://uniportal.huawei.com/authorize' })
      }),
    )
    const replaceLocation = vi.fn<(target: string) => void>()

    renderWithProviders(<LoginPage replaceLocation={replaceLocation} />, {
      route: '/login?returnTo=%2Fora%2Fissues%3Ftab%3Dmine',
    })

    expect(await screen.findByText('正在跳转华为统一登录…')).toBeInTheDocument()
    await waitFor(() => {
      expect(replaceLocation).toHaveBeenCalledWith('https://uniportal.huawei.com/authorize')
    })
    expect(requestedReturnTo).toBe('/ora/issues?tab=mine')
  })

  it('stops after a login-start failure and retries only on user action', async () => {
    let starts = 0
    server.use(
      http.get('/api/v1/me', () => HttpResponse.json(fault, { status: 401 })),
      http.post('/auth/login', () => {
        starts += 1
        return HttpResponse.json({ code: 'login_unavailable' }, { status: 503 })
      }),
    )
    const user = userEvent.setup()
    renderWithProviders(<LoginPage replaceLocation={vi.fn<(target: string) => void>()} />, {
      route: '/login',
    })

    const retry = await screen.findByRole('button', { name: '重新登录' })
    expect(starts).toBe(1)
    await user.click(retry)
    await waitFor(() => expect(starts).toBe(2))
  })

  it('shows a disabled account without starting another login', async () => {
    const loginStarts = installCurrentUserFailure(403, 'user_disabled')

    renderWithProviders(<LoginPage replaceLocation={vi.fn<(target: string) => void>()} />, {
      route: '/login',
    })

    expect(await screen.findByText('账号已被停用')).toBeInTheDocument()
    expect(loginStarts()).toBe(0)
  })

  it('returns an already authenticated user to the requested target', async () => {
    server.use(http.get('/api/v1/me', () => HttpResponse.json(currentUser)))

    renderWithProviders(
      <Routes>
        <Route
          path="/login"
          element={<LoginPage replaceLocation={vi.fn<(target: string) => void>()} />}
        />
        <Route path="/ora/issues" element={<div>Target page</div>} />
      </Routes>,
      { route: '/login?returnTo=%2Fora%2Fissues' },
    )

    expect(await screen.findByText('Target page')).toBeInTheDocument()
  })

  it('does not start login while current-user lookup is unavailable', async () => {
    const loginStarts = installCurrentUserFailure(503, 'unavailable')

    renderWithProviders(<LoginPage replaceLocation={vi.fn<(target: string) => void>()} />, {
      route: '/login',
    })

    expect(await screen.findByRole('button', { name: '重新检查' })).toBeInTheDocument()
    expect(loginStarts()).toBe(0)
  })
})
