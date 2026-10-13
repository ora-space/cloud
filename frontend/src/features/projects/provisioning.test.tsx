import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import {
  provisioningFailureMessage,
  type ProvisioningFailure,
  useProjectProvisioningFailure,
} from '@/features/projects/provisioning'
import { server } from '@/test/msw-server'

const TENANT_ID = '11111111-1111-1111-1111-111111111111'
const PROJECT_ID = '55555555-5555-5555-5555-555555555555'
const OPERATION_ID = '99999999-9999-9999-9999-999999999999'

function wrapper(client: QueryClient) {
  return function Wrapper({ children }: { children: React.ReactNode }) {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>
  }
}

function newClient() {
  return new QueryClient({ defaultOptions: { queries: { retry: false } } })
}

function installHandlers(workspaces: unknown[], found?: unknown) {
  server.use(
    http.get(`/api/v1/tenants/${TENANT_ID}/projects/${PROJECT_ID}/workspaces`, () =>
      HttpResponse.json({ items: workspaces, nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TENANT_ID}/operations/${OPERATION_ID}`, () =>
      HttpResponse.json(found ?? {}),
    ),
  )
}

/** The one field the hook reads off a Workspace: the Operation it was created by. */
function workspace(creatorOperationId: string | null) {
  return { id: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', creatorOperationId }
}

/** The member-facing Operation shape, which is the only one carrying `errorCode`. */
function operation(state: string, errorCode: string | null) {
  return {
    id: OPERATION_ID,
    tenantId: TENANT_ID,
    projectId: PROJECT_ID,
    workspaceId: 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
    kind: 'create_project',
    state,
    step: 'clone',
    errorCode,
    version: 2,
  }
}

/**
 * The failure the hook settles on once it has stopped loading, for a project
 * whose Workspace list and Operation are the given doubles.
 */
async function settledFailure(
  workspaces: unknown[],
  found?: unknown,
): Promise<ProvisioningFailure | undefined> {
  installHandlers(workspaces, found)
  const { result } = renderHook(() => useProjectProvisioningFailure(TENANT_ID, PROJECT_ID), {
    wrapper: wrapper(newClient()),
  })

  await waitFor(() => expect(result.current.isPending).toBe(false))
  return result.current.failure
}

describe('provisioningFailureMessage', () => {
  it('explains the deployment-capability refusal a credentialed clone is settled with', () => {
    expect(provisioningFailureMessage('repository_credential_executor_unavailable')).toContain(
      '未安装仓库凭据执行器',
    )
  })

  it('explains the refusal a reference without the needed capability is settled with', () => {
    expect(provisioningFailureMessage('repository_credential_capability_unsupported')).toContain(
      '未声明本次 Clone 所需的能力',
    )
  })

  it('falls back to the raw code instead of inventing a sentence for an unknown reason', () => {
    expect(provisioningFailureMessage('some_future_code')).toBe('项目创建失败：some_future_code')
  })
})

describe('useProjectProvisioningFailure', () => {
  it('reports the settled reason of a failed provisioning Operation', async () => {
    const failure = await settledFailure(
      [workspace(OPERATION_ID)],
      operation('failed', 'repository_credential_executor_unavailable'),
    )

    expect(failure).toEqual({
      code: 'repository_credential_executor_unavailable',
      message: provisioningFailureMessage('repository_credential_executor_unavailable'),
    })
  })

  it('reports no failure while the Operation is still running', async () => {
    await expect(
      settledFailure([workspace(OPERATION_ID)], operation('running', null)),
    ).resolves.toBeUndefined()
  })

  it('reports no failure for an Operation parked for a retry', async () => {
    await expect(
      settledFailure([workspace(OPERATION_ID)], operation('retry_wait', 'clone_failed')),
    ).resolves.toBeUndefined()
  })

  it('reports no failure for a failure that settled without a reason code', async () => {
    await expect(
      settledFailure([workspace(OPERATION_ID)], operation('failed', null)),
    ).resolves.toBeUndefined()
  })

  it('reports no failure when the project has no Workspace yet', async () => {
    await expect(settledFailure([])).resolves.toBeUndefined()
  })

  it('reports no failure when the Workspace names no creator Operation', async () => {
    await expect(settledFailure([workspace(null)])).resolves.toBeUndefined()
  })

  it('asks nothing until both the tenant and the project are known', () => {
    const { result } = renderHook(() => useProjectProvisioningFailure(undefined, undefined), {
      wrapper: wrapper(newClient()),
    })

    expect(result.current.isPending).toBe(false)
    expect(result.current.failure).toBeUndefined()
  })
})
