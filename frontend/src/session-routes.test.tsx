import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'
import { installCloudSpaceHandlers, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderRoutes } from '@/test/render'
import { server } from '@/test/msw-server'
import { routes } from './routes'

it('resolves a session-list route through the real workspace tenant boundary', async () => {
  installCloudSpaceHandlers('member')
  const reads: string[] = []
  server.use(
    http.get('/api/v1/tenants/:tid/agent-sessions', ({ params }) => {
      reads.push(String(params['tid']))
      return HttpResponse.json({ items: [], nextCursor: '' })
    }),
    http.get('/api/v1/tenants/:tid/projects', () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
  )
  renderRoutes(routes, '/w/cloud-dev/sessions')
  expect(await screen.findByText('没有符合条件的会话。')).toBeInTheDocument()
  expect(reads).toEqual([TEST_TENANT_ID])
  expect(screen.getByRole('link', { name: '会话分析' })).toHaveAttribute(
    'href',
    '/w/cloud-dev/sessions',
  )
})
