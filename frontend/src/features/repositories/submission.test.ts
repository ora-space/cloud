import { afterEach, describe, expect, it, vi } from 'vitest'
import type { GitCredentialSubmission } from '@/api/generated.schemas'
import { submitGitToken } from './submission'

const INGRESS = 'https://credentials.example.invalid/submissions/one'

/** One submission grant, as the Credential Store's browser entry hands it out. */
function submission(overrides: Partial<GitCredentialSubmission> = {}): GitCredentialSubmission {
  return {
    submissionId: '11111111-1111-4111-8111-111111111111',
    ingressUrl: INGRESS,
    grant: 'grant-value',
    storeVersion: 2,
    expiresAt: '2026-10-17T12:00:00+08:00',
    ...overrides,
  }
}

/** Stubs fetch so the test sees the exact request the adapter builds, and nothing leaves jsdom. */
function stubFetch(response: Response) {
  return vi.spyOn(globalThis, 'fetch').mockResolvedValue(response)
}

afterEach(() => {
  vi.restoreAllMocks()
})

describe('submitGitToken', () => {
  it('PUTs the token to the Store and nothing else', async () => {
    const fetchSpy = stubFetch(new Response(null, { status: 204 }))
    await submitGitToken(submission(), 'ghp-secret')
    // The whole call, because each field is a promise the module makes: the token goes to the
    // Store's own origin, the grant travels in its own header, the request carries no gateway
    // cookie, no redirect may move it elsewhere, and the referrer never leaks where it came from.
    expect(fetchSpy.mock.calls).toEqual([
      [
        new URL(INGRESS),
        {
          method: 'PUT',
          credentials: 'omit',
          redirect: 'error',
          referrerPolicy: 'no-referrer',
          headers: {
            'Content-Type': 'application/json',
            Authorization: 'Ora-Submission grant-value',
          },
          body: JSON.stringify({ token: 'ghp-secret' }),
        },
      ],
    ])
  })

  it('refuses a plaintext ingress before it sends the token', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    await expect(
      submitGitToken(submission({ ingressUrl: 'http://credentials.example.invalid/s' }), 't'),
    ).rejects.toThrow('凭据入口必须使用 HTTPS')
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('refuses an ingress that carries credentials in the URL', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    await expect(
      submitGitToken(
        submission({ ingressUrl: 'https://user:pass@credentials.example.invalid/s' }),
        't',
      ),
    ).rejects.toThrow('凭据入口必须使用 HTTPS')
    expect(fetchSpy).not.toHaveBeenCalled()
  })

  it('reports a Store that refused the token instead of pretending it was stored', async () => {
    stubFetch(new Response('no', { status: 403 }))
    await expect(submitGitToken(submission(), 't')).rejects.toThrow(
      '凭据提交失败，请重新申请提交授权',
    )
  })

  it('treats a server error the same as a refusal, so the caller re-asks for a grant', async () => {
    stubFetch(new Response('boom', { status: 503 }))
    await expect(submitGitToken(submission(), 't')).rejects.toThrow(
      '凭据提交失败，请重新申请提交授权',
    )
  })
})
