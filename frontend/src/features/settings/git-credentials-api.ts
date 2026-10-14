import { useMutation, useQuery, useQueryClient, type QueryClient } from '@tanstack/react-query'
import type { Error as ApiError, GitCredential } from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidGitCredentialsCredId,
  getApiV1TenantsTidGitCredentials,
  getGetApiV1TenantsTidGitCredentialsQueryKey,
  postApiV1TenantsTidGitCredentials,
  putApiV1TenantsTidGitCredentialsCredId,
} from '@/api/credentials/credentials'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { faultCode, type ErrorType } from '@/lib/api-client'
import { readAllPages } from '@/lib/pagination'

/** Cache key of the complete credential list; distinct from the page key. */
const DISABLED_CREDENTIALS_KEY = ['git-credentials', 'disabled'] as const

/**
 * Query key of the fully walked credential list for one tenant. It extends the
 * generated page key so invalidating the generated prefix still reaches it,
 * while the extra marker keeps a single generated page from colliding with the
 * aggregated list.
 */
function gitCredentialsQueryKey(tenantId: string | undefined) {
  if (!tenantId) return DISABLED_CREDENTIALS_KEY
  return [...getGetApiV1TenantsTidGitCredentialsQueryKey(tenantId), 'all'] as const
}

/** Refreshes the aggregated list after a create, rotate or revoke. */
function invalidateGitCredentials(queryClient: QueryClient, tenantId: string | undefined): void {
  if (!tenantId) return
  void queryClient.invalidateQueries({
    queryKey: getGetApiV1TenantsTidGitCredentialsQueryKey(tenantId),
  })
}

/**
 * The signed-in user's own personal HTTPS Git credentials in `tenantId`, every
 * page walked so the list is complete. Disabled until the tenant resolves; the
 * token is never among the fields, because the API never returns it.
 */
export function useGitCredentials(tenantId: string | undefined) {
  return useQuery({
    queryKey: gitCredentialsQueryKey(tenantId),
    queryFn: async ({ signal }) => {
      const page = await readAllPages((after) =>
        getApiV1TenantsTidGitCredentials(
          tenantId ?? '',
          after ? { limit: 100, after } : { limit: 100 },
          undefined,
          signal,
        ),
      )
      return page.items
    },
    enabled: !!tenantId,
  })
}

/** The label, single repository scope and token of a new credential. */
export interface CreateGitCredentialInput {
  name: string
  repositoryScope: string
  token: string
}

/**
 * Creates a credential and hands its token to the Credential Service inside
 * the request. POST carries a stable idempotency key so a retry of one click
 * replays the original response instead of creating a second credential, and
 * the list refreshes afterwards.
 */
export function useCreateGitCredential(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<GitCredential, ErrorType<ApiError>, CreateGitCredentialInput>({
    mutationFn: async (input: CreateGitCredentialInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return postApiV1TenantsTidGitCredentials(tenantId, input, {
        headers: mutationHeaders(keyFor(input)),
      })
    },
    onSuccess: () => {
      invalidateGitCredentials(queryClient, tenantId)
    },
  })
}

/** The new token and the version the row was loaded with. */
export interface RotateGitCredentialInput {
  id: string
  token: string
  version: number
}

/**
 * Rotates the stored material of one credential without changing its scope.
 * The version is the one the row was rendered with; the server refuses a stale
 * version so a concurrent change is never silently overwritten. Only an
 * available credential is rotatable, because the server rejects the rest.
 */
export function useRotateGitCredential(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  return useMutation<GitCredential, ErrorType<ApiError>, RotateGitCredentialInput>({
    mutationFn: async (input: RotateGitCredentialInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return putApiV1TenantsTidGitCredentialsCredId(tenantId, input.id, {
        token: input.token,
        version: input.version,
      })
    },
    onSuccess: () => {
      invalidateGitCredentials(queryClient, tenantId)
    },
  })
}

/** The credential to revoke and the version the row was loaded with. */
export interface RevokeGitCredentialInput {
  id: string
  version: number
}

/**
 * Revokes one credential. Revocation is recorded as `invalid` and the row
 * stays listed rather than disappearing, so the owner can see what happened.
 * DELETE carries a stable idempotency key, which makes a retry of an already
 * applied revocation a success instead of a second effect.
 */
export function useRevokeGitCredential(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<GitCredential, ErrorType<ApiError>, RevokeGitCredentialInput>({
    mutationFn: async (input: RevokeGitCredentialInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return deleteApiV1TenantsTidGitCredentialsCredId(
        tenantId,
        input.id,
        { version: input.version },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: () => {
      invalidateGitCredentials(queryClient, tenantId)
    },
  })
}

const GIT_CREDENTIAL_FAULTS: Record<string, string> = {
  invalid_credential_name: '名称需为 1–120 个字符，且首尾不能有空白',
  invalid_repository_scope: '仓库范围需为一个合法且完整的 HTTPS 仓库地址',
  invalid_credential_token: 'Token 不能为空，且不能超过 4096 字节',
  credential_not_available: '凭据已冻结或已撤销，不能再轮换',
  version_conflict: '凭据已在别处修改，请刷新后重试',
  version_required: '缺少版本号，请刷新后重试',
  credential_store_unavailable: '当前部署未配置凭据服务，暂时无法保存 Git 凭据',
  not_found: '凭据不存在或不属于当前用户',
}

/** User-facing reason a credential operation failed, keyed by the Fault code. */
export function gitCredentialFailureMessage(error: unknown): string {
  return GIT_CREDENTIAL_FAULTS[faultCode(error) ?? ''] ?? '操作失败，请稍后重试'
}
