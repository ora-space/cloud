import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { GitCredential, GitCredentialSubmission } from '@/api/generated.schemas'
import { server } from '@/test/msw-server'
import {
  gitCredentialFixture,
  installCloudSpaceHandlers,
  TEST_CREDENTIAL_ID,
  TEST_TENANT_ID,
} from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { GitCredentialsPage } from './credential-page'

const OTHER_USER_ID = 'cccccccc-cccc-cccc-cccc-cccccccccccc'
const INGRESS = 'https://credentials.example.invalid/submissions/one'
const TOKEN = 'ghp-must-never-reach-cloud'

/** One browser submission grant, as creating or rotating a credential returns it. */
function submissionFixture(
  overrides: Partial<GitCredentialSubmission> = {},
): GitCredentialSubmission {
  return {
    submissionId: '11111111-1111-4111-8111-111111111111',
    ingressUrl: INGRESS,
    grant: 'grant-value',
    storeVersion: 2,
    expiresAt: '2026-10-17T12:00:00+08:00',
    ...overrides,
  }
}

const CONFIRM = `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials/${TEST_CREDENTIAL_ID}/confirm`

/** Fills the create form and asks Cloud for a submission grant. */
async function askForGrant() {
  await userEvent.type(await screen.findByLabelText('凭据名称'), 'deploy key')
  await userEvent.type(screen.getByLabelText('仓库作用域'), 'https://example.invalid/r.git')
  await userEvent.click(screen.getByRole('button', { name: '申请提交授权' }))
}

/** Types the token into the submission form and hands it to the Store. */
async function handTokenToStore() {
  await userEvent.type(await screen.findByLabelText('Git token'), TOKEN)
  await userEvent.click(screen.getByRole('button', { name: '提交凭据' }))
}

/** Records the JSON bodies one route receives, so a test can compare complete requests. */
function recordPost(path: string, bodies: unknown[], status = 200) {
  server.use(
    http.post(path, async ({ request }) => {
      bodies.push(await request.json())
      return HttpResponse.json({ resource: gitCredentialFixture() }, { status })
    }),
  )
}

