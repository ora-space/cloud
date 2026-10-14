import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import {
  provisioningFailureMessage,
  type ProvisioningFailure,
  type ProvisioningPhase,
  useProjectProvisioning,
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
 * What the hook settles on once it has stopped loading, for a project whose
 * Workspace list and Operation are the given doubles.
 */
async function settled(
  workspaces: unknown[],
  found?: unknown,
): Promise<{ phase: ProvisioningPhase | undefined; failure: ProvisioningFailure | undefined }> {
  installHandlers(workspaces, found)
  const { result } = renderHook(() => useProjectProvisioning(TENANT_ID, PROJECT_ID), {
    wrapper: wrapper(newClient()),
  })

  await waitFor(() => expect(result.current.isPending).toBe(false))
  return { phase: result.current.phase, failure: result.current.failure }
}

/** The phase the hook reports for one Operation state, with no failure involved. */
async function settledPhase(state: string): Promise<ProvisioningPhase | undefined> {
  return (await settled([workspace(OPERATION_ID)], operation(state, null))).phase
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

  it('explains the refusal a deleted credential is settled with', () => {
    expect(provisioningFailureMessage('repository_credential_revoked')).toContain('Token 已销毁')
  })

  it('names both causes a failed mirror preparation can have instead of guessing one', () => {
    const message = provisioningFailureMessage('mirror_preparation_failed')

    expect(message).toContain('凭据无权读取该仓库')
    expect(message).toContain('无法访问该仓库地址')
  })

  it('falls back to the raw code instead of inventing a sentence for an unknown reason', () => {
    expect(provisioningFailureMessage('some_future_code')).toBe('项目创建失败：some_future_code')
  })
})

describe('useProjectProvisioning', () => {
  it('reports the settled reason of a failed provisioning Operation', async () => {
    const { phase, failure } = await settled(
      [workspace(OPERATION_ID)],
      operation('failed', 'repository_credential_executor_unavailable'),
    )

    expect(phase).toBe('failed')
    expect(failure).toEqual({
      code: 'repository_credential_executor_unavailable',
      message: provisioningFailureMessage('repository_credential_executor_unavailable'),
    })
  })

  it('reports an in-flight creation as provisioning rather than as a failure', async () => {
    await expect(settledPhase('running')).resolves.toBe('provisioning')
  })

  it('reports a pending retry as retrying, which is not a failure', async () => {
    await expect(settledPhase('retry_wait')).resolves.toBe('retrying')
  })

  it('reports a blocked Operation as retrying, which is not a failure', async () => {
    await expect(settledPhase('blocked')).resolves.toBe('retrying')
  })

  it('reports a creation that finished as succeeded', async () => {
    await expect(settledPhase('succeeded')).resolves.toBe('succeeded')
  })

  it('reports no failure while the Operation is still running', async () => {
    const { failure } = await settled([workspace(OPERATION_ID)], operation('running', null))

    expect(failure).toBeUndefined()
  })

  it('reports no failure for an Operation parked for a retry', async () => {
    const { failure } = await settled(
      [workspace(OPERATION_ID)],
      operation('retry_wait', 'clone_failed'),
    )

    expect(failure).toBeUndefined()
  })

  it('reports the failure phase without a reason for a failure that settled without a code', async () => {
    const { phase, failure } = await settled([workspace(OPERATION_ID)], operation('failed', null))

    expect(phase).toBe('failed')
    expect(failure).toBeUndefined()
  })

  it('reports no phase when the project has no Workspace yet', async () => {
    const { phase, failure } = await settled([])

    expect(phase).toBeUndefined()
    expect(failure).toBeUndefined()
  })

  it('reports no phase when the Workspace names no creator Operation', async () => {
    const { phase } = await settled([workspace(null)])

    expect(phase).toBeUndefined()
  })

  it('reports no phase for an Operation state this build has not been taught', async () => {
    // A server that grows a state must not have it read as a phase the badge
    // would then render; the project falls back to its lifecycle status.
    await expect(settledPhase('some_future_state')).resolves.toBeUndefined()
  })

  it('asks nothing until both the tenant and the project are known', () => {
    const { result } = renderHook(() => useProjectProvisioning(undefined, undefined), {
      wrapper: wrapper(newClient()),
    })

    expect(result.current.isPending).toBe(false)
    expect(result.current.phase).toBeUndefined()
    expect(result.current.failure).toBeUndefined()
  })
})
