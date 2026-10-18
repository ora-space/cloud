import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  Error as ApiError,
  PluginCatalog,
  PluginCatalogEntry,
  PostApiV1TenantsTidSpacesSpaceIdPluginsBody,
  SpacePlugin,
  WorkflowAssetImport,
} from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidSpacesSpaceIdPlugins,
  getApiV1TenantsTidSpacesSpaceIdPlugins,
  getApiV1TenantsTidSpacesSpaceIdPluginsAssets,
  getApiV1TenantsTidSpacesSpaceIdPluginsCatalog,
  getGetApiV1TenantsTidSpacesSpaceIdPluginsAssetsQueryKey,
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
 * Poll cadence while a workflow asset import is still running. The server's
 * import loop runs every 30 seconds and publishes no SSE notice of its own, so
 * the screen re-reads the separate import query until every row is terminal.
 */
export const ASSET_IMPORT_POLL_INTERVAL_MS = 10 * 1000

/**
 * The current space's workflow asset imports. Install state and import state
 * are separate reads on purpose: a completed install can still have a pending
 * or failed document import, and the two must be presented independently. The
 * query polls only while at least one import is pending or importing.
 */
export function useWorkflowAssetImports(): {
  imports: WorkflowAssetImport[]
  isPending: boolean
  isError: boolean
} {
  const { tenantId, space } = useCurrentSpace()
  const query = useQuery({
    queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsAssetsQueryKey(
      tenantId ?? '',
      space?.id ?? '',
    ),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidSpacesSpaceIdPluginsAssets(
        tenantId ?? '',
        space?.id ?? '',
        undefined,
        signal,
      ),
    enabled: !!tenantId && !!space,
    refetchInterval: (polled) =>
      (polled.state.data?.items ?? []).some(
        (item) => item.state === 'pending' || item.state === 'importing',
      )
        ? ASSET_IMPORT_POLL_INTERVAL_MS
        : false,
  })
  return {
    imports: query.data?.items ?? [],
    isPending: query.isLoading,
    isError: query.isError,
  }
}

/**
 * Invalidates the plugin queries the current space renders — the selection
 * rows and the separately readable workflow asset imports; shared by the
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
    void queryClient.invalidateQueries({
      queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsAssetsQueryKey(tenantId, space.id),
    })
  }
}

/**
 * Records an administrator selection with the current resource version (zero for a new row).
 * Pins the catalog version by
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
    { identifier: string; pluginVersion?: string; version?: number }
  >({
    mutationFn: async (input) => {
      if (!tenantId || !space) throw new Error('cloud space not resolved')
      const body: PostApiV1TenantsTidSpacesSpaceIdPluginsBody = {
        identifier: input.identifier,
        version: input.version ?? 0,
      }
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
