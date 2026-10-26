import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import { mockApi } from './mock-api-client'

describe('mock API response boundary', () => {
  it('rejects the SPA fallback instead of caching HTML as inbox items', async () => {
    server.use(
      http.get('/mock-api/workspaces/:slug/inbox', () =>
        HttpResponse.html('<!doctype html><html><body>Application</body></html>'),
      ),
    )
    await expect(mockApi.get('/workspaces/test/inbox')).rejects.toThrow('non-JSON response')
  })

  it('accepts JSON again when the worker recovers', async () => {
    server.use(http.get('/mock-api/workspaces/:slug/inbox', () => HttpResponse.json([])))
    const response = await mockApi.get('/workspaces/test/inbox')
    expect(response.data).toEqual([])
  })

  it('accepts an empty 204 response', async () => {
    server.use(
      http.post(
        '/mock-api/workspaces/:slug/inbox/mark-all-read',
        () => new HttpResponse(null, { status: 204 }),
      ),
    )
    const response = await mockApi.post('/workspaces/test/inbox/mark-all-read')
    expect(response.status).toBe(204)
  })
})
