import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { expect, it } from 'vitest'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import { PullRequestRefs } from './pull-request-refs'

const endpoint = '/api/v1/tenants/t1/issues/i1/context-refs'

it('links only safe external PR references and keeps opaque or unsafe references as text', async () => {
  server.use(
    http.get(endpoint, () =>
      HttpResponse.json({
        items: [
          { id: 'a', refType: 'pull_request', refId: 'https://github.com/example/repo/pull/1' },
          { id: 'b', refType: 'pull_request', refId: 'javascript:alert(1)' },
          { id: 'c', refType: 'pull_request', refId: 'cloud#160' },
          { id: 'd', refType: 'project', refId: 'private-project' },
        ],
        nextCursor: '',
      }),
    ),
  )
  renderWithProviders(<PullRequestRefs tid="t1" issueId="i1" />)
  expect(await screen.findByRole('link')).toHaveAttribute(
    'href',
    'https://github.com/example/repo/pull/1',
  )
  expect(screen.getAllByRole('link')).toHaveLength(1)
  expect(screen.getByText('javascript:alert(1)')).toBeInTheDocument()
  expect(screen.getByText('cloud#160')).toBeInTheDocument()
  expect(screen.queryByText('private-project')).not.toBeInTheDocument()
})

it('adds a PR only on explicit submission and preserves the draft on failure', async () => {
  const sent: unknown[] = []
  server.use(
    http.get(endpoint, () => HttpResponse.json({ items: [], nextCursor: '' })),
    http.post(endpoint, async ({ request }) => {
      sent.push(await request.json())
      return HttpResponse.json({ code: 'unavailable' }, { status: 503 })
    }),
  )
  const user = userEvent.setup()
  renderWithProviders(<PullRequestRefs tid="t1" issueId="i1" />)
  await user.click(screen.getByRole('button', { name: '添加关联 PR' }))
  await user.type(screen.getByLabelText('PR 链接或引用 ID'), 'cloud#160')
  expect(sent).toEqual([])
  await user.click(screen.getByRole('button', { name: '保存关联' }))
  await waitFor(() => expect(sent).toEqual([{ refType: 'pull_request', refId: 'cloud#160' }]))
  expect(await screen.findByRole('alert')).toHaveTextContent('保存 PR 引用失败')
  expect(screen.getByLabelText('PR 链接或引用 ID')).toHaveValue('cloud#160')
})
