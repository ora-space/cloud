import { describe, expect, it } from 'vitest'
import { drainEventStream, eventStreamFrames, pauseEventStream } from './event-stream'

describe('event stream boundary', () => {
  it('retains partial frames and accepts CRLF and multiline data without losing order', () => {
    expect(eventStreamFrames('data: first\r\n\r\ndata: second\ndata: line\n\ndata: par')).toEqual({
      data: ['first', 'second\nline'],
      rest: 'data: par',
    })
  })
  it('rejects an oversized partial frame and cancels the underlying reader', async () => {
    let cancelled = false
    const body = new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new Uint8Array(512 * 1024 + 1))
      },
      cancel() {
        cancelled = true
      },
    })
    await expect(drainEventStream(body, () => undefined)).rejects.toThrow('size limit')
    expect(cancelled).toBe(true)
    expect(body.locked).toBe(false)
  })
  it('ends a reconnect wait immediately after cancellation', async () => {
    const controller = new AbortController()
    const waiting = pauseEventStream(controller.signal, 30000)
    controller.abort()
    await expect(waiting).resolves.toBeUndefined()
  })
})
