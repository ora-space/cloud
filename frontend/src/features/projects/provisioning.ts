import { useQuery } from '@tanstack/react-query'
import type { AdminOperation, Operation } from '@/api/generated.schemas'
import {
  getApiV1TenantsTidOperationsOid,
  getGetApiV1TenantsTidOperationsOidQueryKey,
} from '@/api/operations/operations'
import {
  getApiV1TenantsTidProjectsPidWorkspaces,
  getGetApiV1TenantsTidProjectsPidWorkspacesQueryKey,
} from '@/api/workspaces/workspaces'

/**
 * Why a project's creation stopped, in the shape the UI renders: the cloud's
 * stable reason code together with a sentence a member can act on.
 *
 * The code is kept beside the sentence rather than replaced by it. Reason codes
 * are part of the public contract, the sentence is presentation, and support
 * asks for the exact code — a component that rendered only prose would make the
 * two impossible to tell apart.
 */
export interface ProvisioningFailure {
  code: string
  message: string
}

/**
 * Sentences for the failure reasons this module has been taught. A code that is
 * not listed falls back to the bare code (see `provisioningFailureMessage`),
 * which is deliberate: inventing a reassuring sentence for an unknown reason
 * would hide it, and a project that fails for a reason nobody has written copy
 * for is exactly when the raw code is most useful.
 */
const PROVISIONING_FAILURE_MESSAGES: Record<string, string> = {
  // Settled by Cloud when this deployment installed no credentialed-clone
  // executor: a Project that names a credential can never be cloned here, so
  // the Operation reaches `failed` instead of retrying forever.
  repository_credential_executor_unavailable:
    '本部署未安装仓库凭据执行器，无法为绑定了凭据的私有仓库执行 Clone，项目创建已终止。请改用公开仓库，或联系管理员为部署安装执行器。',
  // Settled by Cloud when the bound reference does not declare the capability
  // this clone needs. Unlike the deployment-level refusal above, a member can
  // act on this one by binding a reference that declares the capability.
  repository_credential_capability_unsupported:
    '绑定的凭据引用未声明本次 Clone 所需的能力，项目创建已终止。请改用声明了所需能力的凭据引用。',
  administrative_force_stop: '本次创建已被管理员强制停止。',
}

/** The sentence to show for `code`, falling back to the code itself. */
export function provisioningFailureMessage(code: string): string {
  return PROVISIONING_FAILURE_MESSAGES[code] ?? `项目创建失败：${code}`
}

/**
 * The failure an Operation accounts for, or `undefined` when it accounts for
 * none.
 *
 * `AdminOperation` is this same route's other response shape and carries no
 * reason code at all, so only the member-facing `Operation` can name a reason.
 * Only `failed` counts: `running`, `retry_wait` and `blocked` are work still in
 * progress, and reading a pending retry as a failure would tell the member to
 * give up on something that is still moving. A `failed` Operation with no code
 * is likewise not reported — there is nothing to show beyond the state itself,
 * which the project status line already carries.
 */
function provisioningFailureOf(
  operation: Operation | AdminOperation | undefined,
): ProvisioningFailure | undefined {
  if (!operation || operation.state !== 'failed') {
    return undefined
  }
  const code = 'errorCode' in operation ? operation.errorCode : undefined
  if (!code) {
    return undefined
  }
  return { code, message: provisioningFailureMessage(code) }
}

/**
 * The terminal failure of the Operation creating `projectId`, or `undefined`
 * when there is none.
 *
 * The public contract has no "operations of a project" route, so the Operation
 * is reached through the Workspace it creates: the project's Workspace list
 * carries `creatorOperationId`, and the Operation by id carries the `errorCode`
 * Cloud settled it with. Without this, a project whose credentialed clone was
 * refused renders as `provisioning` — "still being created" — forever, and the
 * reason never reaches the member who has to act on it.
 *
 * Only `failed` counts. An Operation that is still running, parked in
 * `retry_wait`/`blocked` for a retry, or already `succeeded` reports no failure:
 * a retry is not a failure, and reporting one would tell the member to give up
 * on work that is still progressing.
 *
 * `isPending` is true until the failure can be ruled out, so a caller can avoid
 * flashing "no failure" while the answer is still unknown. A project with no
 * Workspace yet — the common case while `sandbox`/`node` run — resolves to
 * `isPending: false` with no failure, because there is nothing to ask about.
 */
export function useProjectProvisioningFailure(
  tenantId: string | undefined,
  projectId: string | undefined,
): { failure: ProvisioningFailure | undefined; isPending: boolean } {
  const enabled = !!tenantId && !!projectId
  // The generated client takes strings; an empty id matches nothing and the
  // query it belongs to is disabled anyway.
  const tid = tenantId ?? ''
  const pid = projectId ?? ''
  const workspaces = useQuery({
    queryKey: getGetApiV1TenantsTidProjectsPidWorkspacesQueryKey(tid, pid),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidProjectsPidWorkspaces(tid, pid, undefined, undefined, signal),
    enabled,
  })
  // A project's main Workspace is created once, so the first Workspace naming a
  // creator Operation is the one whose progress this project is waiting on.
  const operationId =
    workspaces.data?.items.find((item) => item.creatorOperationId)?.creatorOperationId ?? undefined
  const oid = operationId ?? ''
  const operation = useQuery({
    queryKey: getGetApiV1TenantsTidOperationsOidQueryKey(tid, oid),
    queryFn: ({ signal }) => getApiV1TenantsTidOperationsOid(tid, oid, undefined, signal),
    enabled: enabled && !!operationId,
  })

  return {
    failure: provisioningFailureOf(operation.data),
    isPending: enabled && (workspaces.isPending || (!!operationId && operation.isPending)),
  }
}
