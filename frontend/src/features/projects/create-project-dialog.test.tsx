import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { GitCredential } from '@/api/generated.schemas'
import { CreateProjectDialog } from '@/features/projects/create-project-dialog'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'

const PROJECT_ID = '55555555-5555-5555-5555-555555555555'
const PROJECTS_URL = `/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`
const CREDENTIALS_URL = `/api/v1/tenants/${TEST_TENANT_ID}/git-credentials`

const availableCredential: GitCredential = {
  id: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
  tenantId: TEST_TENANT_ID,
  ownerUserId: '33333333-3333-3333-3333-333333333333',
  name: 'deploy-key',
  repositoryScope: 'https://example.com/repo.git',
  attribution: 'personal',
  availability: 'available',
  unavailableReason: null,
  capabilities: ['clone'],
  version: 1,
  createdAt: '2026-09-01T10:00:00+08:00',
  revokedAt: null,
}

function installCredentials(items: GitCredential[] = []): void {
  server.use(http.get(CREDENTIALS_URL, () => HttpResponse.json({ items, nextCursor: '' })))
}

function fault(code: string, status: number) {
  return HttpResponse.json({ code, params: {}, requestId: 'r' }, { status })
}

function setupDialog(
  onCreated = vi.fn<(projectId: string) => void>(),
  credentials: GitCredential[] = [],
) {
  installCredentials(credentials)
  renderWithProviders(<CreateProjectDialog open onOpenChange={() => {}} onCreated={onCreated} />, {
    slug: 'cloud-dev',
  })
  return { onCreated }
}

async function fillProject(user: ReturnType<typeof userEvent.setup>, name = 'Demo'): Promise<void> {
  await user.type(screen.getByLabelText('名称'), name)
  await user.type(screen.getByLabelText('仓库 URL（HTTPS 或 SSH）'), 'https://example.com/repo.git')
}

async function chooseCredentialAndSubmit(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByRole('combobox', { name: '凭据' }))
  await user.click(await screen.findByRole('option', { name: /deploy-key/ }))
  await user.click(screen.getByRole('button', { name: '创建' }))
}

function acceptCreation() {
  return HttpResponse.json(
    { resource: { id: PROJECT_ID }, workspace: { id: 'w1' }, operation: { id: 'o1' } },
    { status: 202 },
  )
}

describe('CreateProjectDialog', () => {
  it('creates a public project without a credential and reports its id', async () => {
    installCloudSpaceHandlers('owner')
    let posted: Record<string, unknown> | null = null
    server.use(
      http.post(PROJECTS_URL, async ({ request }) => {
        const body = await request.json()
        if (typeof body === 'object' && body !== null) {
          posted = body as Record<string, unknown>
        }
        return acceptCreation()
      }),
    )
    const { onCreated } = setupDialog()
    const user = userEvent.setup()

    await fillProject(user)
    await user.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(posted).not.toBeNull())
    expect(posted).toEqual({
      name: 'Demo',
      repositoryUrl: 'https://example.com/repo.git',
      defaultBranch: 'main',
    })
    expect(onCreated).toHaveBeenCalledWith(PROJECT_ID)
  })

  it('sends the chosen credential reference with the project', async () => {
    installCloudSpaceHandlers('owner')
    let posted: Record<string, unknown> | null = null
    server.use(
      http.post(PROJECTS_URL, async ({ request }) => {
        const body = await request.json()
        if (typeof body === 'object' && body !== null) posted = body as Record<string, unknown>
        return acceptCreation()
      }),
    )
    setupDialog(vi.fn(), [availableCredential])
    const user = userEvent.setup()

    await fillProject(user)
    await chooseCredentialAndSubmit(user)

    await waitFor(() => expect(posted).not.toBeNull())
    expect(posted).toEqual({
      name: 'Demo',
      repositoryUrl: 'https://example.com/repo.git',
      defaultBranch: 'main',
      credentialRefId: availableCredential.id,
    })
  })

  it.each([
    [
      'repository_credential_frozen',
      '所选凭据当前不可用（已冻结或已删除），请改用其他凭据，或选择“不使用凭据（公开仓库）”',
    ],
    [
      'repository_credential_scope_mismatch',
      '所选凭据的仓库范围与项目仓库 URL 不一致，请重新选择匹配的凭据',
    ],
  ])('explains a %s refusal as an actionable choice', async (code, copy) => {
    installCloudSpaceHandlers('owner')
    server.use(http.post(PROJECTS_URL, () => fault(code, 409)))
    setupDialog(vi.fn(), [availableCredential])
    const user = userEvent.setup()

    await fillProject(user)
    await chooseCredentialAndSubmit(user)

    expect(await screen.findByRole('alert')).toHaveTextContent(copy)
  })

  it('rejects malformed repository URLs before submit', async () => {
    installCloudSpaceHandlers('owner')
    setupDialog()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('名称'), 'Demo')
    await user.type(screen.getByLabelText('仓库 URL（HTTPS 或 SSH）'), 'ftp://example.com/repo')
    expect(screen.getByText('仅支持 https:// 或 ssh:// 地址')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建' })).toBeDisabled()
  })

  it('requires a concrete default branch, not an empty one or HEAD', async () => {
    installCloudSpaceHandlers('owner')
    setupDialog()
    const user = userEvent.setup()

    await fillProject(user)
    const branch = screen.getByLabelText('默认分支')
    await user.clear(branch)
    expect(screen.getByText('请填写具体分支名，不能为空或 HEAD')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '创建' })).toBeDisabled()
    await user.type(branch, 'HEAD')
    expect(screen.getByRole('button', { name: '创建' })).toBeDisabled()
    await user.clear(branch)
    await user.type(branch, 'trunk')
    expect(screen.getByRole('button', { name: '创建' })).toBeEnabled()
  })
})
