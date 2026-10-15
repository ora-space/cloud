import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { ProjectsPage } from './projects-page'

const PROJECT_ID = '55555555-5555-5555-5555-555555555555'
const WORKSPACE_ID = 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'
const OPERATION_ID = '99999999-9999-9999-9999-999999999999'

function cloudProject(id: string, name: string, lifecycle = 'active') {
  return {
    id,
    tenantId: TEST_TENANT_ID,
    ownerUserId: 'u1',
    spaceId: TEST_SPACE_ID,
    name,
    repositoryUrl: `https://example.com/${name}.git`,
    defaultBranch: 'main',
    credentialRefId: null,
    lifecycle,
    version: 1,
    createdAt: '2026-09-20T10:00:00+08:00',
    deletedAt: null,
  }
}

/** Installs the project list plus, for one project, its provisioning Operation. */
function installProjects(projects: unknown[], provisioning?: { id: string; state: string }) {
  installCloudSpaceHandlers('member')
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
      HttpResponse.json({ items: projects, nextCursor: '' }),
    ),
  )
  if (!provisioning) {
    return
  }
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${provisioning.id}/workspaces`, () =>
      HttpResponse.json({
        items: [{ id: WORKSPACE_ID, kind: 'main', creatorOperationId: OPERATION_ID }],
      }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/operations/${OPERATION_ID}`, () =>
      HttpResponse.json({
        id: OPERATION_ID,
        tenantId: TEST_TENANT_ID,
        projectId: provisioning.id,
        workspaceId: WORKSPACE_ID,
        kind: 'create_project',
        state: provisioning.state,
        step: 'node',
        errorCode: provisioning.state === 'failed' ? 'mirror_preparation_failed' : null,
        version: 2,
      }),
    ),
  )
}

describe('ProjectsPage', () => {
  it('renders every project of the resolved space as a card', async () => {
    installCloudSpaceHandlers('member')
    server.use(
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
        HttpResponse.json({
          items: [
            cloudProject('55555555-5555-5555-5555-555555555555', 'Alpha'),
            cloudProject('66666666-6666-6666-6666-666666666666', 'Beta'),
          ],
          nextCursor: '',
        }),
      ),
    )
    renderWithProviders(<ProjectsPage slug="cloud-dev" />, { slug: 'cloud-dev' })

    expect(await screen.findByText('Alpha')).toBeInTheDocument()
    expect(await screen.findByText('Beta')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /新建项目/ })).toBeInTheDocument()
  })

  it('shows a project whose creation failed as failed instead of as planned', async () => {
    installProjects([cloudProject(PROJECT_ID, 'Alpha', 'provisioning')], {
      id: PROJECT_ID,
      state: 'failed',
    })
    renderWithProviders(<ProjectsPage slug="cloud-dev" />, { slug: 'cloud-dev' })

    expect(await screen.findByText('创建失败')).toBeInTheDocument()
    expect(screen.queryByText('规划中')).not.toBeInTheDocument()
  })

  it('shows a creation that is being retried apart from a terminal failure', async () => {
    installProjects([cloudProject(PROJECT_ID, 'Alpha', 'provisioning')], {
      id: PROJECT_ID,
      state: 'retry_wait',
    })
    renderWithProviders(<ProjectsPage slug="cloud-dev" />, { slug: 'cloud-dev' })

    expect(await screen.findByText('重试中')).toBeInTheDocument()
    expect(screen.queryByText('创建失败')).not.toBeInTheDocument()
  })

  it('asks about provisioning only for a project that is still being created', async () => {
    // No Workspace handler is installed for this project: a card that asked
    // about a settled project would be an unhandled request, which the test
    // server fails on (`onUnhandledRequest: 'error'`).
    installProjects([cloudProject(PROJECT_ID, 'Alpha')])
    renderWithProviders(<ProjectsPage slug="cloud-dev" />, { slug: 'cloud-dev' })

    expect(await screen.findByText('进行中')).toBeInTheDocument()
  })
})
