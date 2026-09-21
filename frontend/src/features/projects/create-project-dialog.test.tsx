import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { CreateProjectDialog } from '@/features/projects/create-project-dialog'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'

const PROJECT_ID = '55555555-5555-5555-5555-555555555555'

function setupDialog(onCreated = vi.fn<(projectId: string) => void>()) {
  renderWithProviders(<CreateProjectDialog open onOpenChange={() => {}} onCreated={onCreated} />, {
    slug: 'cloud-dev',
    authenticated: true,
  })
  return { onCreated }
}

describe('CreateProjectDialog', () => {
  it('creates a project in the current space and reports its id', async () => {
    installCloudSpaceHandlers('owner')
    let posted: Record<string, unknown> | null = null
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`,
        async ({ request }) => {
          const body = await request.json()
          if (typeof body === 'object' && body !== null) {
            posted = body as Record<string, unknown>
          }
          return HttpResponse.json(
            { resource: { id: PROJECT_ID }, workspace: { id: 'w1' }, operation: { id: 'o1' } },
            { status: 202 },
          )
        },
      ),
    )
    const { onCreated } = setupDialog()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('名称'), 'Demo')
    await user.type(
      screen.getByLabelText('仓库 URL（HTTPS 或 SSH）'),
      'https://example.com/repo.git',
    )
    await user.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(posted).not.toBeNull())
    expect(posted).toMatchObject({ name: 'Demo', repositoryUrl: 'https://example.com/repo.git' })
    expect(onCreated).toHaveBeenCalledWith(PROJECT_ID)
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
})
