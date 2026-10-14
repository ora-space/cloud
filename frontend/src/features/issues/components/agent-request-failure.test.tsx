import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { postApiV1TenantsTidIssuesIidComments } from '@/api/tenants/tenants'
import { renderAtRoute } from '@/test/render'
import { server } from '@/test/msw-server'
import { PendingTargets } from './pending-targets'

async function submitToServer() {
  await postApiV1TenantsTidIssuesIidComments('t1', 'i1', { body: 'Task', targets: [] })
}

function renderTask(onSubmit = submitToServer) {
  return renderAtRoute(
    '/w/:workspaceSlug/issues/:issueId',
    <PendingTargets
      slug="t1"
      issueId="i1"
      targets={[{ type: 'agent', id: 'a1' }]}
      catalog={[]}
      onChange={() => undefined}
      onSubmit={onSubmit}
    />,
    '/w/example/issues/i1',
  )
}

describe('model-backed task refusal', () => {
  it.each([
    { code: 'issue_project_required', message: '请关联可用项目', projectHref: '#issue-project' },
    { code: 'target_not_found', message: '协作对象已不可用', projectHref: null },
  ])('keeps the task draft and explains $code safely', async ({ code, message, projectHref }) => {
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', () =>
        HttpResponse.json({ code, params: {}, requestId: 'r' }, { status: 409 }),
      ),
    )
    const user = userEvent.setup()
    renderTask()
    await user.type(screen.getByLabelText('a1 的留言'), 'Keep this task')
    await user.click(screen.getByRole('button', { name: '提交' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(message)
    expect(screen.getByLabelText('a1 的留言')).toHaveValue('Keep this task')
    expect(screen.queryByRole('link', { name: '选择关联项目' })?.getAttribute('href') ?? null).toBe(
      projectHref,
    )
  })
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
    renderTask()
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
    renderTask(() => Promise.reject(new Error('Untrusted upstream details')))
    await user.type(screen.getByLabelText('a1 的留言'), 'Task')
    await user.click(screen.getByRole('button', { name: '提交' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('提交失败，请稍后重试。')
    expect(screen.queryByText('Untrusted upstream details')).not.toBeInTheDocument()
  })
})
