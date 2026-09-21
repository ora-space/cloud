import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { createMemoryRouter, RouterProvider } from 'react-router-dom'
import { SidebarProvider } from '@/components/ui/sidebar'
import { GeneralSettingsPage } from '@/features/settings/general-settings-page'
import { SettingsLayout } from '@/features/settings/settings-layout'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'

function renderSettingsPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const router = createMemoryRouter(
    [
      {
        path: '/:workspaceSlug/settings',
        element: <SettingsLayout slug="cloud-dev" />,
        children: [{ index: true, element: <GeneralSettingsPage /> }],
      },
    ],
    { initialEntries: ['/cloud-dev/settings'] },
  )
  return render(
    <QueryClientProvider client={queryClient}>
      <SidebarProvider>
        <CurrentSpaceProvider slug="cloud-dev" authenticated>
          <RouterProvider router={router} />
        </CurrentSpaceProvider>
      </SidebarProvider>
    </QueryClientProvider>,
  )
}

describe('GeneralSettingsPage cloud mode', () => {
  it('shows the archive danger zone to owners and archives on confirmation', async () => {
    installCloudSpaceHandlers('owner')
    let deleted = false
    server.use(
      http.delete(`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}`, () => {
        deleted = true
        return HttpResponse.json({
          id: TEST_SPACE_ID,
          tenantId: TEST_TENANT_ID,
          name: 'Cloud Dev',
          slug: 'cloud-dev',
          description: '',
          createdBy: 'u1',
          version: 2,
          createdAt: '2026-09-20T10:00:00+08:00',
          updatedAt: '2026-09-20T10:00:00+08:00',
          archivedAt: '2026-09-20T11:00:00+08:00',
        })
      }),
    )
    renderSettingsPage()
    const user = userEvent.setup()

    expect(
      await screen.findByRole('button', { name: '归档工作区' }, { timeout: 5000 }),
    ).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '归档工作区' }))
    await user.click(await screen.findByRole('button', { name: '确认归档' }))

    await waitFor(() => expect(deleted).toBe(true))
  })

  it('hides the danger zone from non-owners', async () => {
    installCloudSpaceHandlers('member')
    renderSettingsPage()

    expect(
      await screen.findByLabelText('工作区名称', undefined, { timeout: 5000 }),
    ).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '归档工作区' })).not.toBeInTheDocument()
  })
})
