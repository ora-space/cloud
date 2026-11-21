import { http } from './api-client'

// The Gateway's browser login routes. They are not part of api/openapi.json, so they are written by
// hand; v2-server serves the same shapes with the login methods its V2_LOGIN names.

/** The offered login methods, in the order the login screen shows them. */
export interface Providers {
  providers: string[]
}

export const getProviders = () => http.get<Providers>('/auth/providers').then((r) => r.data)

/**
 * Starts a login; the browser then navigates to the returned URL. The email provider needs the
 * address it signs in; every other provider carries its own flow.
 */
export const startLogin = (provider: string, returnTo: string, email?: string) =>
  http
    .post<{ authorizationUrl: string }>('/auth/login', { provider, returnTo, ...(email === undefined ? {} : { email }) })
    .then((r) => r.data.authorizationUrl)

export const logout = () => http.post('/auth/logout').then(() => undefined)

/** What `/desktop/authorize` received from Ora desktop, forwarded unchanged to the approval. */
export interface DesktopApproval {
  redirectUri: string
  state: string
  codeChallenge: string
  codeChallengeMethod: string
}

/**
 * Approves an Ora desktop login with this browser's session (A6 D6). The answer is the desktop's
 * loopback address carrying a one-time code, where the browser goes next.
 */
export const approveDesktopLogin = (approval: DesktopApproval) =>
  http.post<{ redirectUrl: string }>('/auth/desktop/approve', approval).then((r) => r.data.redirectUrl)
