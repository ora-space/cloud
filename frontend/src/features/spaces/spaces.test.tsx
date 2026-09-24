import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { delay } from 'msw'
import { http, HttpResponse } from 'msw'
import type { ReactNode } from 'react'
import { describe, expect, it } from 'vitest'
import { SessionProvider } from '@/features/auth/session'
import {
  normalizeSpaceRole,
  useArchiveSpace,
  useCreateSpace,
  useJoinedSpaces,
  useSpaceMembers,
  useSpaceProjects,
  useUpdateSpace,
  useUpdateSpaceMember,
} from '@/features/spaces/api'
import { CurrentSpaceProvider, useCurrentSpace } from '@/features/spaces/current-space'
import { isValidSlug, slugFromName } from '@/features/spaces/slug'
import { parseSSEFrames, reconnectDelay, useSpaceEvents } from '@/features/spaces/use-space-events'
import {
  installCloudSpaceHandlers,
  installSignedInSession,
  TEST_TENANT_ID,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'

function wrapperFor(slug: string) {
  return function Wrapper({ children }: { children: ReactNode }) {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    return (
      <QueryClientProvider client={queryClient}>
        <SessionProvider>
          <CurrentSpaceProvider slug={slug}>{children}</CurrentSpaceProvider>
        </SessionProvider>
      </QueryClientProvider>
    )
  }
}
const wrapper = wrapperFor('cloud-dev')

function eventStream(frames: string[]): HttpResponse<ReadableStream<Uint8Array>> {
  const encoder = new TextEncoder()
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const frame of frames) controller.enqueue(encoder.encode(frame))
      controller.close()
    },
  })
  return new HttpResponse(body, { headers: { 'Content-Type': 'text/event-stream' } })
}

const queryWrapper = ({ children }: { children: ReactNode }) => {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
}

describe('parseSSEFrames', () => {
  it('splits complete frames into typed events and keeps partial tails', () => {
    const stream =
      'data: {"type":"project.updated","spaceId":"s","projectId":"p","version":3}\n\n' +
      'data: {"type":"space.updated","spaceId":"s"}\n\n' +
      'data: {"type":"project.cr'
    const { events, rest } = parseSSEFrames(stream)
    expect(events).toEqual([
      { type: 'project.updated', spaceId: 's', projectId: 'p', version: 3 },
      { type: 'space.updated', spaceId: 's' },
    ])
    expect(rest).toBe('data: {"type":"project.cr')
  })

  it('ignores malformed data payloads and non-data lines', () => {
    const stream =
      'retry: 1000\n\ndata: not-json\n\ndata: {"type":"space.updated","spaceId":"s"}\n\n'
    const { events } = parseSSEFrames(stream)
    expect(events).toEqual([{ type: 'space.updated', spaceId: 's' }])
  })
})

describe('CurrentSpaceProvider', () => {
  it('fetches nothing and stays pending while signed out', async () => {
    const { result } = renderHook(() => useCurrentSpace(), { wrapper })
    // Give the session probe time to settle at 401; no tenant request follows.
    await waitFor(() => expect(result.current.isPending).toBe(true))
    expect(result.current.space).toBeUndefined()
    expect(result.current.tenantId).toBeUndefined()
    expect(result.current.spaces).toBeUndefined()
  })

  it('resolves the route slug against the real spaces list once signed in', async () => {
    installCloudSpaceHandlers('owner')
    const { result } = renderHook(() => useCurrentSpace(), { wrapper })
    await waitFor(() => {
      expect(result.current.tenantId).toBe(TEST_TENANT_ID)
      expect(result.current.space?.slug).toBe('cloud-dev')
    })
    expect(result.current.isPending).toBe(false)
  })

  it('leaves the space unresolved for a slug the member did not join', async () => {
    installCloudSpaceHandlers('owner')
    const { result } = renderHook(() => useCurrentSpace(), { wrapper: wrapperFor('not-joined') })
    await waitFor(() => {
      expect(result.current.tenantId).toBe(TEST_TENANT_ID)
      expect(result.current.isPending).toBe(false)
    })
    expect(result.current.space).toBeUndefined()
    expect(result.current.spaces).toHaveLength(1)
  })
})