function renderPage({
  role = 'member',
  credentials = [gitCredentialFixture()],
}: { role?: string; credentials?: GitCredential[] } = {}) {
  installCloudSpaceHandlers(role)
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`, () =>
      HttpResponse.json({ items: credentials, nextCursor: '' }),
    ),
  )
  return renderWithProviders(<GitCredentialsPage />, { slug: 'cloud-dev' })
}

describe('GitCredentialsPage', () => {
  it('reports each credential’s state, proven capabilities and Store version', async () => {
    renderPage({
      credentials: [
        gitCredentialFixture({
          lastVerification: {
            id: '99999999-9999-4999-8999-999999999999',
            state: 'succeeded',
            result: 'verified',
            storeVersion: 1,
            createdAt: '2026-10-17T10:00:00+08:00',
            finishedAt: '2026-10-17T10:00:05+08:00',
          },
        }),
      ],
    })
    expect(
      await screen.findByText('deploy key · https://example.invalid/r.git · stored / available'),
    ).toBeInTheDocument()
    expect(screen.getByText(/已验证能力：read、publish · 版本 1 · verified/)).toBeInTheDocument()
  })

  it('offers a member only personal attribution and an administrator the team one', async () => {
    renderPage()
    expect(await screen.findByRole('option', { name: '个人' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: '团队' })).not.toBeInTheDocument()
  })

  it('offers an administrator team attribution as well', async () => {
    renderPage({ role: 'admin' })
    expect(await screen.findByRole('option', { name: '团队' })).toBeInTheDocument()
  })

  it('sends the token to the Store and confirms only metadata with Cloud', async () => {
    const created: unknown[] = []
    const confirmed: unknown[] = []
    const stored: { grant: string | null; body: unknown }[] = []
    renderPage()
    server.use(
      http.post(`/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`, async ({ request }) => {
        created.push(await request.json())
        return HttpResponse.json(
          { resource: gitCredentialFixture(), submission: submissionFixture() },
          { status: 201 },
        )
      }),
      http.put(INGRESS, async ({ request }) => {
        stored.push({ grant: request.headers.get('authorization'), body: await request.json() })
        return new HttpResponse(null, { status: 204 })
      }),
    )
    recordPost(CONFIRM, confirmed)

    await askForGrant()
    await waitFor(() =>
      expect(created).toEqual([
        {
          name: 'deploy key',
          repositoryScope: 'https://example.invalid/r.git',
          attribution: 'personal',
          requestedCapabilities: ['read', 'publish'],
        },
      ]),
    )

    await handTokenToStore()
    await waitFor(() => expect(confirmed).toHaveLength(1))

    expect(stored).toEqual([{ grant: 'Ora-Submission grant-value', body: { token: TOKEN } }])
    expect(confirmed).toEqual([{ submissionId: submissionFixture().submissionId }])
    // The token is the one thing Cloud must never see, in any request the page makes.
    expect(JSON.stringify([...created, ...confirmed])).not.toContain(TOKEN)
  })

  it('leaves the credential unconfirmed when the Store refused the token', async () => {
    const confirmed: unknown[] = []
    renderPage()
    server.use(
      http.post(`/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`, () =>
        HttpResponse.json(
          { resource: gitCredentialFixture(), submission: submissionFixture() },
          { status: 201 },
        ),
      ),
      http.put(INGRESS, () => new HttpResponse('no', { status: 403 })),
    )
    recordPost(CONFIRM, confirmed)

    await askForGrant()
    await handTokenToStore()

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '提交或确认失败，请检查凭据状态后重新申请授权',
    )
    expect(confirmed).toEqual([])
  })

  it('rotates with the version the row was read at and asks for the new secret', async () => {
    const rotated: unknown[] = []
    renderPage({ credentials: [gitCredentialFixture({ version: 4 })] })
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials/${TEST_CREDENTIAL_ID}/rotate`,
        async ({ request }) => {
          rotated.push(await request.json())
          return HttpResponse.json({
            resource: gitCredentialFixture({ version: 5 }),
            submission: submissionFixture(),
          })
        },
      ),
    )

    await userEvent.click(await screen.findByRole('button', { name: '轮换 token' }))
    await waitFor(() => expect(rotated).toEqual([{ version: 4 }]))
    expect(
      screen.getByText(/token 将直接发送到 https:\/\/credentials\.example\.invalid，不经过 Cloud/),
    ).toBeInTheDocument()
  })

  it('revokes with the version the row was read at', async () => {
    const revoked: unknown[] = []
    renderPage({ credentials: [gitCredentialFixture({ version: 4 })] })
    server.use(
      http.delete(
        `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials/${TEST_CREDENTIAL_ID}`,
        async ({ request }) => {
          revoked.push(await request.json())
          return HttpResponse.json({ resource: gitCredentialFixture() })
        },
      ),
    )

    await userEvent.click(await screen.findByRole('button', { name: '撤销凭据' }))
    await waitFor(() => expect(revoked).toEqual([{ version: 4 }]))
  })

  it('asks for a verification only once a secret is stored', async () => {
    renderPage({ credentials: [gitCredentialFixture({ secretState: 'awaiting_secret' })] })
    expect(await screen.findByRole('button', { name: '验证推送能力' })).toBeDisabled()
  })

  it('reads every page of the list, at the page size Cloud accepts', async () => {
    const CURSOR = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa'
    const requested: string[] = []
    installCloudSpaceHandlers('member')
    server.use(
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`, ({ request }) => {
        const after = new URL(request.url).searchParams.get('after') ?? ''
        requested.push(`${new URL(request.url).searchParams.get('limit')}|${after}`)
        return HttpResponse.json(
          after
            ? { items: [gitCredentialFixture({ name: 'second key' })], nextCursor: '' }
            : { items: [gitCredentialFixture()], nextCursor: CURSOR },
        )
      }),
    )
    renderWithProviders(<GitCredentialsPage />, { slug: 'cloud-dev' })

    expect(await screen.findByText(/deploy key/)).toBeInTheDocument()
    // The page bound is the contract's, not a bigger number that the backend rejects with
    // 400 invalid_pagination; the second page is read by following the cursor it returned.
    await waitFor(() => expect(requested).toEqual(['100|', `100|${CURSOR}`]))
    expect(await screen.findByText(/second key/)).toBeInTheDocument()
  })

  it('asks for a verification of a stored credential', async () => {
    const verified: unknown[] = []
    renderPage()
    recordPost(
      `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials/${TEST_CREDENTIAL_ID}/verifications`,
      verified,
      202,
    )
    await userEvent.click(await screen.findByRole('button', { name: '验证推送能力' }))
    await waitFor(() => expect(verified).toEqual([{}]))
  })

  it('hides rotation and verification from a member who does not own a personal credential', async () => {
    renderPage({
      credentials: [gitCredentialFixture({ ownerUserId: OTHER_USER_ID })],
    })
    expect(await screen.findByText(/deploy key/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '轮换 token' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '验证推送能力' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '撤销凭据' })).not.toBeInTheDocument()
  })

  it('still lets an administrator revoke a member’s personal credential, which tightens access', async () => {
    renderPage({
      role: 'admin',
      credentials: [gitCredentialFixture({ ownerUserId: OTHER_USER_ID })],
    })
    // The backend allows this and nothing else: an admin may revoke but never rotate or verify
    // somebody else's personal credential.
    expect(await screen.findByRole('button', { name: '撤销凭据' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '轮换 token' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '验证推送能力' })).not.toBeInTheDocument()
  })

  it('offers nothing on a revoked credential', async () => {
    renderPage({ credentials: [gitCredentialFixture({ secretState: 'revoked' })] })
    expect(await screen.findByText(/revoked/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '轮换 token' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '撤销凭据' })).not.toBeInTheDocument()
  })

  it('surfaces the fault code the backend refused a credential action with', async () => {
    renderPage()
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials/${TEST_CREDENTIAL_ID}/rotate`,
        () => HttpResponse.json({ code: 'credential_owner_required' }, { status: 403 }),
      ),
    )
    await userEvent.click(await screen.findByRole('button', { name: '轮换 token' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('credential_owner_required')
  })
})
