import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { GitIdentity } from '@/api/generated.schemas'
import { server } from '@/test/msw-server'
import { GitIdentityPage } from './git-identity-page'

const URL = '/api/v1/me/git-identity'

const defaultIdentity: GitIdentity = {
  name: 'octocat',
  email: '1+octocat@users.noreply.github.com',
  isDefault: true,
  version: 0,
}

const ownIdentity: GitIdentity = {
  name: 'Mona Lisa',
  email: 'mona@example.com',
  isDefault: false,
  version: 3,
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <GitIdentityPage />
    </QueryClientProvider>,
  )
}

function fault(code: string, status: number) {
  return HttpResponse.json({ code, params: {}, requestId: 'r' }, { status })
}

describe('GitIdentityPage', () => {
  it('prefills the default identity, marks it default and cannot restore it', async () => {
    server.use(http.get(URL, () => HttpResponse.json(defaultIdentity)))
    renderPage()

    expect(await screen.findByLabelText('名称')).toHaveValue('octocat')
    expect(screen.getByLabelText('邮箱')).toHaveValue('1+octocat@users.noreply.github.com')
    expect(screen.getByText('默认身份')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复默认' })).toBeDisabled()
    expect(screen.getByText(/之后启动的会话/)).toBeInTheDocument()
  })

  it('saves with the loaded version and shows the identity the server returned', async () => {
    const puts: unknown[] = []
    server.use(
      http.get(URL, () => HttpResponse.json(defaultIdentity)),
      http.put(URL, async ({ request }) => {
        puts.push(await request.json())
        return HttpResponse.json({ ...ownIdentity, version: 1 })
      }),
    )
    const user = userEvent.setup()
    renderPage()

    const name = await screen.findByLabelText('名称')
    await user.clear(name)
    await user.type(name, 'Mona Lisa')
    await user.clear(screen.getByLabelText('邮箱'))
    await user.type(screen.getByLabelText('邮箱'), 'mona@example.com')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByText('已保存')).toBeInTheDocument()
    expect(puts).toEqual([{ name: 'Mona Lisa', email: 'mona@example.com', version: 0 }])
    expect(screen.queryByText('默认身份')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '恢复默认' })).toBeEnabled()
    expect(screen.getByLabelText('名称')).toHaveValue('Mona Lisa')
  })

  it.each([
    ['version_conflict', 409, '身份已在别处修改，请刷新后重试'],
    ['invalid_git_name', 400, '名称需为 1–200 个字符，不能包含换行或尖括号'],
    ['invalid_git_email', 400, '邮箱格式不正确'],
    ['internal', 500, '保存失败，请稍后重试'],
  ])('explains a %s refusal and keeps the draft', async (code, status, message) => {
    server.use(
      http.get(URL, () => HttpResponse.json(ownIdentity)),
      http.put(URL, () => fault(code, status)),
    )
    const user = userEvent.setup()
    renderPage()

    await user.type(await screen.findByLabelText('名称'), ' II')
    await user.click(screen.getByRole('button', { name: '保存' }))

    expect(await screen.findByRole('alert')).toHaveTextContent(message)
    expect(screen.getByLabelText('名称')).toHaveValue('Mona Lisa II')
  })

  it('restores the default with the current version and an idempotency key', async () => {
    const deletes: { body: unknown; key: string | null }[] = []
    server.use(
      http.get(URL, () => HttpResponse.json(ownIdentity)),
      http.delete(URL, async ({ request }) => {
        deletes.push({ body: await request.json(), key: request.headers.get('Idempotency-Key') })
        return HttpResponse.json(defaultIdentity)
      }),
    )
    const user = userEvent.setup()
    renderPage()

    expect(await screen.findByLabelText('名称')).toHaveValue('Mona Lisa')
    expect(screen.queryByText('默认身份')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '恢复默认' }))

    expect(await screen.findByText('默认身份')).toBeInTheDocument()
    await waitFor(() => expect(screen.getByLabelText('名称')).toHaveValue('octocat'))
    expect(deletes).toEqual([{ body: { version: 3 }, key: expect.any(String) }])
    expect(screen.getByRole('button', { name: '恢复默认' })).toBeDisabled()
  })

  it('reports a failed load', async () => {
    server.use(http.get(URL, () => fault('internal', 500)))
    renderPage()

    expect(await screen.findByText('Git 身份加载失败')).toBeInTheDocument()
  })
})
