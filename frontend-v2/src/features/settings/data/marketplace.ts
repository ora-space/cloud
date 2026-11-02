import { useRef } from 'react'
import { create } from 'zustand'
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

const useMarketScope = create<{
  byTenant: Record<string, string>
  choose: (tenant: string, space: string) => void
}>((set) => ({
  byTenant: {},
  choose: (tenant, space) =>
    set((s) => ({ byTenant: { ...s.byTenant, [tenant]: space } })),
}))

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
  const scope = useMarketScope()
  const options = spaces.data?.items ?? []
  const spaceId =
    options.find((s) => s.id === scope.byTenant[tenantId])?.id ??
    options[0]?.id ??
    ''
  const keys = useRef(new Map<string, string>())
  const keyFor = (intent: string) => {
    const key = keys.current.get(intent) ?? crypto.randomUUID()
    keys.current.set(intent, key)
    return key
  }

  const catalog = useGetApiV1TenantsTidSpacesSpaceIdPluginsCatalog(
    tenantId,
    spaceId,
    { query: { enabled: tenantId !== '' && spaceId !== '' } },
  )
  const selections = useGetApiV1TenantsTidSpacesSpaceIdPlugins(
    tenantId,
    spaceId,
    {
      query: {
        enabled: tenantId !== '' && spaceId !== '',
        refetchInterval: 3000,
      },
    },
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
      if (entry.kind !== 'skill' && entry.kind !== 'agent')
        throw new Error('此插件类型不支持在此处安装。')
      const current = (selections.data?.items ?? []).find(
        (item) => pluginKey(item) === pluginKey(entry),
      )
      const intent = JSON.stringify([
        'install',
        tenantId,
        spaceId,
        pluginKey(entry),
        entry.version,
        current?.version ?? 0,
      ])
      const result = await postApiV1TenantsTidSpacesSpaceIdPlugins(
        tenantId,
        spaceId,
        {
          identifier: pluginKey(entry),
          pluginVersion: entry.version,
          version: current?.version ?? 0,
        },
        { headers: { 'Idempotency-Key': keyFor(intent) } },
      )
      keys.current.delete(intent)
      return result
    },
    onSuccess: invalidate,
    onError: () => void invalidate(),
  })
  const remove = useMutation({
    mutationFn: async (selection: SpacePlugin) => {
      if (!tenantId || !spaceId) throw new Error('cloud space not resolved')
      const intent = JSON.stringify([
        'remove',
        tenantId,
        spaceId,
        pluginKey(selection),
        selection.version,
      ])
      const result = await deleteApiV1TenantsTidSpacesSpaceIdPlugins(
        tenantId,
        spaceId,
        { identifier: pluginKey(selection), version: selection.version },
        { headers: { 'Idempotency-Key': keyFor(intent) } },
      )
      keys.current.delete(intent)
      return result
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
    spaces: options,
    chooseSpace: (id: string) => scope.choose(tenantId, id),
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
    error: tenants.error ?? spaces.error ?? catalog.error ?? selections.error,
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
