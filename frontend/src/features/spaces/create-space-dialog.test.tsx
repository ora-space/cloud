import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { CreateSpaceDialog } from '@/features/spaces/create-space-dialog'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'

const TENANT_ID = '11111111-1111-1111-1111-111111111111'

function setupDialog(onCreated = vi.fn<(slug: string) => void>()) {
  renderWithProviders(
    <CreateSpaceDialog open onOpenChange={() => {}} tenantId={TENANT_ID} onCreated={onCreated} />,
  )
  return { onCreated }
}

/** Narrows the untrusted request body to a property bag in the handler. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null
}

describe('CreateSpaceDialog', () => {
  it('creates a space through the cloud API and reports the normalized slug', async () => {
    let postedName = ''
    let postedSlug = ''
    server.use(
      http.post(`/api/v1/tenants/${TENANT_ID}/spaces`, async ({ request }) => {
        const body = await request.json()
        if (isRecord(body)) {
          postedName = typeof body['name'] === 'string' ? body['name'] : ''
          postedSlug = typeof body['slug'] === 'string' ? body['slug'] : ''
        }
        return HttpResponse.json({
          id: '22222222-2222-2222-2222-222222222222',
          tenantId: TENANT_ID,
          name: postedName,
          slug: postedSlug,
          description: '',
          createdBy: 'u1',
          version: 1,
          createdAt: '2026-09-20T10:00:00+08:00',
          updatedAt: '2026-09-20T10:00:00+08:00',
          archivedAt: null,
        })
      }),
    )
    const { onCreated } = setupDialog()
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('名称'), 'Team Space')
    await user.type(screen.getByLabelText(/标识/), 'TEAM')
    await user.click(screen.getByRole('button', { name: '创建' }))

    await waitFor(() => expect(postedName).toBe('Team Space'))
    expect(postedSlug).toBe('team') // slug is normalized to lowercase
    expect(onCreated).toHaveBeenCalledWith('team')
  })

  it('disables submit until the name and a valid slug are present', async () => {
    setupDialog()
    const user = userEvent.setup()

    expect(screen.getByRole('button', { name: '创建' })).toBeDisabled()
    await user.type(screen.getByLabelText('名称'), 'Team')
    await user.type(screen.getByLabelText(/标识/), 'Bad Slug!')
    expect(screen.getByRole('button', { name: '创建' })).toBeDisabled()
    expect(screen.getByText(/小写字母、数字与连字符/)).toBeInTheDocument()
  })
})
