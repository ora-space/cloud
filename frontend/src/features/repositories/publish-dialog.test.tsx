import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { GitCredential } from '@/api/generated.schemas'
import { server } from '@/test/msw-server'
import {
  gitCredentialFixture,
  installCloudSpaceHandlers,
  installPublishSourceHandlers,
  installRepositoryHandlers,
  repositoryFixture,
  TEST_BRANCH_OID,
  TEST_CREDENTIAL_ID,
  TEST_PROJECT_ID,
  TEST_SNAPSHOT_OID,
  TEST_TENANT_ID,
  workspaceFixture,
} from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { PublishDialog } from './publish-dialog'

const ZERO_OID = '0'.repeat(40)
const WORKSPACE_ID = workspaceFixture().id

/** Records the publish bodies the dialog sends. */
function installPublish(bodies: unknown[]): void {
  server.use(
    http.post(
      `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/publishes`,
      async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ resource: {} })
      },
    ),
  )
}

/**
 * Mounts the dialog over the shared cloud fixtures, with the credential list a
 * test wants to choose from.
 */
function renderDialog({
  role = 'member',
  credentials = [gitCredentialFixture()],
}: { role?: string; credentials?: GitCredential[] } = {}) {
  installCloudSpaceHandlers(role)
  installRepositoryHandlers({ credentials })
  installPublishSourceHandlers()
  return renderWithProviders(
    <PublishDialog
      projectId={TEST_PROJECT_ID}
      source={repositoryFixture()}
      open
      onOpenChange={() => {}}
    />,
    { slug: 'cloud-dev' },
  )
}

/** Fills in the source half of the form the way a member would. */
async function chooseSource(): Promise<void> {
  await screen.findByRole('option', { name: WORKSPACE_ID })
  await userEvent.selectOptions(screen.getByLabelText('发布 Workspace'), WORKSPACE_ID)
  await screen.findByRole('option', { name: `topic · ${TEST_BRANCH_OID.slice(0, 12)}` })
  await userEvent.selectOptions(screen.getByLabelText('发布源分支'), 'refs/heads/topic')
}

describe('PublishDialog', () => {
  it('seeds the target branch and the external expectation from the Binding', async () => {
    renderDialog()
    expect(await screen.findByRole('option', { name: 'deploy key' })).toBeInTheDocument()
    expect(screen.getByLabelText('发布凭据')).toHaveValue(TEST_CREDENTIAL_ID)
    expect(screen.getByLabelText('目标分支')).toHaveValue('main')
    expect(screen.getByLabelText('基于外部提交（全零表示新分支）')).toHaveValue(TEST_SNAPSHOT_OID)
  })

  it('treats any other target as a branch the publish creates', async () => {
    renderDialog()
    await userEvent.clear(await screen.findByLabelText('目标分支'))
    await userEvent.type(screen.getByLabelText('目标分支'), 'release')
    expect(screen.getByLabelText('基于外部提交（全零表示新分支）')).toHaveValue(ZERO_OID)
  })

  it('sends the branch, its commit and the expectation the target implies', async () => {
    const bodies: unknown[] = []
    installPublish(bodies)
    renderDialog()
    await chooseSource()
    await userEvent.click(await screen.findByRole('button', { name: '发布' }))
    await waitFor(() =>
      expect(bodies).toEqual([
        {
          targetBranch: 'main',
          expectedOldOid: TEST_SNAPSHOT_OID,
          credentialRefId: TEST_CREDENTIAL_ID,
          sourceRef: 'refs/heads/topic',
          sourceCommit: TEST_BRANCH_OID,
        },
      ]),
    )
  })

  it('sends the chosen Revision instead of the branch it was picked beside', async () => {
    const bodies: unknown[] = []
    installPublish(bodies)
    renderDialog()
    await chooseSource()
    await userEvent.type(screen.getByLabelText('Revision ID'), 'rev-42')
    await userEvent.click(await screen.findByRole('button', { name: '发布' }))
    await waitFor(() =>
      expect(bodies).toEqual([
        {
          targetBranch: 'main',
          expectedOldOid: TEST_SNAPSHOT_OID,
          credentialRefId: TEST_CREDENTIAL_ID,
          revisionId: 'rev-42',
        },
      ]),
    )
  })

  it('refuses to submit before a source is chosen', async () => {
    renderDialog()
    expect(await screen.findByRole('button', { name: '发布' })).toBeDisabled()
  })

  it('shows the branch commit the request will name', async () => {
    renderDialog()
    await chooseSource()
    expect(await screen.findByText(`源提交：${TEST_BRANCH_OID}`)).toBeInTheDocument()
  })

  it('offers a personal credential only to the member who owns it', async () => {
    renderDialog()
    expect(await screen.findByText(/需要凭据所有者/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '发布' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '请求发布' })).not.toBeInTheDocument()
  })

  it('asks for approval when the member may not approve the chosen credential', async () => {
    renderDialog({ credentials: [gitCredentialFixture({ attribution: 'team' })] })
    expect(await screen.findByText('需要管理员批准')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '请求发布' })).toBeInTheDocument()
  })

  it('offers no credential that cannot push yet', async () => {
    renderDialog({ credentials: [gitCredentialFixture({ secretState: 'awaiting_secret' })] })
    await screen.findByLabelText('发布凭据')
    expect(screen.getByRole('option', { name: '请选择已验证凭据' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'deploy key' })).not.toBeInTheDocument()
  })

  it('offers the workspaces a member may publish from', async () => {
    renderDialog()
    expect(await screen.findByRole('option', { name: WORKSPACE_ID })).toBeInTheDocument()
  })

  it('reports a publish the backend refused without closing the dialog', async () => {
    server.use(
      http.post(
        `/api/v1/tenants/${TEST_TENANT_ID}/projects/${TEST_PROJECT_ID}/repository/publishes`,
        () => HttpResponse.json({ code: 'not_fast_forward' }, { status: 409 }),
      ),
    )
    renderDialog()
    await chooseSource()
    await userEvent.click(await screen.findByRole('button', { name: '发布' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('not_fast_forward')
    expect(screen.getByRole('button', { name: '发布' })).toBeInTheDocument()
  })
})
