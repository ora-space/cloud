import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { beforeEach, describe, expect, it } from 'vitest'
import { TooltipProvider } from '@/components/ui/tooltip'
import Page from './page'
import { resetChatStore, useChatStore } from './store'
import { type ChatBackend, chatBackend, FakeEventSource, line, tenantId } from './test-backend'

const renderPage = (url = '/chat') =>
  render(
    <MemoryRouter initialEntries={[url]}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <TooltipProvider>
          <Page />
        </TooltipProvider>
      </QueryClientProvider>
    </MemoryRouter>,
  )
const sidebar = () => screen.getByRole('complementary', { name: '聊天记录' })
/** Renders the page and waits until it has loaded the backend's projects. */
const renderReady = async () => {
  renderPage()
  await within(sidebar()).findByRole('region', { name: '项目 ora-space/cloud' })
}

describe('chat page', () => {
  let backend: ChatBackend
  beforeEach(() => {
    resetChatStore()
    backend = chatBackend()
  })

  it('groups history by project', async () => {
    await renderReady()
    expect(screen.getByRole('heading', { level: 1, name: '对话' })).toBeInTheDocument()
    const cloud = within(sidebar()).getByRole('region', { name: '项目 ora-space/cloud' })
    expect(within(cloud).getByText('梳理 v2 接口缺口')).toBeInTheDocument()
    expect(within(cloud).getByText('对话页样式')).toBeInTheDocument()
  })

  it('opens the run a link names', async () => {
    renderPage('/chat?run=run-9')
    expect(await screen.findByRole('region', { name: '工作流运行' })).toBeInTheDocument()
    expect(useChatStore.getState().selectedRunId).toBe('run-9')
  })

  it('opens on a new chat in the first project, as desktop lands', async () => {
    await renderReady()
    expect(screen.getByRole('heading', { level: 2, name: '新对话' })).toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 2, name: '今天想构建什么？' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '选择项目' })).toHaveTextContent('ora-space/cloud')
    // Every conversation runs in a sandbox of its own: there is nowhere else to choose.
    expect(screen.queryByRole('button', { name: '选择工作目录' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '选择启动模式' })).toHaveTextContent('云端')
  })

  it('puts the cursor in the input when a project\'s + menu opens the chat it already shows', async () => {
    await renderReady()
    const input = screen.getByRole('textbox', { name: '消息' })
    expect(input).not.toHaveFocus()
    await userEvent.click(within(sidebar()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: '新建对话' }))
    expect(input).toHaveFocus()
  })

  it('lists only the backend\'s projects and conversations, with no samples or replay', async () => {
    await renderReady()
    expect(within(sidebar()).queryByRole('region', { name: '项目 示例' })).not.toBeInTheDocument()
    await userEvent.click(within(sidebar()).getByRole('button', { name: '梳理 v2 接口缺口' }))
    expect(screen.queryByRole('button', { name: '重新播放' })).not.toBeInTheDocument()
    expect(screen.queryByText(/^样例：/)).not.toBeInTheDocument()
  })

  it('starts a conversation in a project with a text prompt, and shows the Agent\'s reply', async () => {
    await renderReady()
    await userEvent.click(within(sidebar()).getByRole('button', { name: '在 ora-space/desktop 新建对话' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: '新建对话' }))
    expect(screen.getByRole('button', { name: '选择项目' })).toHaveTextContent('ora-space/desktop')
    // Even when the new chat already shows, 新建对话 in the + menu puts the cursor in its input.
    expect(screen.getByRole('textbox', { name: '消息' })).toHaveFocus()
    // The first version sends text only: no files or plugins.
    expect(screen.queryByRole('button', { name: '添加' })).not.toBeInTheDocument()
    const input = screen.getByRole('textbox', { name: '消息' })
    await userEvent.type(input, '检查一下 @代码{Enter}')
    expect(screen.queryByRole('option', { name: '代码审查' })).not.toBeInTheDocument()
    expect(await screen.findByRole('heading', { level: 2, name: '检查一下 @代码' })).toBeInTheDocument()
    expect(backend.bodies.at(-1)).toEqual({ projectId: 'p-desktop', executorId: 'oc-fixture', prompt: '检查一下 @代码' })
    act(() =>
      FakeEventSource.of('c-new-1').emit({
        kind: 'history_snapshot',
        history: { records: [line.user(1, '检查一下 @代码'), line.say(2, '检查完了，没有问题。'), line.ended(3)], incomplete: false },
      }),
    )
    expect(screen.getByText('检查完了，没有问题。')).toBeInTheDocument()
    expect(within(sidebar()).getByRole('region', { name: '项目 ora-space/desktop' })).toHaveTextContent('检查一下 @代码')
    expect(input).toHaveValue('')
  })

  it('makes a stopped conversation read-only and offers a new chat in its project', async () => {
    await renderReady()
    await userEvent.click(within(sidebar()).getByRole('button', { name: '梳理 v2 接口缺口' }))
    const stream = FakeEventSource.of('c-main')
    act(() => stream.emit({ kind: 'history_snapshot', history: { records: [line.user(1, '写长一点')], incomplete: false } }))
    await userEvent.click(screen.getByRole('button', { name: '停止' }))
    act(() => {
      stream.emit({ kind: 'turn_ended', result: { status: 'cancelled', answer: '', answerComplete: false, stopReason: 'cancelled', failureCode: '' } })
      stream.emit({ kind: 'session_ended' })
    })
    expect(screen.getByText(/已停止 · 这个对话已结束/)).toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: '消息' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '新对话' }))
    expect(screen.getByRole('heading', { level: 2, name: '今天想构建什么？' })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '选择项目' })).toHaveTextContent('ora-space/cloud')
    expect(screen.getByRole('textbox', { name: '消息' })).toBeInTheDocument()
  })

  it('shows where a new chat runs; the cloud is the only mode on the web', async () => {
    await renderReady()
    await userEvent.click(screen.getByRole('button', { name: '选择启动模式' }))
    expect(await screen.findByRole('menuitem', { name: /本地/ })).toHaveAttribute('aria-disabled', 'true')
  })

  it('searches the backend\'s projects', async () => {
    await renderReady()
    await userEvent.click(screen.getByRole('button', { name: '选择项目' }))
    await userEvent.type(await screen.findByPlaceholderText('搜索项目'), 'desk')
    expect(screen.getByRole('option', { name: 'ora-space/desktop' })).toBeInTheDocument()
    expect(screen.queryByRole('option', { name: 'ora-space/cloud' })).not.toBeInTheDocument()
  })

  /** Opens the new project dialog from the project menu and fills in a repository. */
  const fillNewProject = async (url: string, branch?: string) => {
    await userEvent.click(screen.getByRole('button', { name: '选择项目' }))
    await userEvent.click(await screen.findByRole('option', { name: /新建项目/ }))
    const dialog = await screen.findByRole('dialog', { name: '新建项目' })
    await userEvent.type(within(dialog).getByLabelText('仓库地址'), url)
    if (branch !== undefined) {
      await userEvent.clear(within(dialog).getByLabelText('默认分支'))
      await userEvent.type(within(dialog).getByLabelText('默认分支'), branch)
    }
    return dialog
  }

  it('creates a project from a repository and starts the new chat in it', async () => {
    await renderReady()
    const dialog = await fillNewProject('https://github.com/octocat/Hello-World.git', 'master')
    expect(within(dialog).getByLabelText('项目名')).toHaveValue('Hello-World')
    await userEvent.click(within(dialog).getByRole('button', { name: '新建项目' }))
    expect(await within(sidebar()).findByRole('region', { name: '项目 Hello-World' })).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '选择项目' })).toHaveTextContent('Hello-World')
    expect(backend.seen).toContain(`POST /api/v2/tenants/${tenantId}/projects`)
    expect(backend.bodies).toContainEqual({ name: 'Hello-World', repositoryUrl: 'https://github.com/octocat/Hello-World.git', defaultBranch: 'master' })

    // The new chat's first prompt goes to the new project.
    await userEvent.type(screen.getByRole('textbox', { name: '消息' }), '读一下 README{Enter}')
    await within(sidebar()).findByText('读一下 README')
    expect(backend.bodies).toContainEqual({ projectId: 'p-new-1', executorId: 'oc-fixture', prompt: '读一下 README' })
  })

  it.each([
    ['invalid_project', 400, /仓库地址要用 HTTPS/],
    ['repository_unavailable', 400, /无法克隆这个仓库，或仓库里没有分支 nope/],
    ['project_conflict', 409, /和已有项目冲突/],
  ])('says why a project was not created (%s), keeping the dialog and what was typed', async (code, status, message) => {
    await renderReady()
    backend.refuseProject = { status, data: { code, params: {}, requestId: 'r' } }
    const dialog = await fillNewProject('https://github.com/octocat/missing', 'nope')
    await userEvent.click(within(dialog).getByRole('button', { name: '新建项目' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(message)
    expect(within(dialog).getByLabelText('仓库地址')).toHaveValue('https://github.com/octocat/missing')
    expect(within(dialog).getByLabelText('默认分支')).toHaveValue('nope')
    expect(within(sidebar()).queryByRole('region', { name: '项目 missing' })).not.toBeInTheDocument()
    expect(backend.created).toEqual([])
  })

  it('picks an agent, then one of its models, from the backend\'s executors', async () => {
    await renderReady()
    await userEvent.click(screen.getByRole('button', { name: /^模型：/ }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'OpenCode' }))
    await userEvent.type(screen.getByRole('textbox', { name: '搜索模型' }), 'sonnet')
    expect(screen.queryByRole('menuitem', { name: 'Fixture' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('menuitem', { name: 'Claude Sonnet 5.5' }))
    expect(screen.getByRole('button', { name: /^模型：/ })).toHaveAccessibleName('模型：OpenCode Claude Sonnet 5.5')
  })

  it('says when the backend cannot run Agents yet', async () => {
    resetChatStore()
    chatBackend({ unavailable: true })
    renderPage()
    expect(await screen.findByText(/后端还不能运行 Agent/)).toHaveRole('status')
  })

  it('labels every chat as running in a sandbox of its own, explained on hover', async () => {
    await renderReady()
    const label = screen.getByText('沙盒')
    await userEvent.hover(label)
    expect(await screen.findByText('每个对话在独立的沙盒中运行，代码从项目仓库 clone 而来，改动不会写回')).toBeInTheDocument()
    await userEvent.unhover(label)
    await userEvent.click(within(sidebar()).getByRole('button', { name: '梳理 v2 接口缺口' }))
    expect(within(screen.getByRole('banner')).getByText('沙盒')).toBeInTheDocument()
  })

  it('renames a conversation by double-clicking it', async () => {
    await renderReady()
    await userEvent.dblClick(within(sidebar()).getByRole('button', { name: '梳理 v2 接口缺口' }))
    const field = screen.getByRole('textbox', { name: '会话名称' })
    await userEvent.clear(field)
    await userEvent.type(field, '接口清单{Enter}')
    expect(within(sidebar()).getByRole('button', { name: '接口清单' })).toBeInTheDocument()
    expect(backend.bodies.at(-1)).toEqual({ title: '接口清单' })
  })

  it('folds the history column away and brings it back', async () => {
    renderPage()
    await userEvent.click(within(sidebar()).getByRole('button', { name: '收起对话列表' }))
    expect(screen.queryByRole('complementary', { name: '聊天记录' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: '展开对话列表' }))
    expect(sidebar()).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '展开对话列表' })).not.toBeInTheDocument()
  })

  it('resizes the history column from its edge, folding it past the narrowest width', async () => {
    renderPage()
    const edge = screen.getByRole('separator', { name: '调整对话列表宽度' })
    edge.focus()
    await userEvent.keyboard('{ArrowRight}')
    expect(sidebar()).toHaveStyle({ width: '304px' })
    expect(edge).toHaveAttribute('aria-valuenow', '304')
    act(() => useChatStore.getState().setSidebar({ width: 200 }))
    await userEvent.keyboard('{ArrowLeft}')
    expect(screen.queryByRole('complementary', { name: '聊天记录' })).not.toBeInTheDocument()
    expect(edge).toHaveAttribute('aria-valuenow', '0')
    await userEvent.keyboard('{Enter}')
    expect(sidebar()).toHaveStyle({ width: '200px' })
  })

  it('reorders projects when one is dropped on another', async () => {
    await renderReady()
    const order = () => within(sidebar()).getAllByRole('region').map((r) => r.getAttribute('aria-label'))
    expect(order()).toEqual(['项目 ora-space/cloud', '项目 ora-space/desktop'])
    // dnd-kit's keyboard sensor needs layout jsdom lacks, so drive the store the drop calls.
    act(() => useChatStore.getState().moveProject('p-desktop', 'p-cloud'))
    expect(order()).toEqual(['项目 ora-space/desktop', '项目 ora-space/cloud'])
  })
})
