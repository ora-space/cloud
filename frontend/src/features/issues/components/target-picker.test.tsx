import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, onTestFinished } from 'vitest'
import { makeAgentTarget } from '@/test/issue-fixtures'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { TargetPicker } from './target-picker'

const URL = '/api/v1/tenants/t1/collaboration/targets'

function renderPicker() {
  return renderWithProviders(<TargetPicker slug="t1" targets={[]} onChange={() => undefined} />)
}

describe('real collaboration directory feedback', () => {
  it('shows loading until the directory responds instead of claiming it is empty', async () => {
    let release: (() => void) | undefined
    const ready = new Promise<void>((resolve) => {
      release = resolve
    })
    onTestFinished(() => release?.())
    server.use(
      http.get(URL, async () => {
        await ready
        return HttpResponse.json({ items: [makeAgentTarget('a1')], nextCursor: '' })
      }),
    )
    const user = userEvent.setup()
    renderPicker()
    await user.click(screen.getByRole('button', { name: '提及 / 任务' }))
    expect(await screen.findByRole('status')).toHaveTextContent('正在加载')
    expect(screen.queryByText('暂无协作对象。')).not.toBeInTheDocument()
    release?.()
    expect(await screen.findByRole('button', { name: /Installed Agent/ })).toBeEnabled()
  })

  it('distinguishes an empty directory from a failed request', async () => {
    server.use(http.get(URL, () => HttpResponse.json({ items: [], nextCursor: '' })))
    const user = userEvent.setup()
    renderPicker()
    await user.click(screen.getByRole('button', { name: '提及 / 任务' }))
    expect(await screen.findByText('暂无协作对象。')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('keeps a refusal visible and retries the same real directory', async () => {
    let reads = 0
    server.use(
      http.get(URL, () => {
        reads += 1
        if (reads === 1)
          return HttpResponse.json(
            { code: 'forbidden', requestId: 'r', params: {} },
            { status: 403 },
          )
        return HttpResponse.json({ items: [makeAgentTarget('a1')], nextCursor: '' })
      }),
    )
    const user = userEvent.setup()
    renderPicker()
    await user.click(screen.getByRole('button', { name: '提及 / 任务' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('协作对象加载失败')
    expect(screen.queryByText('暂无协作对象。')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '重试加载' }))
    expect(await screen.findByRole('button', { name: /Installed Agent/ })).toBeEnabled()
    await waitFor(() => expect(reads).toBe(2))
  })

  it('reports a search miss without offering a demo substitute', async () => {
    server.use(
      http.get(URL, () => HttpResponse.json({ items: [makeAgentTarget('a1')], nextCursor: '' })),
    )
    const user = userEvent.setup()
    renderPicker()
    await user.click(screen.getByRole('button', { name: '提及 / 任务' }))
    await screen.findByRole('button', { name: /Installed Agent/ })
    await user.type(screen.getByPlaceholderText('搜索协作对象…'), 'Missing Agent')
    expect(screen.getByText('没有匹配的协作对象。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Installed Agent/ })).not.toBeInTheDocument()
  })
})
