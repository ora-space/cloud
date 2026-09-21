import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { ProjectDetailPage } from '@/features/projects/project-detail-page'
import { db } from '@/mocks/data/store'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'

const PROJECT_ID = '55555555-5555-5555-5555-555555555555'

function cloudProject(name: string, lifecycle = 'active', version = 1) {
  return {
    id: PROJECT_ID,
    tenantId: TEST_TENANT_ID,
    ownerUserId: 'u1',
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
  )
}

function renderDetail() {
  return renderAtRoute(
    '/:workspaceSlug/projects/:projectId',
    <ProjectDetailPage slug="cloud-dev" />,
    `/cloud-dev/projects/${PROJECT_ID}`,
    { authenticated: true },
  )
}

describe('ProjectDetailPage', () => {
  it('renders the project header and its issues', async () => {
    const project = db.projects[0]
    if (!project) throw new Error('project seed data must not be empty')
    renderAtRoute(
      '/:workspaceSlug/projects/:projectId',
      <ProjectDetailPage slug={db.workspace.slug} />,
      `/${db.workspace.slug}/projects/${project.id}`,
    )

    expect(await screen.findAllByText(project.title)).not.toHaveLength(0)

    const projectIssue = db.issues.find((i) => i.projectId === project.id)
    if (!projectIssue) throw new Error('project seed data must contain an issue')
    expect(await screen.findByText(projectIssue.title)).toBeInTheDocument()
  })
})

describe('ProjectDetailPage cloud mode', () => {
  it('renames the project with the optimistic version', async () => {
    installProjectHandlers('owner')
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
    installProjectHandlers('owner')
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

  it('hides the delete action from members', async () => {
    installProjectHandlers('member')
    renderDetail()
    await screen.findAllByText('Demo')
    expect(screen.getByRole('button', { name: '重命名' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '删除项目' })).not.toBeInTheDocument()
  })
})
