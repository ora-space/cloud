import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import { ScheduledRunArtifacts } from './scheduled-run-artifacts'

const LIST = '/api/v1/tenants/tid1/issues/i1/runs/r1/artifacts'
const ARTIFACT = {
  index: 3,
  name: 'reports/result.html',
  kind: 'html',
  sizeBytes: 42,
  sha256: 'a'.repeat(64),
}

function renderArtifacts() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const view = render(
    <QueryClientProvider client={client}>
      <ScheduledRunArtifacts tid="tid1" issueId="i1" runId="r1" />
    </QueryClientProvider>,
  )
  return { ...view, client, user: userEvent.setup() }
}

async function openArtifacts() {
  server.use(http.get(LIST, () => HttpResponse.json({ artifacts: [ARTIFACT] })))
  const context = renderArtifacts()
  await context.user.click(screen.getByRole('button', { name: '查看产物' }))
  await screen.findByRole('link', { name: '下载 reports/result.html' })
  return context
}

describe('ScheduledRunArtifacts', () => {
  it('loads metadata on expansion and offers only same-origin attachment downloads', async () => {
    let downloads = 0
    server.use(
      http.get(LIST + '/3', () => {
        downloads++
        return HttpResponse.text('private bytes')
      }),
    )
    const { user, client } = await openArtifacts()
    const link = screen.getByRole('link', { name: '下载 reports/result.html' })
    expect(link).toHaveAttribute('href', LIST + '/3')
    expect(link).toHaveAttribute('download')
    expect(link).toHaveAttribute('referrerpolicy', 'no-referrer')
    expect(link).not.toHaveAttribute('target')
    expect(downloads).toBe(0)
    expect(screen.queryByText(/链接有效至/)).not.toBeInTheDocument()
    expect(JSON.stringify(client.getQueryData([LIST]))).not.toContain('private bytes')
    await user.click(screen.getByRole('button', { name: '收起产物' }))
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '查看产物' }))
    expect(await screen.findByRole('link')).toHaveAttribute('href', LIST + '/3')
    expect(downloads).toBe(0)
  })

  it('shows an empty current index even when history previously counted files', async () => {
    server.use(http.get(LIST, () => HttpResponse.json({ artifacts: [] })))
    const { user } = renderArtifacts()
    await user.click(screen.getByRole('button', { name: '查看产物' }))
    expect(await screen.findByText(/暂无可下载产物/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '获取下载链接' })).not.toBeInTheDocument()
  })

  it('allows retrying a failed metadata read without downloading the file', async () => {
    let attempts = 0
    server.use(
      http.get(LIST, () => {
        attempts++
        return attempts === 1
          ? HttpResponse.json({ code: 'unavailable' }, { status: 503 })
          : HttpResponse.json({ artifacts: [ARTIFACT] })
      }),
    )
    const { user } = renderArtifacts()
    await user.click(screen.getByRole('button', { name: '查看产物' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('产物列表加载失败')
    await user.click(screen.getByRole('button', { name: '重试' }))
    expect(
      await screen.findByRole('link', { name: '下载 reports/result.html' }),
    ).toBeInTheDocument()
    expect(attempts).toBe(2)
    expect(screen.getByRole('link')).toHaveAttribute('href', LIST + '/3')
  })
})
