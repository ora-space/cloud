import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createMemoryRouter } from 'react-router'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from '@/app/app'
import { routes } from '@/app/routes'
import { demoUser, fakeBackend } from '@/test/backend'
import { navigation } from './navigation'

const query = new URLSearchParams({
  redirect_uri: 'http://127.0.0.1:53682/callback',
  state: 'st-1',
  code_challenge: 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM',
  code_challenge_method: 'S256',
}).toString()

const renderAt = (path: string) => {
  const router = createMemoryRouter(routes, { initialEntries: [path] })
  render(<App router={router} />)
  return router
}

afterEach(() => vi.restoreAllMocks())

describe('desktop authorize', () => {
  it('issues nothing until 允许, then sends the browser back to the desktop', async () => {
    const posted: string[] = []
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/auth/desktop/approve') {
        posted.push(String(c.data))
        return { status: 200, data: { redirectUrl: 'http://127.0.0.1:53682/callback?code=c&state=st-1' } }
      }
      return undefined
    })
    const assign = vi.spyOn(navigation, 'assign').mockImplementation(() => {})
    renderAt(`/desktop/authorize?${query}`)
    expect(await screen.findByText(/允许 Ora desktop 以/)).toHaveTextContent('「演示用户」')
    expect(posted).toHaveLength(0)

    await userEvent.click(screen.getByRole('button', { name: '允许' }))
    await waitFor(() => expect(assign).toHaveBeenCalledWith('http://127.0.0.1:53682/callback?code=c&state=st-1'))
    expect(JSON.parse(posted[0])).toEqual({
      redirectUri: 'http://127.0.0.1:53682/callback',
      state: 'st-1',
      codeChallenge: 'E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM',
      codeChallengeMethod: 'S256',
    })
  })

  it('cancels without asking the server', async () => {
    const seen = fakeBackend((c) => (c.url === '/api/v1/me' ? { status: 200, data: demoUser } : undefined))
    renderAt(`/desktop/authorize?${query}`)
    await userEvent.click(await screen.findByRole('button', { name: '取消' }))
    expect(screen.getByText(/已取消/)).toBeInTheDocument()
    expect(seen).not.toContain('POST /auth/desktop/approve')
  })

  it('refuses a link the server rejects and does not navigate', async () => {
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 200, data: demoUser }
      if (c.url === '/auth/desktop/approve') return { status: 400, data: { code: 'invalid_redirect_uri', params: {}, requestId: 'r' } }
      return undefined
    })
    const assign = vi.spyOn(navigation, 'assign').mockImplementation(() => {})
    renderAt(`/desktop/authorize?${query}`)
    await userEvent.click(await screen.findByRole('button', { name: '允许' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('不是 Ora desktop 发起的')
    expect(assign).not.toHaveBeenCalled()
  })

  it('explains an incomplete link instead of offering 允许', async () => {
    fakeBackend((c) => (c.url === '/api/v1/me' ? { status: 200, data: demoUser } : undefined))
    renderAt('/desktop/authorize?state=st-1')
    expect(await screen.findByRole('alert')).toHaveTextContent('登录链接不完整')
    expect(screen.queryByRole('button', { name: '允许' })).not.toBeInTheDocument()
  })

  it('signs a visitor in first and returns to the same request', async () => {
    const posted: string[] = []
    fakeBackend((c) => {
      if (c.url === '/api/v1/me') return { status: 401, data: {} }
      if (c.url === '/auth/providers') return { status: 200, data: { providers: ['huawei-idaas'] } }
      if (c.url === '/auth/login') return posted.push(String(c.data)), { status: 200, data: { authorizationUrl: 'about:blank' } }
      return undefined
    })
    renderAt(`/desktop/authorize?${query}`)
    await userEvent.click(await screen.findByRole('button', { name: '使用华为账号登录' }))
    await waitFor(() => expect(posted).toHaveLength(1))
    expect(JSON.parse(posted[0]).returnTo).toBe(`/desktop/authorize?${query}`)
  })
})
