import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  Error as ApiError,
  PluginCatalog,
  PluginCatalogEntry,
  PostApiV1TenantsTidSpacesSpaceIdPluginsBody,
  SpacePlugin,
} from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidSpacesSpaceIdPlugins,
  getApiV1TenantsTidSpacesSpaceIdPlugins,
  getApiV1TenantsTidSpacesSpaceIdPluginsCatalog,
  getGetApiV1TenantsTidSpacesSpaceIdPluginsCatalogQueryKey,
  getGetApiV1TenantsTidSpacesSpaceIdPluginsQueryKey,
  postApiV1TenantsTidSpacesSpaceIdPlugins,
} from '@/api/spaces/spaces'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import { useCurrentSpace } from '@/features/spaces/current-space'
import type { ErrorType } from '@/lib/api-client'

/**
 * The server resyncs the marketplace every five minutes, so a catalog response
 * stays trustworthy for that long; the SSE catalog notice invalidates it
 * earlier when a sync commits.
 */
export const PLUGIN_CATALOG_STALE_MS = 5 * 60 * 1000

/**
 * Catalog snapshot of the current space's tenant, served straight from the
 * PostgreSQL snapshot — the UI never touches the marketplace network itself.
 */
export function usePluginCatalog(): {
  entries: PluginCatalogEntry[]
  syncedAt: string | null | undefined
  isPending: boolean
  isError: boolean
} {
  const { tenantId, space } = useCurrentSpace()
  const query = useQuery({
    queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsCatalogQueryKey(
      tenantId ?? '',
      space?.id ?? '',
    ),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidSpacesSpaceIdPluginsCatalog(
        tenantId ?? '',
        space?.id ?? '',
        undefined,
        signal,
      ),
    enabled: !!tenantId && !!space,
    staleTime: PLUGIN_CATALOG_STALE_MS,
  })
  const catalog: PluginCatalog | undefined = query.data
  return {
    entries: catalog?.items ?? [],
    syncedAt: catalog?.syncedAt,
    isPending: query.isLoading,
    isError: query.isError,
  }
}

/**
 * The current space's selected plugins with their desired/observed states;
 * refetched on every `space.plugins_updated` SSE notice and after mutations.
 */
export function useSpacePlugins(): {
  plugins: SpacePlugin[]
  isPending: boolean
  isError: boolean
} {
  const { tenantId, space } = useCurrentSpace()
  const query = useQuery({
    queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsQueryKey(tenantId ?? '', space?.id ?? ''),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidSpacesSpaceIdPlugins(tenantId ?? '', space?.id ?? '', undefined, signal),
    enabled: !!tenantId && !!space,
  })
  return { plugins: query.data?.items ?? [], isPending: query.isLoading, isError: query.isError }
}

/**
 * Invalidates the two plugin queries the current space renders; shared by the
 * install and remove mutations so their post-success behavior cannot drift.
 */
function useInvalidateSpacePlugins() {
  const queryClient = useQueryClient()
  const { tenantId, space } = useCurrentSpace()
  return (): void => {
    if (!tenantId || !space) return
    void queryClient.invalidateQueries({
      queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsQueryKey(tenantId, space.id),
    })
  }
}

/**
 * Installs one plugin into the current space, pinning the catalog version by
 * default. The server writes the desired state and fans the download out to
 * the runtime workspaces; the row's observed state converges over SSE.
 */
export function useInstallPlugin() {
  const { tenantId, space } = useCurrentSpace()
  const keyFor = useIdempotencyKeys()
  const invalidate = useInvalidateSpacePlugins()
  return useMutation<
    Awaited<ReturnType<typeof postApiV1TenantsTidSpacesSpaceIdPlugins>>,
    ErrorType<ApiError>,
    { identifier: string; pluginVersion?: string }
  >({
    mutationFn: async (input) => {
      if (!tenantId || !space) throw new Error('cloud space not resolved')
      const body: PostApiV1TenantsTidSpacesSpaceIdPluginsBody = { identifier: input.identifier }
      if (input.pluginVersion !== undefined) body.pluginVersion = input.pluginVersion
      return postApiV1TenantsTidSpacesSpaceIdPlugins(tenantId, space.id, body, {
        headers: mutationHeaders(keyFor(input)),
      })
    },
    onSuccess: invalidate,
  })
}

/**
 * Removes one plugin from the current space with an optimistic version guard;
 * a 409 surfaces the conflict and the row is refetched so the caller sees the
 * latest version before retrying.
 */
export function useRemovePlugin() {
  const { tenantId, space } = useCurrentSpace()
  const keyFor = useIdempotencyKeys()
  const invalidate = useInvalidateSpacePlugins()
  return useMutation<
    Awaited<ReturnType<typeof deleteApiV1TenantsTidSpacesSpaceIdPlugins>>,
    ErrorType<ApiError>,
    { identifier: string; version: number }
  >({
    mutationFn: async (input) => {
      if (!tenantId || !space) throw new Error('cloud space not resolved')
      return deleteApiV1TenantsTidSpacesSpaceIdPlugins(
        tenantId,
        space.id,
        { identifier: input.identifier, version: input.version },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: invalidate,
    onError: invalidate,
  })
}
