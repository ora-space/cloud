import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { TooltipProvider } from '@/components/ui/tooltip'
import Page from '../page'
import { resetChatStore } from '../store'
import { chatBackend, tenantId } from '../test-backend'
import { sampleWorkflowBackend, serveWorkflows } from '@/lib/workflows/test/workflow-server'
import { demoRunClock, resetDemoRuns } from './demo-runs'

/** Renders the page and waits until it has loaded the backend's projects. */
const renderPage = async () => {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <TooltipProvider>
        <Page />
      </TooltipProvider>
    </QueryClientProvider>,
  )
  await within(sidebar()).findByRole('region', { name: '项目 ora-space/cloud' })
}
const sidebar = () => screen.getByRole('complementary', { name: '聊天记录' })
const project = () => within(sidebar()).getByRole('region', { name: '项目 ora-space/cloud' })

/** Creates a run of the review sample from the project's + menu, as a person would. */
async function createRun(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
  await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
  // A pointer leaving the submenu's trigger closes the submenu in jsdom, so the item is clicked in place.
  fireEvent.click(await screen.findByRole('menuitem', { name: /示例：代码审查/ }))
  const dialog = await screen.findByRole('dialog', { name: '运行工作流' })
  const field = within(dialog).getByLabelText('运行名称')
  await user.clear(field)
  await user.type(field, name)
  await user.click(within(dialog).getByRole('button', { name: '创建运行' }))
}

describe('workflow runs in chat', () => {
  beforeEach(async () => {
    window.localStorage.clear()
    resetChatStore()
    chatBackend()
    serveWorkflows(tenantId, await sampleWorkflowBackend())
    resetDemoRuns()
    demoRunClock.nodeStepMs = 5
  })
  afterEach(() => {
    resetDemoRuns()
    demoRunClock.nodeStepMs = 1500
  })

  it('runs a published workflow from a project and plays it on the Theater', async () => {
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '审查 cloud#210')

    // The run opens pending on its Overview and is listed with the project's sessions.
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    // The run view is loaded on first use, which takes a while the first time in jsdom.
    expect(await within(pane).findByRole('heading', { level: 2, name: '审查 cloud#210' }, { timeout: 10_000 })).toBeInTheDocument()
    expect(within(pane).getByText('演示运行')).toBeInTheDocument()
    expect(within(pane).getByRole('button', { name: '全图' })).toHaveAttribute('aria-pressed', 'true')
    expect(within(project()).getByRole('button', { name: '工作流运行 审查 cloud#210（未运行）' })).toHaveAttribute('aria-current', 'page')

    // Starting moves to the Theater, which ends on the result act.
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const start = await screen.findByRole('dialog')
    await user.click(within(start).getByRole('button', { name: '启动' }))
    expect(within(pane).getByRole('button', { name: '舞台' })).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(within(project()).getByRole('button', { name: '工作流运行 审查 cloud#210（成功）' })).toBeInTheDocument(), { timeout: 25_000 })
    expect(await within(pane).findByText('运行完成')).toBeInTheDocument()
    expect(within(pane).getByRole('button', { name: '再跑一次' })).toBeInTheDocument()

    // An Agent's act keeps what it was sent and what it answered, drawn as a chat thread.
    await user.click(within(pane).getByRole('button', { name: /Agent：审查改动/ }))
    await user.click(await within(pane).findByRole('button', { name: '查看节点会话' }))
    expect(within(pane).getByText(/分支相对 main 的改动。/, { selector: '[data-slot="bubble-content"]' })).toBeInTheDocument()
    expect(within(pane).getByRole('heading', { name: 'Mock 结论' })).toBeInTheDocument()
    // jsdom re-renders the canvas on every node step, so the whole run takes seconds here.
  }, 30_000)

  it('searches the published workflows in the + menu', async () => {
    const user = userEvent.setup()
    await renderPage()
    await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
    await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
    const list = await screen.findByRole('group', { name: '已发布的工作流' })
    expect(within(list).getByRole('menuitem', { name: /示例：逐段翻译/ })).toBeInTheDocument()

    fireEvent.change(screen.getByRole('textbox', { name: '搜索工作流' }), { target: { value: '审查' } })
    expect(within(list).getAllByRole('menuitem').map((item) => item.textContent)).toEqual(['示例：代码审查v1'])
    fireEvent.change(screen.getByRole('textbox', { name: '搜索工作流' }), { target: { value: '没有这个' } })
    expect(within(list).getByText('没有匹配的工作流')).toBeInTheDocument()
  })

  it('goes back to the chat when a session is picked', async () => {
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '临时运行')
    await within(await screen.findByRole('region', { name: '工作流运行' })).findByRole('heading', { level: 2, name: '临时运行' }, { timeout: 10_000 })

    await user.click(within(project()).getByRole('button', { name: '梳理 v2 接口缺口' }))
    expect(screen.queryByRole('region', { name: '工作流运行' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { level: 2, name: '梳理 v2 接口缺口' })).toBeInTheDocument()
    expect(within(project()).getByRole('button', { name: '工作流运行 临时运行（未运行）' })).not.toHaveAttribute('aria-current')
  })
})
