import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { ProjectDetailPage } from '@/features/projects/project-detail-page'
import {
  installCloudSpaceHandlers,
  TEST_SPACE_ID,
  TEST_TENANT_ID,
  TEST_USER_ID,
} from '@/test/cloud-handlers'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'

const PROJECT_ID = '55555555-5555-5555-5555-555555555555'
const WORKSPACE_ID = 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'
const OPERATION_ID = '99999999-9999-9999-9999-999999999999'
const OTHER_USER_ID = '44444444-4444-4444-4444-444444444444'

function cloudProject(name: string, lifecycle = 'active', version = 1, ownerUserId = 'u1') {
  return {
    id: PROJECT_ID,
    tenantId: TEST_TENANT_ID,
    ownerUserId,
    spaceId: TEST_SPACE_ID,
    name,
    repositoryUrl: 'https://example.com/repo.git',
    defaultBranch: 'main',
    credentialRefId: null,
    lifecycle,
    version,
    createdAt: '2026-09-20T10:00:00+08:00',
    deletedAt: null,
  }
}

function installProjectHandlers(role: string, project = cloudProject('Demo')) {
  installCloudSpaceHandlers(role)
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
      HttpResponse.json({ items: [project], nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${PROJECT_ID}`, () =>
      HttpResponse.json(project),
    ),
    // The detail body also loads the project's Workspaces (to reach the
    // provisioning Operation), the tenant issues and the members list. No
    // Workspace means no provisioning answer to render, which is the state
    // every test here except the failure case is about.
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${PROJECT_ID}/workspaces`, () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/issues`, () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/members`, () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
  )
}

function renderDetail() {
  return renderAtRoute(
    '/w/:workspaceSlug/projects/:projectId',
    <ProjectDetailPage slug="cloud-dev" />,
    `/w/cloud-dev/projects/${PROJECT_ID}`,
  )
}

describe('ProjectDetailPage', () => {
  it('renames the project with the optimistic version', async () => {
    installProjectHandlers('admin')
    let patchBody: Record<string, unknown> | null = null
    server.use(
      http.patch(
        `/api/v1/tenants/${TEST_TENANT_ID}/projects/${PROJECT_ID}`,
        async ({ request }) => {
          const body = await request.json()
          if (typeof body === 'object' && body !== null) {
            patchBody = body as Record<string, unknown>
          }
          return HttpResponse.json(cloudProject('Renamed', 'active', 2))
        },
      ),
    )
    renderDetail()
    const user = userEvent.setup()

    await screen.findAllByText('Demo')
    await user.click(screen.getByRole('button', { name: '重命名' }))
    await user.type(screen.getByLabelText('名称'), ' Renamed')
    await user.click(screen.getByRole('button', { name: '保存' }))

    await waitFor(() => expect(patchBody).not.toBeNull())
    expect(patchBody).toEqual({ name: 'Demo Renamed', version: 1 })
  })

  it('deletes the project through the lifecycle state machine', async () => {
    installProjectHandlers('admin')
    let deleted = false
    server.use(
      http.delete(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${PROJECT_ID}`, () => {
        deleted = true
        return HttpResponse.json(
          { resource: cloudProject('Demo', 'deleting', 2), operation: { id: 'o1' } },
          { status: 202 },
        )
      }),
    )
    renderDetail()
    const user = userEvent.setup()

    await screen.findAllByText('Demo')
    await user.click(screen.getByRole('button', { name: '删除项目' }))
    await user.click(await screen.findByRole('button', { name: '确认删除' }))

    await waitFor(() => expect(deleted).toBe(true))
  })

  it('shows why provisioning failed instead of leaving the project looking unfinished', async () => {
    installProjectHandlers('member', cloudProject('Demo', 'provisioning'))
    server.use(
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${PROJECT_ID}/workspaces`, () =>
        HttpResponse.json({ items: [{ id: WORKSPACE_ID, creatorOperationId: OPERATION_ID }] }),
      ),
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/operations/${OPERATION_ID}`, () =>
        HttpResponse.json({
          id: OPERATION_ID,
          tenantId: TEST_TENANT_ID,
          projectId: PROJECT_ID,
          workspaceId: WORKSPACE_ID,
          kind: 'create_project',
          state: 'failed',
          step: 'clone',
          errorCode: 'repository_credential_executor_unavailable',
          version: 2,
        }),
      ),
    )
    renderDetail()

    const notice = await screen.findByRole('alert')
    expect(notice).toHaveTextContent('项目创建失败')
    expect(notice).toHaveTextContent('本部署未安装仓库凭据执行器')
    expect(notice).toHaveTextContent('repository_credential_executor_unavailable')
  })

  it('hides delete from a member even when they created the project', async () => {
    installProjectHandlers('member', cloudProject('Demo', 'active', 1, TEST_USER_ID))
    renderDetail()
    await screen.findAllByText('Demo')
    expect(screen.queryByRole('button', { name: '删除项目' })).not.toBeInTheDocument()
  })

  it('hides delete from a member who is not the creator', async () => {
    installProjectHandlers('member', cloudProject('Demo', 'active', 1, OTHER_USER_ID))
    renderDetail()
    await screen.findAllByText('Demo')
    expect(screen.getByRole('button', { name: '重命名' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '删除项目' })).not.toBeInTheDocument()
  })

  it('shows delete for a workspace admin who is not the creator', async () => {
    installProjectHandlers('admin', cloudProject('Demo', 'active', 1, OTHER_USER_ID))
    renderDetail()
    await screen.findAllByText('Demo')
    expect(screen.getByRole('button', { name: '删除项目' })).toBeInTheDocument()
  })

  it('does not grant delete from a legacy owner role', async () => {
    installProjectHandlers('owner', cloudProject('Demo', 'active', 1, OTHER_USER_ID))
    renderDetail()
    await screen.findAllByText('Demo')
    expect(screen.queryByRole('button', { name: '删除项目' })).not.toBeInTheDocument()
  })
})
