import { describe, expect, it } from 'vitest'
import { AXIOS_INSTANCE } from '@/lib/api-client'
import { installFakeHttp } from './http'

describe('installFakeHttp', () => {
  it('records requests and answers with the configured body', async () => {
    const http = installFakeHttp({ ok: true })

    const response = await AXIOS_INSTANCE.get('/x')

    expect(response.data).toEqual({ ok: true })
    expect(http.requests).toEqual([
      {
        url: '/x',
        method: 'get',
        signal: undefined,
        idempotencyKey: undefined,
        authorization: undefined,
        userToken: undefined,
      },
    ])
  })

  it('rejects like axios for error statuses', async () => {
    installFakeHttp({ code: 'not_found' }, 404)

    await expect(AXIOS_INSTANCE.get('/missing')).rejects.toMatchObject({ status: 404 })
  })
})