describe('useJoinedSpaces', () => {
  it('resolves to an empty list, not pending, for a member with no tenant', async () => {
    installSignedInSession()
    server.use(
      http.get('/api/v1/me/tenants', () => HttpResponse.json({ items: [], nextCursor: '' })),
    )
    const { result } = renderHook(() => useJoinedSpaces(), { wrapper })
    await waitFor(() => expect(result.current.isPending).toBe(false))
    expect(result.current).toEqual({
      tenantId: undefined,
      spaces: [],
      isPending: false,
      isError: false,
    })
  })

  it('reports an error when the tenant list fails for a non-auth reason', async () => {
    installSignedInSession()
    server.use(
      http.get('/api/v1/me/tenants', () =>
        HttpResponse.json({ code: 'internal_error', params: {}, requestId: 'r' }, { status: 500 }),
      ),
    )
    const { result } = renderHook(() => useJoinedSpaces(), { wrapper })
    await waitFor(() => expect(result.current.isError).toBe(true))
    expect(result.current.isPending).toBe(false)
    expect(result.current.spaces).toBeUndefined()
  })
})

describe('slug', () => {
  it('derives a backend-valid slug from a display name', () => {
    expect(slugFromName('Acme Inc')).toBe('acme-inc')
    expect(slugFromName('  --Hello, World!--  ')).toBe('hello-world')
    expect(slugFromName('研发组织')).toBe('')
    expect(slugFromName('a'.repeat(80))).toHaveLength(64)
    expect(isValidSlug(slugFromName('Acme Inc'))).toBe(true)
  })

  it('accepts exactly what the backend pattern accepts', () => {
    expect(isValidSlug('acme')).toBe(true)
    expect(isValidSlug('a1-b2')).toBe(true)
    expect(isValidSlug('-acme')).toBe(false)
    expect(isValidSlug('Acme')).toBe(false)
    expect(isValidSlug('')).toBe(false)
    expect(isValidSlug('a'.repeat(65))).toBe(false)
  })
})

describe('reconnectDelay', () => {
  it('doubles from one second and caps at thirty', () => {
    expect([0, 1, 2, 3, 4, 5, 10].map(reconnectDelay)).toEqual([
      1000, 2000, 4000, 8000, 16000, 30000, 30000,
    ])
  })
})

