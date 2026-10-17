import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { GitCredential } from '@/api/generated.schemas'
import { installCloudSpaceHandlers, TEST_USER_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { mayManageCredential, useCredentialViewer } from './credential-authority'
import type { CredentialViewer } from './credential-authority'

const OWNER = TEST_USER_ID
const OTHER = 'cccccccc-cccc-cccc-cccc-cccccccccccc'
const member: CredentialViewer = { userId: OWNER, isTenantAdmin: false }
const admin: CredentialViewer = { userId: OWNER, isTenantAdmin: true }

/** Renders the viewer the hooks resolve, so the provider wiring is what is asserted. */
function ViewerProbe() {
  const viewer = useCredentialViewer()
  return <p>{`${viewer.userId}|${String(viewer.isTenantAdmin)}`}</p>
}

/** The two fields the management rule reads, for a credential fixture. */
function credentialOf(attribution: GitCredential['attribution'], ownerUserId = OWNER) {
  return { attribution, ownerUserId }
}

describe('mayManageCredential', () => {
  it('lets a member manage their own personal credential', () => {
    expect(mayManageCredential(credentialOf('personal'), member)).toBe(true)
  })

  it('keeps a member out of another member personal credential', () => {
    expect(mayManageCredential(credentialOf('personal', OTHER), member)).toBe(false)
  })

  it('keeps a member out of another member personal credential even as tenant admin', () => {
    expect(mayManageCredential(credentialOf('personal', OTHER), admin)).toBe(false)
  })

  it('lets a tenant admin manage a team credential', () => {
    expect(mayManageCredential(credentialOf('team'), admin)).toBe(true)
  })

  it('keeps a plain member out of a team credential', () => {
    expect(mayManageCredential(credentialOf('team'), member)).toBe(false)
  })

  it('claims no right over a credential whose attribution the server could not resolve', () => {
    expect(mayManageCredential(credentialOf('unknown'), admin)).toBe(false)
  })

  it('claims no right while the credential list has not loaded', () => {
    expect(mayManageCredential(undefined, admin)).toBe(false)
  })
})

describe('useCredentialViewer', () => {
  it('reports the signed-in member and their admin role', async () => {
    installCloudSpaceHandlers('admin')
    renderWithProviders(<ViewerProbe />, { slug: 'cloud-dev' })
    expect(await screen.findByText(`${OWNER}|true`)).toBeInTheDocument()
  })

  it('reports no admin right for a plain member', async () => {
    installCloudSpaceHandlers('member')
    renderWithProviders(<ViewerProbe />, { slug: 'cloud-dev' })
    expect(await screen.findByText(`${OWNER}|false`)).toBeInTheDocument()
  })

  it('reports no user at all when the session is signed out', async () => {
    renderWithProviders(<ViewerProbe />, { slug: 'cloud-dev' })
    expect(await screen.findByText('|false')).toBeInTheDocument()
  })
})
