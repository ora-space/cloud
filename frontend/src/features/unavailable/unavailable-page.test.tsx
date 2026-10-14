import { screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { UnavailablePage } from './unavailable-page'

describe('UnavailablePage', () => {
  it('explains availability without issuing demo requests or accepting work', () => {
    const requests: string[] = []
    server.use(
      http.all('*/workspaces/:slug/*', ({ request }) => {
        requests.push(request.url)
        return HttpResponse.json([])
      }),
    )
    renderWithProviders(
      <UnavailablePage
        title="示例入口"
        description="请使用已经开放的任务入口。"
        actions={[{ label: '打开任务', to: '/w/real-workspace/issues' }]}
      />,
    )
    expect(screen.getByRole('heading', { name: '示例入口暂不可用' })).toBeInTheDocument()
    expect(screen.queryByRole('textbox')).not.toBeInTheDocument()
    expect(requests).toEqual([])
  })
})
