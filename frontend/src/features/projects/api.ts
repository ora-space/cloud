import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  DeleteApiV1TenantsTidProjectsPid202,
  Error as ApiError,
  Project as CloudProject,
} from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidProjectsPid,
  getApiV1TenantsTidProjectsPid,
  patchApiV1TenantsTidProjectsPid,
} from '@/api/projects/projects'
import { postApiV1TenantsTidSpacesSpaceIdProjects } from '@/api/spaces/spaces'
import { useCurrentSpace } from '@/features/spaces/current-space'
import { mutationHeaders, useIdempotencyKeys, useSpaceProjects } from '@/features/spaces/api'
import type { ErrorType } from '@/lib/api-client'
import type { Project } from '@/mocks/data/types'

/**
 * Maps the cloud's execution lifecycle onto the UI board status. Provisioning
 * counts as planned, anything past active (deleting/deleted) as completed.
 */
function statusFor(lifecycle: string): Project['status'] {
  if (lifecycle === 'active') return 'in_progress'
  if (lifecycle === 'provisioning') return 'planned'
  return 'completed'
}

/**
 * Maps the cloud project contract onto the UI's project shape, which the
 * mock-backed pages (issues, boards) still share. Fields the cloud does not
 * model get stable UI defaults.
 */
export function cloudProjectToUI(p: CloudProject): Project {
  return {
    id: p.id,
    // `projects.space_id` stays nullable in this tree (D2); the UI renders
    // space-scoped projects, so the mapping falls back to the empty id.
    workspaceId: p.spaceId ?? '',
    title: p.name,
    description: p.repositoryUrl,
    icon: 'folder-kanban',
    color: '#3b82f6',
    status: statusFor(p.lifecycle),
    leadId: p.ownerUserId,
    targetDate: null,
    createdAt: p.createdAt,
  }
}

/**
 * Projects of the space at `slug`, fetched through the generated client once
 * the slug resolved to a joined space. Until then the list is pending, never
 * demo data.
 */
export function useProjects(slug: string): {
  data: Project[] | undefined
  isPending: boolean
  isError: boolean
} {
  const { tenantId, space } = useCurrentSpace()
  const cloud = useSpaceProjects(tenantId, space?.slug === slug ? space.id : undefined)
  return {
    data: cloud.data?.items.map(cloudProjectToUI),
    isPending: cloud.isLoading,
    isError: cloud.isError,
  }
}

/** One project by id, resolved from the current space's project list. */
export function useProject(
  slug: string,
  id: string | undefined,
): {
  data: Project | undefined
  isPending: boolean
} {
  const { tenantId, space } = useCurrentSpace()
  const cloud = useSpaceProjects(tenantId, space?.slug === slug ? space.id : undefined)
  const found = cloud.data?.items.find((candidate) => candidate.id === id)
  return { data: found ? cloudProjectToUI(found) : undefined, isPending: cloud.isLoading }
}

/** Input for creating a project; the repository URL is required by the cloud. */
export interface CreateProjectInput {
  title: string
  repositoryUrl: string
  defaultBranch: string
  credentialRefId?: string
}

/** Creates a project in the current cloud space; creation is asynchronous (202). */
export function useCreateProject() {
  const queryClient = useQueryClient()
  const { tenantId, space } = useCurrentSpace()
  const keyFor = useIdempotencyKeys()
  return useMutation<
    Awaited<ReturnType<typeof postApiV1TenantsTidSpacesSpaceIdProjects>>,
    ErrorType<ApiError>,
    CreateProjectInput
  >({
    mutationFn: async (input: CreateProjectInput) => {
      if (!tenantId || !space) throw new Error('cloud space not resolved')
      return postApiV1TenantsTidSpacesSpaceIdProjects(
        tenantId,
        space.id,
        {
          name: input.title,
          repositoryUrl: input.repositoryUrl,
          defaultBranch: input.defaultBranch,
          ...(input.credentialRefId ? { credentialRefId: input.credentialRefId } : {}),
        },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: () => {
      if (!tenantId || !space) return
      void queryClient.invalidateQueries({
        queryKey: [`/api/v1/tenants/${tenantId}/spaces/${space.id}/projects`],
      })
    },
  })
}

/** Renames a project with an optimistic version guard. */
export function useUpdateProject() {
  const queryClient = useQueryClient()
  const { tenantId, space } = useCurrentSpace()
  return useMutation<
    Awaited<ReturnType<typeof patchApiV1TenantsTidProjectsPid>>,
    ErrorType<ApiError>,
    { id: string; title: string; version: number }
  >({
    mutationFn: async (input: { id: string; title: string; version: number }) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return patchApiV1TenantsTidProjectsPid(tenantId, input.id, {
        name: input.title,
        version: input.version,
      })
    },
    onSuccess: () => {
      if (!tenantId || !space) return
      void queryClient.invalidateQueries({
        queryKey: [`/api/v1/tenants/${tenantId}/spaces/${space.id}/projects`],
      })
      void queryClient.invalidateQueries({ queryKey: ['cloud-project', tenantId] })
    },
  })
}

/** Raw cloud project fetch used by the detail page fallback in cloud mode. */
export async function fetchCloudProject(
  tenantId: string,
  projectId: string,
): Promise<CloudProject> {
  return getApiV1TenantsTidProjectsPid(tenantId, projectId)
}

/**
 * Live cloud project by id, used by detail pages that need the current
 * optimistic version for renames and deletes.
 */
export function useCloudProject(tenantId: string | undefined, projectId: string | undefined) {
  return useQuery({
    queryKey: ['cloud-project', tenantId, projectId],
    queryFn: ({ signal }) =>
      getApiV1TenantsTidProjectsPid(tenantId ?? '', projectId ?? '', undefined, signal),
    enabled: !!tenantId && !!projectId,
  })
}

/** Deletes a project; the cloud enters the lifecycle state machine (202). */
export function useDeleteProject() {
  const queryClient = useQueryClient()
  const { tenantId, space } = useCurrentSpace()
  const keyFor = useIdempotencyKeys()
  return useMutation<
    DeleteApiV1TenantsTidProjectsPid202,
    ErrorType<ApiError>,
    { id: string; version: number }
  >({
    mutationFn: async (input: { id: string; version: number }) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return deleteApiV1TenantsTidProjectsPid(
        tenantId,
        input.id,
        { version: input.version },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: () => {
      if (!tenantId || !space) return
      void queryClient.invalidateQueries({
        queryKey: [`/api/v1/tenants/${tenantId}/spaces/${space.id}/projects`],
      })
    },
  })
}
