/** Splits SSE data lines while retaining the one incomplete frame between network reads. */
export function eventStreamFrames(buffer: string): { data: string[]; rest: string } {
  const data: string[] = []
  let rest = buffer
  for (;;) {
    const boundary = /\r?\n\r?\n/u.exec(rest)
    if (!boundary) return { data, rest }
    const frame = rest.slice(0, boundary.index)
    rest = rest.slice(boundary.index + boundary[0].length)
    const lines = frame.split(/\r?\n/u).filter((line) => line.startsWith('data:'))
    if (lines.length) data.push(lines.map((line) => line.slice(5).trimStart()).join('\n'))
  }
}

/** Reads a bounded SSE stream and always releases its reader on abort, parse error or disconnect. */
export async function drainEventStream(
  body: ReadableStream<Uint8Array>,
  onData: (data: string) => void,
  signal?: AbortSignal,
): Promise<void> {
  const reader = body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''
  const cancel = () => {
    void reader.cancel().catch(() => undefined)
  }
  signal?.addEventListener('abort', cancel, { once: true })
  if (signal?.aborted) cancel()
  try {
    for (;;) {
      // oxlint-disable-next-line no-await-in-loop -- stream reads must retain wire order
      const next = await reader.read()
      if (next.done) return
      buffer += decoder.decode(next.value, { stream: true })
      if (buffer.length > 512 * 1024) throw new Error('Event stream frame exceeds the size limit')
      const frames = eventStreamFrames(buffer)
      buffer = frames.rest
      frames.data.forEach(onData)
    }
  } finally {
    signal?.removeEventListener('abort', cancel)
    cancel()
    reader.releaseLock()
  }
}

/** Reconnect delay that aborts together with the subscription rather than retaining a timer. */
export function pauseEventStream(signal: AbortSignal, delay: number): Promise<void> {
  return new Promise((resolve) => {
    const finish = () => {
      clearTimeout(timer)
      signal.removeEventListener('abort', finish)
      resolve()
    }
    const timer = setTimeout(finish, delay)
    signal.addEventListener('abort', finish, { once: true })
    if (signal.aborted) finish()
  })
}
