import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { GitCredential } from '@/api/generated.schemas'
import { GitCredentialsPage } from '@/features/settings/git-credentials-page'
import { installCloudSpaceHandlers, TEST_TENANT_ID, TEST_USER_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderWithProviders } from '@/test/render'

const URL = `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`

const available: GitCredential = {
  id: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
  tenantId: TEST_TENANT_ID,
  ownerUserId: TEST_USER_ID,
  name: 'deploy-key',
  repositoryScope: 'https://github.com/acme/app',
  attribution: 'personal',
  availability: 'available',
  unavailableReason: null,
  capabilities: ['clone'],
  version: 2,
  createdAt: '2026-09-01T10:00:00+08:00',
  revokedAt: null,
}

const revoked: GitCredential = {
  ...available,
  id: 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
  name: 'old-key',
  repositoryScope: 'https://github.com/acme/legacy',
  availability: 'invalid',
  unavailableReason: 'revoked_by_owner',
  version: 3,
  revokedAt: '2026-09-02T10:00:00+08:00',
}

const frozen: GitCredential = {
  ...available,
  id: 'cccccccc-cccc-cccc-cccc-cccccccccccc',
  name: 'ci-key',
  availability: 'frozen',
  unavailableReason: 'associated_member_departed',
  version: 4,
}

function installCredentials(items: GitCredential[]): void {
  server.use(http.get(URL, () => HttpResponse.json({ items, nextCursor: '' })))
}

function fault(code: string, status: number) {
  return HttpResponse.json({ code, params: {}, requestId: 'r' }, { status })
}

function renderPage() {
  installCloudSpaceHandlers('owner')
  return renderWithProviders(<GitCredentialsPage />, { slug: 'cloud-dev' })
}

/** Fills and submits the create form with the fixture values the tests assert. */
async function submitCreateForm(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.type(await screen.findByLabelText('名称'), 'new-key')
  await user.type(screen.getByLabelText('仓库范围'), 'https://github.com/acme/app')
  await user.type(screen.getByLabelText('Token'), 'secret-token')
  await user.click(screen.getByRole('button', { name: '添加凭据' }))
}

describe('GitCredentialsPage', () => {
  it('shows a loading indicator before the credentials resolve', async () => {
    installCredentials([])
    renderPage()

    expect(screen.getByRole('status', { name: '正在加载 Git 凭据' })).toBeInTheDocument()
    expect(await screen.findByText('暂无凭据，请在上方添加。')).toBeInTheDocument()
  })

  it('says so when the caller has no credentials yet', async () => {
    installCredentials([])
    renderPage()

    expect(await screen.findByText('暂无凭据，请在上方添加。')).toBeInTheDocument()
    expect(screen.getByText('Git 凭据')).toBeInTheDocument()
  })

  it('lists each credential with its scope and availability badge', async () => {
    installCredentials([available, revoked])
    renderPage()

    expect(await screen.findByText('deploy-key')).toBeInTheDocument()
    expect(screen.getByText('https://github.com/acme/app')).toBeInTheDocument()
    expect(screen.getByText('可用')).toBeInTheDocument()
    expect(screen.getByText('old-key')).toBeInTheDocument()
    expect(screen.getByText('已撤销')).toBeInTheDocument()
  })

  it('renders a revoked credential as revoked instead of dropping it', async () => {
    installCredentials([revoked])
    renderPage()

    expect(await screen.findByText('old-key')).toBeInTheDocument()
    expect(screen.getByText('已撤销')).toBeInTheDocument()
    expect(screen.getByText(/已被你撤销/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '轮换' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '撤销' })).toBeDisabled()
  })

  it('disables rotation for a frozen credential and explains why', async () => {
    installCredentials([frozen])
    renderPage()

    expect(await screen.findByText('已冻结')).toBeInTheDocument()
    expect(screen.getByText(/因不再是有效成员被冻结/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '轮换' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '撤销' })).toBeDisabled()
  })

  it('creates a credential with the exact body and an idempotency key', async () => {
    const posts: { body: unknown; key: string | null }[] = []
    installCredentials([])
    server.use(
      http.post(URL, async ({ request }) => {
        posts.push({ body: await request.json(), key: request.headers.get('Idempotency-Key') })
        return HttpResponse.json(
          { ...available, name: 'new-key', repositoryScope: 'https://github.com/acme/app' },
          { status: 201 },
        )
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await submitCreateForm(user)

    expect(await screen.findByText('已添加')).toBeInTheDocument()
    expect(posts).toEqual([
      {
        body: {
          name: 'new-key',
          repositoryScope: 'https://github.com/acme/app',
          token: 'secret-token',
        },
        key: expect.any(String),
      },
    ])
    // The token is never rendered back after the request.
    expect(screen.queryByDisplayValue('secret-token')).not.toBeInTheDocument()
  })

  it('explains a version conflict on rotate and keeps the typed token', async () => {
    const puts: unknown[] = []
    installCredentials([available])
    server.use(
      http.put(`${URL}/${available.id}`, async ({ request }) => {
        puts.push(await request.json())
        return fault('version_conflict', 409)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    const token = await screen.findByLabelText('新 Token')
    await user.type(token, 'rotated-secret')
    await user.click(screen.getByRole('button', { name: '轮换' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('凭据已在别处修改，请刷新后重试')
    expect(puts).toEqual([{ token: 'rotated-secret', version: 2 }])
    expect(screen.getByLabelText('新 Token')).toHaveValue('rotated-secret')
  })

  it('explains a version conflict on revoke without removing the row', async () => {
    installCredentials([available])
    server.use(http.delete(`${URL}/${available.id}`, () => fault('version_conflict', 409)))
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('deploy-key')
    await user.click(screen.getByRole('button', { name: '撤销' }))

    expect(await screen.findByRole('alert')).toHaveTextContent('凭据已在别处修改，请刷新后重试')
    expect(screen.getByText('deploy-key')).toBeInTheDocument()
    expect(screen.queryByText('已撤销')).not.toBeInTheDocument()
  })

  it('reports the deployment-unconfigured copy when the store is unavailable', async () => {
    installCredentials([])
    server.use(http.post(URL, () => fault('credential_store_unavailable', 503)))
    const user = userEvent.setup()
    renderPage()

    await submitCreateForm(user)

    expect(await screen.findByRole('alert')).toHaveTextContent(
      '当前部署未配置凭据服务，暂时无法保存 Git 凭据',
    )
    expect(screen.queryByText('已添加')).not.toBeInTheDocument()
    await waitFor(() => expect(screen.getByLabelText('名称')).toHaveValue('new-key'))
  })

  it('reports a failed list load', async () => {
    server.use(http.get(URL, () => fault('internal', 500)))
    renderPage()

    expect(await screen.findByText('Git 凭据加载失败')).toBeInTheDocument()
  })
})
