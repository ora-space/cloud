import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { describe, expect, it } from 'vitest'
import {
  PLUGIN_CATALOG_STALE_MS,
  useInstallPlugin,
  usePluginCatalog,
  useRemovePlugin,
  useSpacePlugins,
} from '@/features/plugins/api'
import { SessionProvider } from '@/features/auth/session'
import { CurrentSpaceProvider, useCurrentSpace } from '@/features/spaces/current-space'
import { jsonObject, stringField } from '@/mocks/handlers/shared'
import {
  catalogEntry,
  pluginCloudHandlers,
  PLUGIN_FIXTURE_SYNCED_AT,
  spacePlugin,
} from '@/mocks/handlers/plugins'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'

const SPACE_ID = '22222222-2222-2222-2222-222222222222'

/** Narrows an unknown JSON field to a number, or undefined. */
function numberField(value: unknown, key: string): number | undefined {
  const field = jsonObject(value)[key]
  return typeof field === 'number' ? field : undefined
}

/** Waits until the current space resolved, so tenant-scoped hooks enable. */
async function waitForSpace(queryClient: QueryClient): Promise<void> {
  const probe = renderHook(() => useCurrentSpace(), { wrapper: wrapperFor(queryClient) })
  await waitFor(() => expect(probe.result.current.space?.id).toBe(SPACE_ID))
}

/** Mounts the given hook under the app's query + current-space providers. */
function wrapperFor(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={queryClient}>
        <SessionProvider>
          <CurrentSpaceProvider slug="cloud-dev">{children}</CurrentSpaceProvider>
        </SessionProvider>
      </QueryClientProvider>
    )
  }
}

describe('usePluginCatalog', () => {
  it('serves the catalog snapshot and keeps it fresh for the sync cadence', async () => {
    installCloudSpaceHandlers('owner')
    let catalogReads = 0
    server.use(
      http.get('/api/v1/tenants/:tid/spaces/:spaceId/plugins/catalog', () => {
        catalogReads += 1
        return HttpResponse.json({
          items: [catalogEntry('official/hello-world', { title: 'Hello World' })],
          syncedAt: PLUGIN_FIXTURE_SYNCED_AT,
        })
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const first = renderHook(() => usePluginCatalog(), { wrapper: wrapperFor(queryClient) })
    await waitFor(() => expect(catalogReads).toBe(1))
    expect(first.result.current.isPending).toBe(false)

    // A second mount within the stale window answers from the cache: the
    // server-side sync only runs every five minutes, so five minutes is the
    // freshness contract.
    const second = renderHook(() => usePluginCatalog(), { wrapper: wrapperFor(queryClient) })
    await waitFor(() => expect(second.result.current.isPending).toBe(false))
    expect(catalogReads).toBe(1)
    expect(PLUGIN_CATALOG_STALE_MS).toBe(5 * 60 * 1000)
  })
})

describe('useSpacePlugins', () => {
  it('lists the current space rows', async () => {
    installCloudSpaceHandlers('owner')
    server.use(
      ...pluginCloudHandlers({
        catalog: [catalogEntry('official/hello-world')],
        plugins: [spacePlugin('official/hello-world', { observedState: 'installed' })],
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const { result } = renderHook(() => useSpacePlugins(), { wrapper: wrapperFor(queryClient) })
    await waitFor(() =>
      expect(result.current.plugins.map((p) => p.id)).toEqual(['official/hello-world']),
    )
  })
})

describe('useInstallPlugin', () => {
  it('posts the install and invalidates the space plugin query', async () => {
    installCloudSpaceHandlers('owner')
    const key = [`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${SPACE_ID}/plugins`]
    const posted: string[] = []
    server.use(
      http.post('/api/v1/tenants/:tid/spaces/:spaceId/plugins', async ({ request }) => {
        const body: unknown = await request.json()
        posted.push(stringField(jsonObject(body)['identifier']) ?? '')
        const row = spacePlugin('official/hello-world', { observedState: 'pending' })
        return HttpResponse.json({ resource: row })
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    queryClient.setQueryData(key, { items: [] })
    await waitForSpace(queryClient)
    const { result } = renderHook(() => useInstallPlugin(), { wrapper: wrapperFor(queryClient) })
    result.current.mutate({ identifier: 'official/hello-world' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(posted).toEqual(['official/hello-world'])
    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true)
  })
})

describe('useRemovePlugin', () => {
  it('deletes with the optimistic version and refetches after a conflict', async () => {
    installCloudSpaceHandlers('owner')
    const key = [`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${SPACE_ID}/plugins`]
    let conflict = true
    server.use(
      http.delete('/api/v1/tenants/:tid/spaces/:spaceId/plugins', async ({ request }) => {
        const body: unknown = await request.json()
        if (conflict) {
          conflict = false
          return HttpResponse.json(
            { code: 'version_conflict', params: {}, requestId: 'r' },
            { status: 409 },
          )
        }
        expect(numberField(body, 'version')).toBe(7)
        return HttpResponse.json({
          resource: spacePlugin('official/hello-world', {
            desiredState: 'removed',
            observedState: 'removed',
          }),
        })
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    queryClient.setQueryData(key, { items: [] })
    await waitForSpace(queryClient)
    const { result } = renderHook(() => useRemovePlugin(), { wrapper: wrapperFor(queryClient) })
    result.current.mutate({ identifier: 'official/hello-world', version: 7 })
    await waitFor(() => expect(result.current.isError).toBe(true))
    // The conflict refetches the latest row so the next attempt has a fresh
    // version; retrying with the same input then succeeds.
    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true)
    result.current.mutate({ identifier: 'official/hello-world', version: 7 })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
  })
})
