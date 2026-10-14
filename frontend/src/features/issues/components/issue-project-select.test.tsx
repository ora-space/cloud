import { screen, waitFor } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, onTestFinished, vi } from 'vitest'
import { installCloudSpaceHandlers, TEST_SPACE_ID, TEST_TENANT_ID } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { IssueProjectSelect } from './issue-project-select'

const URL = `/api/v1/tenants/${TEST_TENANT_ID}/spaces/${TEST_SPACE_ID}/projects`

function renderStoredProject() {
  installCloudSpaceHandlers('admin')
  const changed = vi.fn<(value: string) => void>()
  renderWithProviders(<IssueProjectSelect value="stored-project" onChange={changed} />, {
    slug: 'cloud-dev',
  })
  return changed
}

describe('stored project references', () => {
  it('keeps a stored reference visible and disables selection while its real list loads', async () => {
    let release: (() => void) | undefined
    let reading = false
    const ready = new Promise<void>((resolve) => {
      release = resolve
    })
    onTestFinished(() => release?.())
    server.use(
      http.get(URL, async () => {
        reading = true
        await ready
        return HttpResponse.json({ items: [], nextCursor: '' })
      }),
    )
    const changed = renderStoredProject()
    await waitFor(() => expect(reading).toBe(true))
    const choice = screen.getByRole('combobox', { name: '项目' })
    expect(choice).toHaveTextContent('已关联项目（加载中）')
    expect(choice).toBeDisabled()
    release?.()
    await waitFor(() => expect(choice).toHaveTextContent('stored-project'))
    expect(choice).not.toHaveTextContent('未关联项目')
    expect(changed).not.toHaveBeenCalled()
  })

  it('does not turn a failed project list into an unassociated task', async () => {
    server.use(
      http.get(URL, () =>
        HttpResponse.json({ code: 'unavailable', params: {}, requestId: 'r' }, { status: 503 }),
      ),
    )
    const changed = renderStoredProject()
    expect(await screen.findByRole('alert')).toHaveTextContent('关联状态已保留')
    expect(screen.getByRole('combobox', { name: '项目' })).toHaveTextContent(
      '已关联项目（列表加载失败）',
    )
    expect(screen.getByRole('combobox', { name: '项目' })).toBeDisabled()
    expect(changed).not.toHaveBeenCalled()
  })
})
