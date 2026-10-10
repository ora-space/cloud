import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { postApiV1TenantsTidIssuesIidComments } from '@/api/tenants/tenants'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'
import { PendingTargets } from './pending-targets'

describe('model-backed task refusal', () => {
  it('preserves the staged task and links missing defaults to personal model settings', async () => {
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', () =>
        HttpResponse.json(
          { code: 'model_default_required', params: {}, requestId: 'r' },
          { status: 409 },
        ),
      ),
    )
    const user = userEvent.setup()
    renderAtRoute(
      '/w/:workspaceSlug/issues/:issueId',
      <PendingTargets
        slug="t1"
        issueId="i1"
        targets={[{ type: 'agent', id: 'a1' }]}
        catalog={[]}
        onChange={() => undefined}
        onSubmit={async () => {
          await postApiV1TenantsTidIssuesIidComments('t1', 'i1', { body: 'Task', targets: [] })
        }}
      />,
      '/w/example/issues/i1',
    )
    await user.type(screen.getByLabelText('a1 的留言'), 'Read the repository')
    await user.click(screen.getByRole('button', { name: '提交' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('请先配置可用的模型连接')
    expect(screen.getByRole('link', { name: '打开模型连接设置' })).toHaveAttribute(
      'href',
      '/w/example/settings/model-connections',
    )
    expect(screen.getByLabelText('a1 的留言')).toHaveValue('Read the repository')
    expect(screen.getByRole('button', { name: '提交' })).toBeEnabled()
  })

  it('turns unrelated task errors into bounded generic messages', async () => {
    server.use(
      http.get('/api/v1/me', () => HttpResponse.json({ code: 'unauthenticated' }, { status: 401 })),
    )
    const user = userEvent.setup()
    renderAtRoute(
      '/w/:workspaceSlug/issues/:issueId',
      <PendingTargets
        slug="t1"
        issueId="i1"
        targets={[{ type: 'agent', id: 'a1' }]}
        catalog={[]}
        onChange={() => undefined}
        onSubmit={() => Promise.reject(new Error('Untrusted upstream details'))}
      />,
      '/w/example/issues/i1',
    )
    await user.type(screen.getByLabelText('a1 的留言'), 'Task')
    await user.click(screen.getByRole('button', { name: '提交' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('提交失败，请稍后重试。')
    expect(screen.queryByText('Untrusted upstream details')).not.toBeInTheDocument()
  })
})
