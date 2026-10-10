import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { makeIssue } from '@/test/issue-fixtures'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { CreateIssueDialog } from './create-issue-dialog'

function serve() {
  installCloudSpaceHandlers('admin')
  let body: unknown
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/issues`, () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`, () =>
      HttpResponse.json({
        items: [
          {
            id: 'project-1',
            tenantId: TEST_TENANT_ID,
            spaceId: TEST_SPACE_ID,
            name: 'Cloned repository',
            repositoryUrl: 'https://github.com/ora-space/marketplace',
            defaultBranch: 'main',
            ownerUserId: 'u1',
            lifecycle: 'active',
            version: 1,
            createdAt: '2026-10-01T00:00:00Z',
            updatedAt: '2026-10-01T00:00:00Z',
          },
        ],
        nextCursor: '',
      }),
    ),
    http.post(`/api/v1/tenants/${TEST_TENANT_ID}/issues`, async ({ request }) => {
      body = await request.json()
      return HttpResponse.json({ resource: makeIssue('i1', 'Task') }, { status: 201 })
    }),
  )
  renderWithProviders(<CreateIssueDialog slug={TEST_TENANT_ID} />, { slug: 'cloud-dev' })
  return () => body
}

describe('CreateIssueDialog project association', () => {
  it('creates an associated task using a project from the current real space', async () => {
    const request = serve()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '新建任务' }))
    await user.click(screen.getByPlaceholderText('任务标题'))
    await user.paste('Repository task')
    await user.click(await screen.findByRole('combobox', { name: '项目' }))
    await user.click(await screen.findByRole('option', { name: 'Cloned repository' }))
    await user.click(screen.getByRole('button', { name: '创建任务' }))
    await waitFor(() =>
      expect(request()).toEqual({
        title: 'Repository task',
        description: '',
        status: 'backlog',
        priority: 'none',
        projectRef: 'project-1',
      }),
    )
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    await user.click(screen.getByRole('button', { name: '新建任务' }))
    expect(screen.getByRole('combobox', { name: '项目' })).toHaveTextContent('未关联项目')
  })

  it('keeps project association optional when the user has not selected one', async () => {
    const request = serve()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: '新建任务' }))
    await user.click(screen.getByPlaceholderText('任务标题'))
    await user.paste('Plain task')
    await user.click(screen.getByRole('button', { name: '创建任务' }))
    await waitFor(() =>
      expect(request()).toEqual({
        title: 'Plain task',
        description: '',
        status: 'backlog',
        priority: 'none',
      }),
    )
  })
})
