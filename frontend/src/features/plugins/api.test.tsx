import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  ASSET_IMPORT_POLL_INTERVAL_MS,
  PLUGIN_CATALOG_STALE_MS,
  useInstallPlugin,
  usePluginCatalog,
  useRemovePlugin,
  useSpacePlugins,
  useWorkflowAssetImports,
} from '@/features/plugins/api'
import { SessionProvider } from '@/features/auth/session'
import { CurrentSpaceProvider, useCurrentSpace } from '@/features/spaces/current-space'
import { jsonObject, stringField } from '@/mocks/handlers/shared'
import {
  assetImport,
  catalogEntry,
  importWithNameConflict,
  pluginCloudHandlers,
  PLUGIN_FIXTURE_SYNCED_AT,
  spacePlugin,
} from '@/mocks/handlers/plugins'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'

const SPACE_ID = '22222222-2222-2222-2222-222222222222'

afterEach(() => {
  vi.useRealTimers()
})

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
    installCloudSpaceHandlers('admin')
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
    installCloudSpaceHandlers('admin')
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

describe('useWorkflowAssetImports', () => {
  it('reads import rows and their document outcomes independently of install state', async () => {
    installCloudSpaceHandlers('admin')
    server.use(
      ...pluginCloudHandlers({
        catalog: [],
        plugins: [spacePlugin('official/demo', { observedState: 'installed' })],
        assetImports: [importWithNameConflict('official/demo')],
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const { result } = renderHook(() => useWorkflowAssetImports(), {
      wrapper: wrapperFor(queryClient),
    })
    await waitFor(() => expect(result.current.imports).toHaveLength(1))
    // The installed install row and the import row are two independent facts.
    expect(result.current.imports[0]?.state).toBe('completed')
    expect(
      result.current.imports[0]?.documents.map((document) => [
        document.documentPath,
        document.outcome,
      ]),
    ).toEqual([
      ['assets/workflows/release.json', 'imported'],
      ['assets/workflows/deploy.json', 'failed'],
    ])
  })

  it('polls while an import runs and stops once every row is terminal', async () => {
    // Only the query's polling interval is faked: testing-library's waitFor
    // polls on setInterval too, so the test flushes real timers instead.
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    installCloudSpaceHandlers('admin')
    let reads = 0
    server.use(
      http.get('/api/v1/tenants/:tid/spaces/:spaceId/plugins/assets', () => {
        reads += 1
        return HttpResponse.json({
          items: [assetImport('official/demo', { state: reads === 1 ? 'importing' : 'completed' })],
        })
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const { result } = renderHook(() => useWorkflowAssetImports(), {
      wrapper: wrapperFor(queryClient),
    })
    const settle = async (state: string, attempt = 0): Promise<void> => {
      if (result.current.imports[0]?.state === state || attempt >= 50) return
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 5))
      })
      await settle(state, attempt + 1)
    }
    await settle('importing')
    expect(result.current.imports[0]?.state).toBe('importing')
    expect(reads).toBe(1)

    await act(() => vi.advanceTimersByTimeAsync(ASSET_IMPORT_POLL_INTERVAL_MS))
    await settle('completed')
    expect(result.current.imports[0]?.state).toBe('completed')
    expect(reads).toBe(2)

    // A terminal row clears the interval: further ticks issue no request.
    await act(() => vi.advanceTimersByTimeAsync(ASSET_IMPORT_POLL_INTERVAL_MS * 2))
    expect(reads).toBe(2)
  })
})

describe('useInstallPlugin', () => {
  it('posts the install and invalidates the space plugin query', async () => {
    installCloudSpaceHandlers('admin')
    const key = [`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${SPACE_ID}/plugins`]
    const assetsKey = [`/api/v1/tenants/${TEST_TENANT_ID}/spaces/${SPACE_ID}/plugins/assets`]
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
    queryClient.setQueryData(assetsKey, { items: [] })
    await waitForSpace(queryClient)
    const { result } = renderHook(() => useInstallPlugin(), { wrapper: wrapperFor(queryClient) })
    result.current.mutate({ identifier: 'official/hello-world' })
    await waitFor(() => expect(result.current.isSuccess).toBe(true))
    expect(posted).toEqual(['official/hello-world'])
    expect(queryClient.getQueryState(key)?.isInvalidated).toBe(true)
    // The install transaction writes the import request row, so the separate
    // asset query is refetched too.
    expect(queryClient.getQueryState(assetsKey)?.isInvalidated).toBe(true)
  })
})

describe('useRemovePlugin', () => {
  it('deletes with the optimistic version and refetches after a conflict', async () => {
    installCloudSpaceHandlers('admin')
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
