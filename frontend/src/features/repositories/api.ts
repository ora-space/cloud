import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  GitCredential,
  GitCredentialSubmission,
  PostApiV1TenantsTidProjectsPidRepositoryPublishesBody,
} from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidGitCredentialsGcid,
  getApiV1TenantsTidGitCredentials,
  postApiV1TenantsTidGitCredentials,
  postApiV1TenantsTidGitCredentialsGcidConfirm,
  postApiV1TenantsTidGitCredentialsGcidRotate,
  postApiV1TenantsTidGitCredentialsGcidVerifications,
} from '@/api/git-credentials/git-credentials'
import {
  getApiV1TenantsTidProjectsPidRepository,
  getApiV1TenantsTidProjectsPidRepositoryOperations,
  getApiV1TenantsTidProjectsPidRepositoryWorkspacesWidBranches,
  postApiV1TenantsTidProjectsPidRepositoryCredential,
  postApiV1TenantsTidProjectsPidRepositoryOperationsRoidApprove,
  postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCancel,
  postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCheck,
  postApiV1TenantsTidProjectsPidRepositoryOperationsRoidReject,
  postApiV1TenantsTidProjectsPidRepositoryPublishes,
  postApiV1TenantsTidProjectsPidRepositorySyncs,
} from '@/api/repository/repository'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { readAllPages } from '@/lib/pagination'

/**
 * One page of a Cloud list. The backend caps a page at 100 rows and rejects anything above that
 * with 400 invalid_pagination, so a longer list is read by following its cursor, never by asking
 * for a bigger page.
 */
function pageParams(after: string | undefined) {
  return after ? { limit: 100, after } : { limit: 100 }
}

/** Current tenant's safe credential metadata; secrets never enter this query cache. */
export function useGitCredentials() {
  const { tenantId } = useCurrentSpace()
  return useQuery({
    queryKey: ['git-credentials', tenantId],
    enabled: !!tenantId,
    queryFn: ({ signal }) =>
      readAllPages((after) =>
        getApiV1TenantsTidGitCredentials(tenantId ?? '', pageParams(after), undefined, signal),
      ),
    refetchInterval: 5000,
  })
}

/** Versioned credential metadata operations. Submission grants are returned only to the caller. */
export function useCredentialActions() {
  const { tenantId } = useCurrentSpace()
  const cache = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: async (
      input:
        | {
            action: 'create'
            name: string
            repositoryScope: string
            attribution: 'personal' | 'team'
          }
        | { action: 'rotate' | 'revoke' | 'verify'; credential: GitCredential },
    ): Promise<{ resource: { id: string }; submission?: GitCredentialSubmission }> => {
      if (!tenantId) throw new Error('请选择空间')
      const options = { headers: mutationHeaders(keyFor(input)) }
      if (input.action === 'create')
        return postApiV1TenantsTidGitCredentials(
          tenantId,
          {
            name: input.name,
            repositoryScope: input.repositoryScope,
            attribution: input.attribution,
            requestedCapabilities: ['read', 'publish'],
          },
          options,
        )
      const c = input.credential
      if (input.action === 'rotate')
        return postApiV1TenantsTidGitCredentialsGcidRotate(
          tenantId,
          c.id,
          { version: c.version },
          options,
        )
      if (input.action === 'revoke')
        return deleteApiV1TenantsTidGitCredentialsGcid(
          tenantId,
          c.id,
          { version: c.version },
          options,
        )
      // The verification request carries no body, but its idempotency key travels in `options`;
      // passing it in the body position would send the key as JSON and drop the header.
      return postApiV1TenantsTidGitCredentialsGcidVerifications(tenantId, c.id, {}, options)
    },
    onSuccess: () => {
      void cache.invalidateQueries({ queryKey: ['git-credentials', tenantId] })
    },
  })
}

/** Confirms only metadata after a direct Store submission. No token reaches Cloud. */
export function useConfirmCredential() {
  const { tenantId } = useCurrentSpace()
  const cache = useQueryClient()
  return useMutation({
    mutationFn: (input: { credentialId: string; submissionId: string }) =>
      postApiV1TenantsTidGitCredentialsGcidConfirm(tenantId ?? '', input.credentialId, {
        submissionId: input.submissionId,
      }),
    onSuccess: () => {
      void cache.invalidateQueries({ queryKey: ['git-credentials', tenantId] })
    },
  })
}

