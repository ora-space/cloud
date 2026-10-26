import type { ReactNode } from 'react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { ThreadEntry, ThreadPreviewSnapshot } from '@/api/generated.schemas'
import { server } from '@/test/msw-server'
import { adoptThreadPreview, emptyPreview, mergeThreadPreview, parseThreadPreview } from './preview'
import { useThreadPreview } from './use-thread-preview'

const URL = '/api/v1/tenants/t/issues/i/runs/r/thread/preview/events'
const REF = { tid: 't', issueId: 'i', runId: 'r' }

function snapshot(version: number, text: string): ThreadPreviewSnapshot {
  return {
    available: true,
    executionId: 'execution',
    version,
    incomplete: false,
    blocks: [
      {
        historySequence: 4,
        turnId: 'turn',
        record: {
          type: 'update',
          update: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } },
        },
      },
    ],
  }
}

function encode(value: ThreadPreviewSnapshot) {
  return new TextEncoder().encode(`data: ${JSON.stringify(value)}\n\n`)
}

describe('Thread previews', () => {
  it('validates identities and adopts whole versions without appending duplicate text', () => {
    const latest = snapshot(3, 'complete preview')
    expect(parseThreadPreview(JSON.stringify(latest))).toEqual(latest)
    expect(
      parseThreadPreview(
        JSON.stringify({ ...latest, blocks: [...latest.blocks, ...latest.blocks] }),
      ),
    ).toBeNull()
    expect(parseThreadPreview('{broken')).toBeNull()
    expect(adoptThreadPreview(latest, snapshot(2, 'late'))).toBe(latest)
    expect(adoptThreadPreview(latest, snapshot(3, 'duplicate'))).toBe(latest)
    expect(
      adoptThreadPreview(latest, { ...snapshot(4, 'foreign'), executionId: 'old-execution' }),
    ).toBe(latest)
    expect(adoptThreadPreview(latest, snapshot(4, 'replacement')).blocks).toEqual(
      snapshot(4, 'replacement').blocks,
    )
  })

  it('never resurrects a persisted block after a late preview or reconnect snapshot', () => {
    const final: ThreadEntry = {
      seq: 12,
      source: 'node',
      kind: 'update',
      record: { type: 'update' },
      createdAt: '2026-10-18T01:00:00Z',
      historySequence: 4,
    }
    expect(mergeThreadPreview([final], snapshot(100, 'late preview'))).toEqual([final])
    const pending = mergeThreadPreview([], snapshot(1, 'first'))
    expect(pending).toHaveLength(1)
    expect(mergeThreadPreview([], snapshot(2, 'replacement'))[0]?.seq).toBe(pending[0]?.seq)
  })

  it('shows chunks before a final record and releases its stream and cache on unmount', async () => {
    let stream: ReadableStreamDefaultController<Uint8Array> | undefined
    let cancelled = false
    let aborted = false
    server.use(
      http.get(URL, ({ request }) => {
        request.signal.addEventListener('abort', () => {
          aborted = true
        })
        return new HttpResponse(
          new ReadableStream<Uint8Array>({
            start(controller) {
              stream = controller
              controller.enqueue(encode(snapshot(1, 'first chunk')))
            },
            cancel() {
              cancelled = true
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        )
      }),
    )
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )
    const hook = renderHook(() => useThreadPreview(REF, true), { wrapper })
    await waitFor(() => expect(hook.result.current.snapshot.version).toBe(1))
    await act(async () => {
      stream?.enqueue(encode(snapshot(2, 'first chunk plus next')))
    })
    await waitFor(() => expect(hook.result.current.snapshot.version).toBe(2))
    expect(hook.result.current.connection).toBe('live')
    hook.unmount()
    await waitFor(() => expect(aborted).toBe(true))
    if (!cancelled) stream?.close()
    await waitFor(() =>
      expect(client.getQueryData(['thread-preview', 't', 'i', 'r'])).toBeUndefined(),
    )
  })

  it('reconnects promptly after each healthy stream instead of compounding backoff', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
    try {
      let requests = 0
      server.use(
        http.get(URL, () => {
          requests++
          return new HttpResponse(
            new ReadableStream<Uint8Array>({
              start(controller) {
                controller.enqueue(encode(snapshot(requests, `stream ${requests}`)))
                controller.close()
              },
            }),
            { headers: { 'Content-Type': 'text/event-stream' } },
          )
        }),
      )
      const client = new QueryClient()
      const wrapper = ({ children }: { children: ReactNode }) => (
        <QueryClientProvider client={client}>{children}</QueryClientProvider>
      )
      const hook = renderHook(() => useThreadPreview(REF, true), { wrapper })
      // Compounding backoff would wait 1s, 2s, then 4s; healthy streams keep the first delay.
      for (let elapsed = 0; elapsed < 3000; elapsed += 100) {
        // oxlint-disable-next-line no-await-in-loop -- each step lets the stream settle
        await act(() => vi.advanceTimersByTimeAsync(100))
      }
      expect(requests).toBe(4)
      hook.unmount()
    } finally {
      vi.useRealTimers()
    }
  })

  it('clears optional content and stops reconnecting when membership is refused', async () => {
    let requests = 0
    server.use(
      http.get(URL, () => {
        requests++
        return new HttpResponse(null, { status: 403 })
      }),
    )
    const client = new QueryClient()
    client.setQueryData(['thread-preview', 't', 'i', 'r'], snapshot(1, 'private'))
    const wrapper = ({ children }: { children: ReactNode }) => (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    )
    const hook = renderHook(() => useThreadPreview(REF, true), { wrapper })
    await waitFor(() => expect(hook.result.current.connection).toBe('unavailable'))
    expect(hook.result.current.snapshot).toEqual(emptyPreview())
    expect(requests).toBe(1)
    hook.unmount()
  })
})
