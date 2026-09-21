import { CanceledError } from 'axios'
import { describe, expect, it } from 'vitest'
import { installFakeHttp } from '@/test/http'
import { customInstance } from './api-client'
import { mockApi } from './mock-api-client'

describe('customInstance', () => {
  it('unwraps the response body', async () => {
    installFakeHttp({ status: 'ok' })

    await expect(customInstance({ url: '/healthz', method: 'GET' })).resolves.toEqual({
      status: 'ok',
    })
  })

  it('aborts the request when cancel() is called', async () => {
    const http = installFakeHttp({})

    const promise = customInstance({ url: '/healthz', method: 'GET' })
    promise.cancel()

    await expect(promise).rejects.toBeInstanceOf(CanceledError)
    expect(http.requests[0]?.signal?.aborted).toBe(true)
  })

  it('aborts the request when the caller signal aborts', async () => {
    const http = installFakeHttp({})
    const caller = new AbortController()

    const promise = customInstance({ url: '/healthz', method: 'GET', signal: caller.signal })
    caller.abort()

    await expect(promise).rejects.toBeInstanceOf(CanceledError)
    expect(http.requests[0]?.signal?.aborted).toBe(true)
  })

  it('adds an idempotency key to writes without browser-readable credentials', async () => {
    const http = installFakeHttp({})

    await customInstance({ url: '/api/v1/projects', method: 'POST' })

    expect(http.requests[0]?.idempotencyKey).toMatch(/^[0-9a-f-]{36}$/)
    expect(http.requests[0]?.authorization).toBeUndefined()
    expect(http.requests[0]?.userToken).toBeUndefined()
  })

  it('preserves a caller-provided idempotency key', async () => {
    const http = installFakeHttp({})

    await customInstance({
      url: '/api/v1/projects',
      method: 'DELETE',
      headers: { 'Idempotency-Key': 'operation-1' },
    })

    expect(http.requests[0]?.idempotencyKey).toBe('operation-1')
  })
})

describe('mockApi', () => {
  it('isolates simulated business requests without adding authentication state', () => {
    expect(mockApi.defaults.baseURL).toBe('/mock-api')
    expect(mockApi.defaults.headers.common.Authorization).toBeUndefined()
  })
})
