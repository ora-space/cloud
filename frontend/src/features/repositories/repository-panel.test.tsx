import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import {
  gitCredentialFixture,
  installCloudSpaceHandlers,
  installRepositoryHandlers,
  repositoryFixture,
  repositoryOperationFixture,
  TEST_CREDENTIAL_ID,
  TEST_PROJECT_ID,
  TEST_SNAPSHOT_OID,
  TEST_TENANT_ID,
  TEST_USER_ID,
} from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { RepositoryPanel } from './repository-panel'

const OTHER_USER_ID = 'cccccccc-cccc-cccc-cccc-cccccccccccc'

/** Records the POSTs the panel sends to one Operation route. */
function installOperationAction(action: 'approve' | 'cancel' | 'check', bodies: unknown[]): void {
  server.use(
    http.post(
      `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/operations/:roid/${action}`,
      async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ resource: {} })
      },
    ),
  )
}

function renderPanel({
  role = 'member',
  ...source
}: Parameters<typeof installRepositoryHandlers>[0] & { role?: string } = {}) {
  installCloudSpaceHandlers(role)
  installRepositoryHandlers(source)
  return renderWithProviders(<RepositoryPanel projectId={TEST_PROJECT_ID} />, {
    slug: 'cloud-dev',
  })
}

describe('RepositoryPanel', () => {
  it('reports the Binding, its snapshot and the latest sync verdict', async () => {
    renderPanel()
    expect(await screen.findByText('https://example.invalid/r.git · main')).toBeInTheDocument()
    expect(screen.getByText(new RegExp(TEST_SNAPSHOT_OID))).toBeInTheDocument()
    expect(screen.getByText(/同步：成功/)).toBeInTheDocument()
    expect(screen.getByText(/凭据：deploy key/)).toBeInTheDocument()
  })

  it('reads as anonymous when the Binding has no credential and no snapshot', async () => {
    const { binding } = repositoryFixture()
    renderPanel({
      source: {
        binding: { ...binding, credentialRefId: null },
        snapshot: null,
        latestSync: null,
      },
    })
    expect(await screen.findByText(/当前快照：尚未同步/)).toBeInTheDocument()
    expect(screen.getByText(/同步：暂无/)).toBeInTheDocument()
    expect(screen.getByText(/凭据：匿名读取/)).toBeInTheDocument()
  })

  it('reports a source it cannot read instead of an empty panel', async () => {
    renderPanel()
    server.use(
      http.get(`/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository`, () =>
        HttpResponse.json({ code: 'not_configured' }, { status: 503 }),
      ),
    )
    expect(await screen.findByRole('alert')).toHaveTextContent('无法读取代码来源')
  })

  it('asks for a sync the member triggered by hand', async () => {
    const bodies: unknown[] = []
    renderPanel()
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/syncs`,
        async ({ request }) => {
          bodies.push(await request.json())
          return HttpResponse.json({ resource: {} })
        },
      ),
    )
    await userEvent.click(await screen.findByRole('button', { name: '立即同步' }))
    await waitFor(() => expect(bodies).toEqual([{}]))
  })

  it('binds the chosen credential with the Binding version it was read at', async () => {
    const bodies: unknown[] = []
    renderPanel()
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/credential`,
        async ({ request }) => {
          bodies.push(await request.json())
          return HttpResponse.json({ resource: {} })
        },
      ),
    )
    await screen.findByRole('option', { name: 'deploy key · https://example.invalid/r.git' })
    await userEvent.selectOptions(screen.getByLabelText('Git 凭据'), TEST_CREDENTIAL_ID)
    await userEvent.click(screen.getByRole('button', { name: '验证并更换凭据' }))
    await waitFor(() =>
      expect(bodies).toEqual([{ credentialRefId: TEST_CREDENTIAL_ID, version: 4 }]),
    )
  })

  it('offers a member their own pending publish, to approve or to cancel', async () => {
    renderPanel({ operations: [repositoryOperationFixture()] })
    expect(await screen.findByRole('button', { name: '批准' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '拒绝' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '取消发布' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '立即核验' })).not.toBeInTheDocument()
  })

  it('hides approval from a member who does not own the credential that pushes', async () => {
    renderPanel({
      operations: [repositoryOperationFixture({ requestedBy: OTHER_USER_ID })],
      credentials: [gitCredentialFixture({ ownerUserId: OTHER_USER_ID })],
    })
    expect(await screen.findByText(/等待批准/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '批准' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '取消发布' })).not.toBeInTheDocument()
  })

  it('offers only the read-only check while a publish outcome is unknown', async () => {
    const bodies: unknown[] = []
    renderPanel({ operations: [repositoryOperationFixture({ state: 'uncertain' })] })
    installOperationAction('check', bodies)
    await userEvent.click(await screen.findByRole('button', { name: '立即核验' }))
    await waitFor(() => expect(bodies).toEqual([{}]))
    expect(screen.queryByRole('button', { name: '批准' })).not.toBeInTheDocument()
  })

  it('approves the Operation the member chose', async () => {
    const bodies: unknown[] = []
    renderPanel({ operations: [repositoryOperationFixture()] })
    installOperationAction('approve', bodies)
    await userEvent.click(await screen.findByRole('button', { name: '批准' }))
    await waitFor(() => expect(bodies).toEqual([{}]))
    expect(screen.getByText(/凭据版本 1/)).toBeInTheDocument()
  })

  it('shows the fault code the backend refused the action with', async () => {
    renderPanel({ operations: [repositoryOperationFixture()] })
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/operations/:roid/approve`,
        () => HttpResponse.json({ code: 'credential_changed' }, { status: 409 }),
      ),
    )
    await userEvent.click(await screen.findByRole('button', { name: '批准' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('credential_changed')
  })

  it('keeps sync Operations out of the publication list', async () => {
    renderPanel({
      operations: [
        repositoryOperationFixture({ id: 'op-sync', type: 'repository_sync', state: 'succeeded' }),
      ],
    })
    expect(await screen.findByText('https://example.invalid/r.git · main')).toBeInTheDocument()
    expect(screen.queryByText(/成功 · main/)).not.toBeInTheDocument()
  })

  it('reports the publication audit trail the Operation carries', async () => {
    renderPanel({ operations: [repositoryOperationFixture()] })
    expect(await screen.findByText(new RegExp(`发起人 ${TEST_USER_ID}`))).toBeInTheDocument()
    expect(screen.getByText(/源：refs\/ora\/deliveries\/one/)).toBeInTheDocument()
    expect(screen.getByText(/审批人 待批准/)).toBeInTheDocument()
  })
})
