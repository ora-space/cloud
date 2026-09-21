import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { CurrentSpaceProvider, useCurrentSpace } from '@/features/spaces/current-space'
import { parseSSEFrames } from '@/features/spaces/use-space-events'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'

function wrapper({ children }: { children: ReactNode }) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return (
    <QueryClientProvider client={queryClient}>
      <CurrentSpaceProvider slug="cloud-dev" authenticated>
        {children}
      </CurrentSpaceProvider>
    </QueryClientProvider>
  )
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
  beforeEach(() => {
    installCloudSpaceHandlers('owner')
  })

  it('uses the Gateway-authenticated Cloud context without browser tokens', async () => {
    const { result } = renderHook(() => useCurrentSpace(), { wrapper })
    await waitFor(() => {
      expect(result.current.tenantId).toBe(TEST_TENANT_ID)
      expect(result.current.space?.slug).toBe('cloud-dev')
    })
    expect(result.current.cloudMode).toBe(true)
  })

  it('leaves the space unresolved for a slug the member did not join', async () => {
    const { result } = renderHook(() => useCurrentSpace(), {
      wrapper: ({ children }: { children: ReactNode }) => {
        const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
        return (
          <QueryClientProvider client={queryClient}>
            <CurrentSpaceProvider slug="not-joined" authenticated>
              {children}
            </CurrentSpaceProvider>
          </QueryClientProvider>
        )
      },
    })
    await waitFor(() => {
      expect(result.current.tenantId).toBe(TEST_TENANT_ID)
    })
    expect(result.current.space).toBeUndefined()
  })
})
