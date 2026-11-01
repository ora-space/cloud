import { useMutation, useQueryClient } from '@tanstack/react-query'
import type { PluginCatalogEntry, SpacePlugin } from '@/api/generated.schemas'
import { useGetApiV1MeTenants } from '@/api/me/me'
import {
  deleteApiV1TenantsTidSpacesSpaceIdPlugins,
  getGetApiV1TenantsTidSpacesSpaceIdPluginsQueryKey,
  useGetApiV1TenantsTidSpaces,
  useGetApiV1TenantsTidSpacesSpaceIdPlugins,
  useGetApiV1TenantsTidSpacesSpaceIdPluginsCatalog,
  postApiV1TenantsTidSpacesSpaceIdPlugins,
} from '@/api/spaces/spaces'
import { useRole } from '@/app/auth/user'

/** The canonical marketplace identity shared by catalog entries and space selections. */
export const pluginKey = (plugin: {
  sourceNamespace: string
  identifier: string
}) => `${plugin.sourceNamespace}/${plugin.identifier}`

/** A space selection represents a usable install only after desired and observed both settle. */
export const pluginInstalled = (selection?: SpacePlugin) =>
  selection?.desiredState === 'installed' &&
  selection.observedState === 'installed'

/**
 * Reads the current workspace's marketplace catalog and Space selections through generated Cloud
 * clients. Writes are owner-only in v2 and carry a per-request idempotency key plus the current
 * selection version.
 */
export function usePluginMarketplace() {
  const tenants = useGetApiV1MeTenants()
  const tenantId = tenants.data?.items?.[0]?.id ?? ''
  const spaces = useGetApiV1TenantsTidSpaces(tenantId, undefined, {
    query: { enabled: tenantId !== '' },
  })
  const spaceId = spaces.data?.items?.[0]?.id ?? ''
  const catalog = useGetApiV1TenantsTidSpacesSpaceIdPluginsCatalog(
    tenantId,
    spaceId,
    { query: { enabled: tenantId !== '' && spaceId !== '' } },
  )
  const selections = useGetApiV1TenantsTidSpacesSpaceIdPlugins(
    tenantId,
    spaceId,
    { query: { enabled: tenantId !== '' && spaceId !== '' } },
  )
  const client = useQueryClient()
  const canManage = useRole() === 'owner'
  const invalidate = async () => {
    if (!tenantId || !spaceId) return
    await client.invalidateQueries({
      queryKey: getGetApiV1TenantsTidSpacesSpaceIdPluginsQueryKey(
        tenantId,
        spaceId,
      ),
    })
  }
  const install = useMutation({
    mutationFn: async (entry: PluginCatalogEntry) => {
      if (!tenantId || !spaceId) throw new Error('cloud space not resolved')
      if (entry.kind === 'agent' || entry.kind === 'pack')
        throw new Error('此插件类型不支持在此处安装。')
      const current = (selections.data?.items ?? []).find(
        (item) => pluginKey(item) === pluginKey(entry),
      )
      return postApiV1TenantsTidSpacesSpaceIdPlugins(
        tenantId,
        spaceId,
        {
          identifier: pluginKey(entry),
          pluginVersion: entry.version,
          version: current?.version ?? 0,
        },
        { headers: { 'Idempotency-Key': crypto.randomUUID() } },
      )
    },
    onSuccess: invalidate,
    onError: () => void invalidate(),
  })
  const remove = useMutation({
    mutationFn: async (selection: SpacePlugin) => {
      if (!tenantId || !spaceId) throw new Error('cloud space not resolved')
      return deleteApiV1TenantsTidSpacesSpaceIdPlugins(
        tenantId,
        spaceId,
        { identifier: pluginKey(selection), version: selection.version },
        { headers: { 'Idempotency-Key': crypto.randomUUID() } },
      )
    },
    onSuccess: invalidate,
    onError: () => void invalidate(),
  })
  const catalogItems = catalog.data?.items ?? []
  const installations = selections.data?.items ?? []
  const byKey = new Map(
    installations.map((selection) => [pluginKey(selection), selection]),
  )
  return {
    tenantId,
    spaceId,
    catalog: catalogItems,
    syncedAt: catalog.data?.syncedAt ?? undefined,
    installations,
    selection: (entry: { sourceNamespace: string; identifier: string }) =>
      byKey.get(pluginKey(entry)),
    canManage,
    pending:
      tenants.isLoading ||
      spaces.isLoading ||
      catalog.isLoading ||
      selections.isLoading,
    error:
      tenants.error ??
      spaces.error ??
      catalog.error ??
      selections.error,
    mutationError: install.error ?? remove.error,
    mutating: install.isPending || remove.isPending,
    resetErrors: () => {
      install.reset()
      remove.reset()
    },
    install: install.mutateAsync,
    remove: remove.mutateAsync,
    refetch: async () => {
      await Promise.all([catalog.refetch(), selections.refetch()])
    },
  }
}
