import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { GeneralSettingsPage } from '@/features/settings/general-settings-page'
import { CurrentSpaceProvider } from '@/features/spaces/current-space'
import { installCloudSpaceHandlers } from '@/test/cloud-handlers'
import { renderRoutes } from '@/test/render'
import { SettingsLayout } from './settings-layout'

function renderSettings() {
  installCloudSpaceHandlers('owner')
  return renderRoutes(
    [
      {
        path: '/w/:workspaceSlug/settings',
        element: (
          <CurrentSpaceProvider slug="cloud-dev">
            <SettingsLayout slug="cloud-dev" />
          </CurrentSpaceProvider>
        ),
        children: [
          { index: true, element: <GeneralSettingsPage /> },
          { path: 'members', element: <div>Members screen</div> },
          { path: 'git-identity', element: <div>Git identity screen</div> },
        ],
      },
    ],
    '/w/cloud-dev/settings',
  )
}

describe('SettingsLayout', () => {
  it('shows General by default and navigates to Members on tab click', async () => {
    const user = userEvent.setup()
    renderSettings()

    expect(await screen.findByDisplayValue('Cloud Dev')).toBeInTheDocument()

    await user.click(screen.getByRole('link', { name: '成员' }))
    expect(await screen.findByText('Members screen')).toBeInTheDocument()
  })

  it('links the Git 身份 tab to the git identity route', async () => {
    const user = userEvent.setup()
    renderSettings()

    expect(await screen.findByDisplayValue('Cloud Dev')).toBeInTheDocument()
    await user.click(screen.getByRole('link', { name: 'Git 身份' }))
    expect(await screen.findByText('Git identity screen')).toBeInTheDocument()
  })
})
