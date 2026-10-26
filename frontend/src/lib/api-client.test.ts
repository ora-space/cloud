import { CanceledError } from 'axios'
import { describe, expect, it, onTestFinished } from 'vitest'
import { installFakeHttp } from '@/test/http'
import {
  customInstance,
  faultCode,
  faultParam,
  isUnauthorizedError,
  onUnauthorized,
} from './api-client'

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
})

describe('HTTP policy', () => {
  it('adds an idempotency key to POST and DELETE but not to GET', async () => {
    const http = installFakeHttp({})

    await customInstance({ url: '/api/v1/tenants', method: 'POST', data: {} })
    await customInstance({ url: '/api/v1/tenants/x', method: 'DELETE', data: {} })
    await customInstance({ url: '/api/v1/me', method: 'GET' })

    expect(http.requests.map((r) => typeof r.headers['Idempotency-Key'])).toEqual([
      'string',
      'string',
      'undefined',
    ])
  })

  it('keeps a caller-supplied idempotency key', async () => {
    const http = installFakeHttp({})

    await customInstance({
      url: '/api/v1/tenants',
      method: 'POST',
      data: {},
      headers: { 'Idempotency-Key': 'caller-key' },
    })

    expect(http.requests[0]?.headers['Idempotency-Key']).toBe('caller-key')
  })

  it('never attaches credential headers; the session is a cookie the browser owns', async () => {
    const http = installFakeHttp({})

    await customInstance({ url: '/api/v1/me', method: 'GET' })

    expect(http.requests[0]?.headers['Authorization']).toBeUndefined()
    expect(http.requests[0]?.headers['X-Ora-User-Token']).toBeUndefined()
  })

  it('notifies unauthorized listeners on 401 and still rejects the caller', async () => {
    installFakeHttp({ code: 'unauthenticated' }, 401)
    const seen: number[] = []
    const stop = onUnauthorized(() => seen.push(1))

    await expect(customInstance({ url: '/api/v1/me', method: 'GET' })).rejects.toSatisfy(
      isUnauthorizedError,
    )
    expect(seen).toEqual([1])

    stop()
    await expect(customInstance({ url: '/api/v1/me', method: 'GET' })).rejects.toSatisfy(
      isUnauthorizedError,
    )
    expect(seen).toEqual([1])
  })

  it('does not treat other failures as unauthorized', async () => {
    installFakeHttp({ code: 'not_found' }, 404)
    const seen: number[] = []
    onTestFinished(onUnauthorized(() => seen.push(1)))

    await expect(customInstance({ url: '/api/v1/me', method: 'GET' })).rejects.not.toSatisfy(
      isUnauthorizedError,
    )
    expect(seen).toEqual([])
  })
})

describe('faultCode', () => {
  it('extracts the backend Fault.code from a rejected axios request', () => {
    const error = { isAxiosError: true, response: { data: { code: 'not_found' } } }
    expect(faultCode(error)).toBe('not_found')
  })

  it('returns undefined when the response carries no string code', () => {
    expect(faultCode({ isAxiosError: true })).toBeUndefined()
    expect(faultCode({ isAxiosError: true, response: {} })).toBeUndefined()
    expect(faultCode({ isAxiosError: true, response: { data: { code: 404 } } })).toBeUndefined()
  })

  it('returns undefined for failures that are not axios errors', () => {
    expect(faultCode(new Error('network down'))).toBeUndefined()
    expect(faultCode(undefined)).toBeUndefined()
  })
})

describe('faultParam', () => {
  it('extracts a string parameter the backend marked safe for the client', () => {
    const error = {
      isAxiosError: true,
      response: { data: { code: 'required_field_missing', params: { field: 'prompt' } } },
    }
    expect(faultParam(error, 'field')).toBe('prompt')
  })

  it('returns undefined for missing, non-string or non-axios parameters', () => {
    expect(faultParam({ isAxiosError: true }, 'field')).toBeUndefined()
    expect(faultParam({ isAxiosError: true, response: {} }, 'field')).toBeUndefined()
    expect(
      faultParam({ isAxiosError: true, response: { data: { params: { field: 3 } } } }, 'field'),
    ).toBeUndefined()
    expect(faultParam(new Error('network down'), 'field')).toBeUndefined()
    expect(faultParam(undefined, 'field')).toBeUndefined()
  })
})
