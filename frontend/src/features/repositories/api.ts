import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  GitCredential,
  GitCredentialSubmission,
  PostApiV1TenantsTidProjectsPidRepositoryPublishesBody,
} from '@/api/generated.schemas'
import * as credentials from '@/api/git-credentials/git-credentials'
import * as repository from '@/api/repository/repository'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'

/** Current tenant's safe credential metadata; secrets never enter this query cache. */
export function useGitCredentials() {
  const { tenantId } = useCurrentSpace()
  return useQuery({
    queryKey: ['git-credentials', tenantId],
    enabled: !!tenantId,
    queryFn: ({ signal }) =>
      credentials.getApiV1TenantsTidGitCredentials(
        tenantId ?? '',
        { limit: 200 },
        undefined,
        signal,
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
        return credentials.postApiV1TenantsTidGitCredentials(
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
        return credentials.postApiV1TenantsTidGitCredentialsGcidRotate(
          tenantId,
          c.id,
          { version: c.version },
          options,
        )
      if (input.action === 'revoke')
        return credentials.deleteApiV1TenantsTidGitCredentialsGcid(
          tenantId,
          c.id,
          { version: c.version },
          options,
        )
      return credentials.postApiV1TenantsTidGitCredentialsGcidVerifications(tenantId, c.id, options)
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
      credentials.postApiV1TenantsTidGitCredentialsGcidConfirm(tenantId ?? '', input.credentialId, {
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
      repository.getApiV1TenantsTidProjectsPidRepository(
        tenantId ?? '',
        projectId,
        undefined,
        signal,
      ),
    refetchInterval: 5000,
  })
  const operations = useQuery({
    queryKey: ['repository-operations', tenantId, projectId],
    enabled,
    queryFn: ({ signal }) =>
      repository.getApiV1TenantsTidProjectsPidRepositoryOperations(
        tenantId ?? '',
        projectId,
        { limit: 200 },
        undefined,
        signal,
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
        return repository.postApiV1TenantsTidProjectsPidRepositorySyncs(
          tenantId,
          projectId,
          {},
          opts,
        )
      if (input.action === 'credential')
        return repository.postApiV1TenantsTidProjectsPidRepositoryCredential(
          tenantId,
          projectId,
          { credentialRefId: input.credentialRefId, version: input.version },
          opts,
        )
      if (input.action === 'publish')
        return repository.postApiV1TenantsTidProjectsPidRepositoryPublishes(
          tenantId,
          projectId,
          input.body,
          opts,
        )
      const actions = {
        approve: repository.postApiV1TenantsTidProjectsPidRepositoryOperationsRoidApprove,
        reject: repository.postApiV1TenantsTidProjectsPidRepositoryOperationsRoidReject,
        cancel: repository.postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCancel,
        check: repository.postApiV1TenantsTidProjectsPidRepositoryOperationsRoidCheck,
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
      return getApiV1TenantsTidProjectsPidWorkspaces(
        tenantId ?? '',
        projectId,
        { limit: 200 },
        undefined,
        signal,
      )
    },
  })
  const branches = useQuery({
    queryKey: ['publish-branches', tenantId, projectId, workspaceId],
    enabled: !!tenantId && !!workspaceId,
    queryFn: ({ signal }) =>
      repository.getApiV1TenantsTidProjectsPidRepositoryWorkspacesWidBranches(
        tenantId ?? '',
        projectId,
        workspaceId,
        undefined,
        signal,
      ),
  })
  return { workspaces, branches }
}
