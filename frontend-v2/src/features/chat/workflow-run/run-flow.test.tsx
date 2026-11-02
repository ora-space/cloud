import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { TooltipProvider } from '@/components/ui/tooltip'
import Page from '../page'
import { resetChatStore } from '../store'
import { chatBackend, FakeEventSource, line, tenantId } from '../test-backend'
import { sampleWorkflowBackend, serveWorkflows, type WorkflowServer } from '@/lib/workflows/test/workflow-server'
import { runRefreshMs } from './server-runs'
import { serveRuns, type RunServer } from './test-run-server'

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
  let workflows: WorkflowServer
  let runs: RunServer
  beforeEach(async () => {
    window.localStorage.clear()
    resetChatStore()
    chatBackend()
    workflows = serveWorkflows(tenantId, await sampleWorkflowBackend())
    runs = serveRuns(tenantId, workflows.backend)
    runRefreshMs.interval = 20
  })
  afterEach(() => {
    runRefreshMs.interval = 1_000
  })

  it('runs a published workflow on v2-server and follows it on the Theater', async () => {
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '审查 cloud#210')

    // The run opens pending on its Overview and is listed with the project's sessions.
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    // The run view is loaded on first use, which takes a while the first time in jsdom.
    expect(await within(pane).findByRole('heading', { level: 2, name: '审查 cloud#210' }, { timeout: 10_000 })).toBeInTheDocument()
    expect(within(pane).queryByText('演示运行')).not.toBeInTheDocument()
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

    // The kickoff and the run are what v2-server was sent.
    const [created] = [...runs.runs.values()]
    expect(created!.detail.run.input).not.toBeNull()
    expect(runs.seen.filter((r) => /\/(input|start)$/.test(r))).toEqual([
      `PATCH /api/v2/tenants/${tenantId}/workflow-runs/${created!.detail.run.id}/input`,
      `POST /api/v2/tenants/${tenantId}/workflow-runs/${created!.detail.run.id}/start`,
    ])

    // An Agent's act shows the answer v2-server recorded, drawn as a chat thread.
    await user.click(within(pane).getByRole('button', { name: /Agent：审查改动/ }))
    await user.click(await within(pane).findByRole('button', { name: '查看节点会话' }))
    expect(within(pane).getByText('请完成：Agent：审查改动', { selector: '[data-slot="bubble-content"]' })).toBeInTheDocument()
    expect(within(pane).getAllByText('回答：Agent：审查改动').length).toBeGreaterThan(0)

    // Its details say what it actually ran on, and that v2 did not retry it as desktop would.
    await user.click(within(pane).getByRole('button', { name: '打开节点详情' }))
    const details = await within(pane).findByLabelText('阶段详情')
    expect(within(details).getByLabelText('实际执行')).toHaveTextContent('Claude Code · Fixture')
    expect(within(details).getByLabelText('实际执行')).toHaveTextContent('没有找到配置的模型「claude-sonnet-5-5」')
    expect(within(details).getByLabelText('v2 未应用的设置')).toHaveTextContent('v2 未应用：自动重试')
  }, 30_000)

  it('reads an executing run again whenever its events stream says it changed', async () => {
    runs.hold = true
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '跟随事件')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await within(pane).findByRole('heading', { level: 2, name: '跟随事件' }, { timeout: 10_000 })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))
    await waitFor(() => expect(within(project()).getByRole('button', { name: '工作流运行 跟随事件（运行中）' })).toBeInTheDocument())

    // From here only the stream can tell the page anything: polling is effectively off.
    const [entry] = [...runs.runs.values()]
    const stream = await waitFor(() => {
      const found = FakeEventSource.all.find((s) => s.url === `/api/v2/tenants/${tenantId}/workflow-runs/${entry!.detail.run.id}/events` && !s.closed)
      expect(found).toBeDefined()
      return found!
    })
    runRefreshMs.interval = 60_000
    await new Promise((resolve) => setTimeout(resolve, 50))
    const { run, nodes } = entry!.detail
    for (const node of nodes) {
      node.status = 'succeeded'
      node.output = '回答'
    }
    run.status = 'succeeded'
    run.output = '{}'
    run.finishedAt = Date.now()
    run.state = JSON.stringify({ current_nodes: [] })
    act(() => stream.fire('workflow_run_invalidated'))

    // The run page and the sidebar's list both read the run again; an ended run is not followed.
    expect(await within(pane).findByText('运行完成')).toBeInTheDocument()
    await waitFor(() => expect(within(project()).getByRole('button', { name: '工作流运行 跟随事件（成功）' })).toBeInTheDocument())
    await waitFor(() => expect(stream.closed).toBe(true))
  }, 30_000)

  it('shows an Agent node its whole session once the session can be read', async () => {
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '完整会话')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await within(pane).findByRole('heading', { level: 2, name: '完整会话' }, { timeout: 10_000 })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))
    expect(await within(pane).findByText('运行完成', {}, { timeout: 25_000 })).toBeInTheDocument()

    await user.click(within(pane).getByRole('button', { name: /Agent：审查改动/ }))
    await user.click(await within(pane).findByRole('button', { name: '查看节点会话' }))
    // Until the session stream sends anything, the recorded task and answer stand in.
    expect(within(pane).getAllByText('回答：Agent：审查改动').length).toBeGreaterThan(0)

    const [entry] = [...runs.runs.values()]
    const attempt = entry!.detail.nodes.find((n) => n.nodeType === 'agent' && n.nodeId === 'review')!
    const stream = FakeEventSource.all.find((s) => s.url.endsWith(`/workflow-runs/${entry!.detail.run.id}/nodes/${attempt.id}/events`))!
    act(() => {
      stream.emit({
        kind: 'history_snapshot',
        history: { records: [line.user(1, '完整的任务说明'), line.say(2, '读完了改动，结论是可以合并。'), line.ended(3)], incomplete: false },
      })
      stream.emit({ kind: 'turn_ended', prompt: '完整的任务说明', result: { status: 'completed', answer: '读完了改动，结论是可以合并。', answerComplete: true, stopReason: 'end_turn', failureCode: '' } })
      stream.emit({ kind: 'session_ended' })
      stream.fire('end')
    })
    expect(within(pane).getByText('读完了改动，结论是可以合并。')).toBeInTheDocument()
    expect(within(pane).queryByText('回答：Agent：审查改动', { selector: '[data-slot="bubble-content"], p' })).not.toBeInTheDocument()
    // The bridge ends a node's session after its turn; that is not shown as an interruption.
    expect(within(pane).queryByText(/中断/)).not.toBeInTheDocument()
    expect(stream.closed).toBe(true)
  }, 30_000)

  it('stops a run that is still executing', async () => {
    runs.hold = true
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '要停的运行')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await within(pane).findByRole('heading', { level: 2, name: '要停的运行' }, { timeout: 10_000 })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))
    await user.click(await within(pane).findByRole('button', { name: '停止' }))
    await waitFor(() => expect(within(project()).getByRole('button', { name: /工作流运行 要停的运行（已取消|已停止）/ })).toBeInTheDocument())
    const [run] = [...runs.runs.values()]
    expect(run!.detail.nodes.find((n) => n.nodeType === 'agent')!.status).toBe('cancelled')
  }, 30_000)

  it('searches the published workflows in the + menu', async () => {
    const user = userEvent.setup()
    await renderPage()
    await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
    await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
    const list = await screen.findByRole('group', { name: '已发布的工作流' })
    expect(await within(list).findByRole('menuitem', { name: /示例：逐段翻译/ })).toBeInTheDocument()

    fireEvent.change(screen.getByRole('textbox', { name: '搜索工作流' }), { target: { value: '审查' } })
    expect(within(list).getAllByRole('menuitem').map((item) => item.textContent)).toEqual(['示例：代码审查v1'])
    fireEvent.change(screen.getByRole('textbox', { name: '搜索工作流' }), { target: { value: '没有这个' } })
    expect(within(list).getByText('没有匹配的工作流')).toBeInTheDocument()
  })

  it('reads the library on every opening, shows no list when that fails, and retries', async () => {
    const user = userEvent.setup()
    await renderPage()
    const openMenu = async () => {
      await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
      await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
      return screen.findByRole('group', { name: '已发布的工作流' })
    }
    const listed = () => workflows.seen.filter((r) => r.endsWith(`/tenants/${tenantId}/workflows`)).length

    let list = await openMenu()
    await within(list).findByRole('menuitem', { name: /示例：代码审查/ })
    expect(listed()).toBe(1)
    // Closing the + menu closes the submenu with it.
    await user.keyboard('{Escape}')
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('menu')).not.toBeInTheDocument())

    workflows.refuse = { match: /GET .*\/workflows$/, status: 503, data: { code: 'workflow_store_unavailable', params: {}, requestId: 'r' } }
    list = await openMenu()
    const failure = await within(list).findByRole('alert')
    expect(failure).toHaveTextContent('读取工作流失败')
    expect(within(list).queryByRole('menuitem')).not.toBeInTheDocument()
    expect(listed()).toBe(2)

    workflows.refuse = undefined
    // A pointer leaving the submenu's trigger closes the submenu in jsdom, so the button is clicked in place.
    fireEvent.click(within(failure).getByRole('button', { name: '重试' }))
    expect(await within(list).findByRole('menuitem', { name: /示例：代码审查/ })).toBeInTheDocument()
    expect(within(list).queryByRole('alert')).not.toBeInTheDocument()
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