/** Live code-source state and operation history, scoped to the selected project. */
export function useRepository(projectId: string) {
  const { tenantId } = useCurrentSpace()
  const enabled = !!tenantId
  const source = useQuery({
    queryKey: ['repository', tenantId, projectId],
    enabled,
    queryFn: ({ signal }) =>
      getApiV1TenantsTidProjectsPidRepository(tenantId ?? '', projectId, undefined, signal),
    refetchInterval: 5000,
  })
  const operations = useQuery({
    queryKey: ['repository-operations', tenantId, projectId],
    enabled,
    queryFn: ({ signal }) =>
      readAllPages((after) =>
        getApiV1TenantsTidProjectsPidRepositoryOperations(
          tenantId ?? '',
          projectId,
          pageParams(after),
          undefined,
          signal,
        ),
      ),
    refetchInterval: 5000,
  })
  return { source, operations }
}

/** All project mutations are explicit; uncertain operations offer only a read-only check. */
export function useRepositoryAction(projectId: string) {
  const { tenantId } = useCurrentSpace()
  const cache = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation({
    mutationFn: async (
      input:
        | { action: 'sync' }
        | { action: 'credential'; credentialRefId: string; version: number }
        | { action: 'publish'; body: PostApiV1TenantsTidProjectsPidRepositoryPublishesBody }
        | { action: 'approve' | 'reject' | 'cancel' | 'check'; operationId: string },
    ) => {
      if (!tenantId) throw new Error('请选择空间')
      const opts = { headers: mutationHeaders(keyFor(input)) }
      if (input.action === 'sync')
        return postApiV1TenantsTidProjectsPidRepositorySyncs(tenantId, projectId, {}, opts)
      if (input.action === 'credential')
        return postApiV1TenantsTidProjectsPidRepositoryCredential(
          tenantId,
          projectId,
          { credentialRefId: input.credentialRefId, version: input.version },
          opts,
        )
      if (input.action === 'publish')
        return postApiV1TenantsTidProjectsPidRepositoryPublishes(
          tenantId,
          projectId,
          input.body,
          opts,
        )
      const actions = {
        approve: postApiV1TenantsTidProjectsPidRepositoryOperationsRoidApprove,
        reject: postApiV1TenantsTidProjectsPidRepositoryOperationsRoidReject,
        cancel: postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCancel,
        check: postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCheck,
      }
      return actions[input.action](tenantId, projectId, input.operationId, {}, opts)
    },
    onSuccess: () => {
      void cache.invalidateQueries({ queryKey: ['repository', tenantId, projectId] })
      void cache.invalidateQueries({ queryKey: ['repository-operations', tenantId, projectId] })
    },
  })
}

/** Lists visible workspaces and one selected workspace's Git branches for explicit publishing. */
export function usePublishSources(projectId: string, workspaceId: string) {
  const { tenantId } = useCurrentSpace()
  const workspaces = useQuery({
    queryKey: ['publish-workspaces', tenantId, projectId],
    enabled: !!tenantId,
    queryFn: async ({ signal }) => {
      const { getApiV1TenantsTidProjectsPidWorkspaces } =
        await import('@/api/workspaces/workspaces')
      return readAllPages((after) =>
        getApiV1TenantsTidProjectsPidWorkspaces(
          tenantId ?? '',
          projectId,
          pageParams(after),
          undefined,
          signal,
        ),
      )
    },
  })
  const branches = useQuery({
    queryKey: ['publish-branches', tenantId, projectId, workspaceId],
    enabled: !!tenantId && !!workspaceId,
    queryFn: ({ signal }) =>
      getApiV1TenantsTidProjectsPidRepositoryWorkspacesWidBranches(
        tenantId ?? '',
        projectId,
        workspaceId,
        undefined,
        signal,
      ),
  })
  return { workspaces, branches }
}