describe('useSpaceEvents', () => {
  const tenantId = TEST_TENANT_ID
  const spaceId = '22222222-2222-2222-2222-222222222222'
  const eventsUrl = `/api/v1/tenants/${tenantId}/spaces/${spaceId}/events`

  /** Renders the events hook over a client seeded with the given query keys. */
  function renderEvents(
    queryClient: QueryClient,
    keys: string[][],
  ): { unmount: () => void; invalidated: (key: string[]) => boolean } {
    for (const key of keys) {
      queryClient.setQueryData(key, [])
    }
    const { unmount } = renderHook(() => useSpaceEvents(tenantId, spaceId), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    })
    return {
      unmount,
      invalidated: (key) => queryClient.getQueryState(key)?.isInvalidated === true,
    }
  }

  it('invalidates the queries each event names, reconnects after the stream ends, and stops on 401', async () => {
    let connections = 0
    server.use(
      http.get(eventsUrl, async () => {
        connections += 1
        if (connections === 1) {
          return eventStream([
            'data: {"type":"project.created","spaceId":"S","projectId":"P"}\n\n',
            'data: {"type":"space.member_updated","spaceId":"S"}\n\n',
            'data: {"type":"space.updated","spaceId":"S","version":2}\n\n',
          ])
        }
        if (connections === 2) {
          // A dropped connection: the server closes without a frame.
          await delay(10)
          return eventStream([])
        }
        return HttpResponse.json(
          { code: 'unauthenticated', params: {}, requestId: 'r' },
          { status: 401 },
        )
      }),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const projectsKey = [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/projects`]
    const membersKey = [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/members`]
    const spacesKey = [`/api/v1/tenants/${tenantId}/spaces`]
    const foreignKey = ['/api/v1/tenants/other/spaces']
    const { unmount, invalidated } = renderEvents(queryClient, [
      projectsKey,
      membersKey,
      spacesKey,
      foreignKey,
    ])

    await waitFor(() => expect(invalidated(spacesKey)).toBe(true))
    expect(invalidated(projectsKey)).toBe(true)
    expect(invalidated(membersKey)).toBe(true)
    expect(invalidated(foreignKey)).toBe(false)

    // Second connection after the 1s backoff; the reconnect refetches the
    // tenant's queries, then the third answers 401 and the loop ends.
    await waitFor(() => expect(connections).toBe(3), { timeout: 5000 })
    await new Promise((resolve) => setTimeout(resolve, 1200))
    expect(connections).toBe(3)
    unmount()
  })

  it('invalidates the plugin queries the plugin event types name', async () => {
    server.use(
      http.get(eventsUrl, () =>
        eventStream([
          'data: {"type":"space.plugins_updated","spaceId":"S","version":2}\n\n',
          'data: {"type":"plugins.catalog_updated","spaceId":""}\n\n',
        ]),
      ),
    )
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const pluginsKey = [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/plugins`]
    const catalogKey = [`/api/v1/tenants/${tenantId}/spaces/${spaceId}/plugins/catalog`]
    const { unmount, invalidated } = renderEvents(queryClient, [pluginsKey, catalogKey])

    await waitFor(() => expect(invalidated(pluginsKey)).toBe(true))
    expect(invalidated(catalogKey)).toBe(true)
    unmount()
  })

  it('does nothing without a tenant or space', async () => {
    let connections = 0
    server.use(
      http.get(eventsUrl, () => {
        connections += 1
        return eventStream([])
      }),
    )
    const queryClient = new QueryClient()
    renderHook(() => useSpaceEvents(undefined, spaceId), {
      wrapper: ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
      ),
    })
    await new Promise((resolve) => setTimeout(resolve, 50))
    expect(connections).toBe(0)
  })
})

describe('space hooks without a resolved tenant or space', () => {
  it('keeps member and project queries disabled', () => {
    const members = renderHook(() => useSpaceMembers(undefined, undefined), {
      wrapper: queryWrapper,
    })
    const projects = renderHook(() => useSpaceProjects(TEST_TENANT_ID, undefined), {
      wrapper: queryWrapper,
    })
    expect(members.result.current.fetchStatus).toBe('idle')
    expect(projects.result.current.fetchStatus).toBe('idle')
  })

  it('lets mutations settle without invalidating anything', async () => {
    const calls: string[] = []
    server.use(
      // Without ids the generated client still builds these (empty-segment) paths.
      http.post('/api/v1/tenants//spaces', () => {
        calls.push('create')
        return HttpResponse.json({ id: 's', slug: 'x', version: 1 })
      }),
      http.patch('/api/v1/tenants//spaces/', () => {
        calls.push('update')
        return HttpResponse.json({ id: 's', slug: 'x', version: 2 })
      }),
      http.delete('/api/v1/tenants//spaces/', () => {
        calls.push('archive')
        return HttpResponse.json({ id: 's', slug: 'x', version: 3 })
      }),
      http.put('/api/v1/tenants//spaces//members/u', () => {
        calls.push('member')
        return HttpResponse.json({ userId: 'u', role: 'member', status: 'active', version: 1 })
      }),
    )
    const create = renderHook(() => useCreateSpace(undefined), { wrapper: queryWrapper })
    const update = renderHook(() => useUpdateSpace(undefined, undefined), { wrapper: queryWrapper })
    const archive = renderHook(() => useArchiveSpace(undefined, undefined), {
      wrapper: queryWrapper,
    })
    const member = renderHook(() => useUpdateSpaceMember(undefined, undefined), {
      wrapper: queryWrapper,
    })

    await create.result.current.mutateAsync({ name: 'n', slug: 'x', description: '' })
    await update.result.current.mutateAsync({ name: 'n', description: '', version: 1 })
    await archive.result.current.mutateAsync(2)
    await member.result.current.mutateAsync({
      userId: 'u',
      role: 'member',
      status: 'active',
      version: 0,
    })
    expect(calls).toEqual(['create', 'update', 'archive', 'member'])
  })

  it('normalizes unknown roles to member', () => {
    expect(normalizeSpaceRole('owner')).toBe('owner')
    expect(normalizeSpaceRole('superuser')).toBe('member')
  })
})
