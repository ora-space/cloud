import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { UserContext } from '@/app/auth/user'
import { TooltipProvider } from '@/components/ui/tooltip'
import { demoUser } from '@/test/backend'
import Page from '../page'
import { newChatKey, resetChatStore, useChatStore } from '../store'
import { chatBackend, FakeEventSource, line, tenantId } from '../test-backend'
import { sampleWorkflowBackend, serveWorkflows, type WorkflowServer } from '@/lib/workflows/test/workflow-server'
import RunPane from './run-pane'
import { runRefreshMs } from './server-runs'
import { serveRuns, type RunServer } from './test-run-server'

/** Renders the page and waits until it has loaded the backend's projects. */
const renderPage = async () => {
  render(
    <MemoryRouter initialEntries={['/chat']}>
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <UserContext.Provider value={demoUser}>
          <TooltipProvider>
            <Page />
          </TooltipProvider>
        </UserContext.Provider>
      </QueryClientProvider>
    </MemoryRouter>,
  )
  const column = await screen.findByRole('complementary', { name: '聊天记录' })
  await within(column).findByRole('region', { name: '项目 ora-space/cloud' })
}
const sidebar = () => screen.getByRole('complementary', { name: '聊天记录' })
const project = () => within(sidebar()).getByRole('region', { name: '项目 ora-space/cloud' })

/** Creates a run of a published workflow from the project's + menu, as a person would. */
async function createRunOf(user: ReturnType<typeof userEvent.setup>, workflow: RegExp, name: string) {
  await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
  await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
  // A pointer leaving the submenu's trigger closes the submenu in jsdom, so the item is clicked in place.
  fireEvent.click(await screen.findByRole('menuitem', { name: workflow }))
  const dialog = await screen.findByRole('dialog', { name: '运行工作流' })
  const field = within(dialog).getByLabelText('运行名称')
  await user.clear(field)
  await user.type(field, name)
  await user.click(within(dialog).getByRole('button', { name: '创建运行' }))
}

/** Creates a run of the review sample from the project's + menu, as a person would. */
async function createRun(user: ReturnType<typeof userEvent.setup>, name: string) {
  await createRunOf(user, /示例：代码审查/, name)
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
    vi.unstubAllEnvs()
  })

  it('uses the production picker, Overview, Start dialog and Theater for the cdase demo', async () => {
    vi.stubEnv('VITE_DEMO_MOCK', '1')
    const user = userEvent.setup()
    await renderPage()
    await user.click(within(project()).getByRole('button', { name: '在 ora-space/cloud 新建对话' }))
    await user.click(await screen.findByRole('menuitem', { name: '运行工作流' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: /cdase · SFMEA 评审/ }))
    await user.click(within(await screen.findByRole('dialog', { name: '运行工作流' })).getByRole('button', { name: '创建运行' }))
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    expect(await within(pane).findByRole('button', { name: '全图' }, { timeout: 10000 })).toHaveAttribute('aria-pressed', 'true')
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))
    expect(within(pane).getByRole('button', { name: '舞台' })).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(within(pane).getByText('运行完成')).toBeInTheDocument(), { timeout: 15000 })
    expect(within(pane).getByText(/本次仅输出文本评审结论，未修改仓库文件/)).toBeInTheDocument()
    expect(runs.runs.size).toBe(0)
    expect(screen.queryByText('让研发经验进入每一次任务')).not.toBeInTheDocument()
  }, 25000)

  it('creates a run in the chosen project from the visible new-chat entry', async () => {
    const user = userEvent.setup()
    await renderPage()
    await user.click(screen.getByRole('button', { name: '选择项目' }))
    await user.click(await screen.findByRole('option', { name: 'ora-space/desktop' }))
    await user.click(screen.getByRole('button', { name: '运行工作流' }))
    await user.click(await screen.findByRole('menuitem', { name: /示例：代码审查/ }))
    const dialog = await screen.findByRole('dialog', { name: '运行工作流' })
    await user.click(within(dialog).getByRole('button', { name: '创建运行' }))
    await screen.findByRole('region', { name: '工作流运行' })
    await waitFor(() => expect([...runs.runs.values()]).toHaveLength(1))
    expect([...runs.runs.values()][0]!.detail.run.projectId).toBe('p-desktop')
    expect(screen.queryByRole('dialog', { name: '运行工作流' })).not.toBeInTheDocument()
  })

  it('shows a run read-only on a page outside chat, on the tenant it is given', async () => {
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '任务页的运行')
    await screen.findByRole('region', { name: '工作流运行' })
    await waitFor(() => expect(runs.runs.size).toBe(1))
    const runId = [...runs.runs.keys()][0]!
    cleanup()
    resetChatStore()

    const workspace = { open: false, setOpen: () => {}, request: undefined, navigation: { openDiff: () => {} } }
    render(<MemoryRouter><QueryClientProvider client={new QueryClient()}><TooltipProvider><RunPane runId={runId} tenantId={tenantId} readOnly workspace={workspace} /></TooltipProvider></QueryClientProvider></MemoryRouter>)
    expect(await screen.findByRole('heading', { level: 2, name: '任务页的运行' })).toBeInTheDocument()
    // The host keeps the workspace toggle, so the view has none of its own.
    for (const name of ['启动', '停止', '再跑一次', '在对话中追问', '工作区']) expect(screen.queryByRole('button', { name })).not.toBeInTheDocument()
    expect(screen.getByRole('group', { name: '运行视图' })).toBeInTheDocument()
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

    // Starting moves to the Theater, which ends on the result act. Nothing on the path is
    // unsupported, so once the frozen graph has been analyzed the dialog warns about nothing.
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const start = await screen.findByRole('dialog')
    await waitFor(() => expect(workflows.seen).toContain(`POST /api/v2/tenants/${tenantId}/workflows/analyze`))
    expect(within(start).queryByRole('alert')).not.toBeInTheDocument()
    await user.click(within(start).getByRole('button', { name: '启动' }))
    expect(within(pane).getByRole('button', { name: '舞台' })).toHaveAttribute('aria-pressed', 'true')
    await waitFor(() => expect(within(project()).getByRole('button', { name: '工作流运行 审查 cloud#210（成功）' })).toBeInTheDocument(), { timeout: 25_000 })
    // The run went pending → running → succeeded under polling; its frozen graph was analyzed once.
    expect(workflows.seen.filter((request) => request.endsWith('/workflows/analyze'))).toHaveLength(1)
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

    // Its details say what it actually ran on, and name the settings the attempt recorded as not applied.
    await user.click(within(pane).getByRole('button', { name: '打开节点详情' }))
    const details = await within(pane).findByLabelText('阶段详情')
    expect(within(details).getByLabelText('实际执行')).toHaveTextContent('Claude Code · Fixture')
    expect(within(details).getByLabelText('实际执行')).toHaveTextContent('没有找到配置的模型「claude-sonnet-5-5」')
    expect(within(details).getByLabelText('v2 未应用的设置')).toHaveTextContent('v2 未应用：自动重试')

    await user.click(within(pane).getByRole('button', { name: '在对话中追问' }))
    expect(screen.getByRole('button', { name: '选择项目' })).toHaveTextContent('ora-space/cloud')
    const followup = useChatStore.getState().drafts[newChatKey]!.text
    expect(followup).toContain('审查 cloud#210')
    expect(followup).toContain(created!.detail.run.id)
    expect(followup).toContain('独立沙盒')
    expect(screen.getByRole('textbox', { name: '消息' })).toHaveValue(followup)
  }, 30_000)

  it('names the reachable nodes the server says start will refuse, before starting', async () => {
    let reported: { nodeId: string; title: string } | undefined
    // Stands in for v2-server's analysis of the frozen graph: it reports one reachable node.
    workflows.runPolicy = (graph) => {
      const node = (JSON.parse(graph) as { nodes: { id: string; data: { kind: string; title?: string } }[] }).nodes.find((n) => n.data.kind === 'agent')!
      reported = { nodeId: node.id, title: node.data.title ?? node.id }
      return { unappliedSettings: [], unsupportedNodes: [{ nodeId: node.id, kind: 'tool' }] }
    }
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '含不支持节点')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    expect(await within(pane).findByRole('heading', { level: 2, name: '含不支持节点' }, { timeout: 10_000 })).toBeInTheDocument()

    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const start = await screen.findByRole('dialog')
    const warning = await within(start).findByRole('alert')
    expect(warning).toHaveTextContent('有节点暂不支持运行')
    expect(warning).toHaveTextContent(`「${reported!.title}」（工具节点）`)
    expect(warning).toHaveTextContent('desktop 同样不执行')
    // The warning informs; start stays enabled and the refusal stays the server's.
    expect(within(start).getByRole('button', { name: '启动' })).toBeEnabled()
  }, 20_000)

  it('explains an analysis failure and leaves validation to server start', async () => {
    workflows.refuse = { match: /\/workflows\/analyze$/, status: 503, data: { code: 'workflow_domain_unavailable' } }
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '分析暂不可用')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const start = await screen.findByRole('dialog')
    expect(await within(start).findByRole('alert')).toHaveTextContent('暂时无法检查执行路径，启动时将由服务端校验。')
    expect(within(start).getByRole('button', { name: '启动' })).toBeEnabled()
  })

  it('keeps Start disabled until the frozen graph has been analyzed', async () => {
    let release = () => {}
    workflows.hold = { match: /\/workflows\/analyze$/, until: new Promise<void>((resolve) => { release = resolve }) }
    workflows.runPolicy = () => ({ unappliedSettings: [], unsupportedNodes: [] })
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '等待分析')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    expect(await within(pane).findByRole('heading', { level: 2, name: '等待分析' }, { timeout: 10_000 })).toBeInTheDocument()

    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const start = await screen.findByRole('dialog')
    await waitFor(() => expect(workflows.seen).toContain(`POST /api/v2/tenants/${tenantId}/workflows/analyze`))
    // A fast click must not start before the dialog knows what start would refuse.
    expect(within(start).getByText('正在检查执行路径…')).toBeInTheDocument()
    expect(within(start).getByRole('button', { name: '启动' })).toBeDisabled()
    release()
    await waitFor(() => expect(within(start).getByRole('button', { name: '启动' })).toBeEnabled())
    expect(within(start).queryByText('正在检查执行路径…')).not.toBeInTheDocument()
  }, 20_000)

  it('analyzes each pending run on its own, so one run never shows another run\'s warning', async () => {
    // The first analysis (run A) reports an unsupported node; any later one (run B) reports none.
    let analyses = 0
    workflows.runPolicy = (graph) => {
      analyses += 1
      const agent = (JSON.parse(graph) as { nodes: { id: string; data: { kind: string } }[] }).nodes.find((n) => n.data.kind === 'agent')!
      return { unappliedSettings: [], unsupportedNodes: analyses === 1 ? [{ nodeId: agent.id, kind: 'tool' }] : [] }
    }
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '运行甲')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    expect(await within(pane).findByRole('heading', { level: 2, name: '运行甲' }, { timeout: 10_000 })).toBeInTheDocument()
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const first = await screen.findByRole('dialog')
    expect(await within(first).findByRole('alert')).toHaveTextContent('有节点暂不支持运行')
    await user.keyboard('{Escape}')
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())

    await createRun(user, '运行乙')
    expect(await within(pane).findByRole('heading', { level: 2, name: '运行乙' }, { timeout: 10_000 })).toBeInTheDocument()
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    const second = await screen.findByRole('dialog')
    await waitFor(() => expect(analyses).toBe(2))
    await waitFor(() => expect(within(second).getByRole('button', { name: '启动' })).toBeEnabled())
    expect(within(second).queryByRole('alert')).not.toBeInTheDocument()
  }, 30_000)

  it('analyzes a finished run once and explains each Agent from that analysis with its own list', async () => {
    // Two Agents with different lists: each node's details must show only its own.
    workflows.runPolicy = (graph) => {
      const [first, second] = (JSON.parse(graph) as { nodes: { id: string; data: { kind: string } }[] }).nodes.filter((n) => n.data.kind === 'agent')
      return { unappliedSettings: [{ nodeId: first!.id, settings: ['role', 'retry'] }, { nodeId: second!.id, settings: ['mcp'] }], unsupportedNodes: [] }
    }
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '失败的运行')
    await waitFor(() => expect(runs.runs.size).toBe(1))
    const [runId, entry] = [...runs.runs.entries()][0]!
    cleanup()
    resetChatStore()
    // The Agent failed: v2-server writes no `unapplied` into a failure payload.
    const agent = (JSON.parse(entry.graph) as { nodes: { id: string; data: { kind: string; title?: string } }[] }).nodes.find((n) => n.data.kind === 'agent')!
    const at = entry.detail.run.createdAt
    entry.detail.nodes.push({
      id: 'node-run-failed', runId, scopeId: `root:${runId}`, nodeId: agent.id, nodeType: 'agent', sessionId: null, status: 'failed', input: null, output: null,
      error: 'model unavailable', payload: null, iteration: null, startedAt: at, finishedAt: at, createdAt: at, updatedAt: at,
    })
    Object.assign(entry.detail.run, { status: 'failed', startedAt: at, finishedAt: at, error: 'model unavailable' })
    const analyses = () => workflows.seen.filter((request) => request.endsWith('/workflows/analyze')).length
    const before = analyses()

    const workspace = { open: false, setOpen: () => {}, request: undefined, navigation: { openDiff: () => {} } }
    render(<MemoryRouter><QueryClientProvider client={new QueryClient()}><UserContext.Provider value={demoUser}><TooltipProvider><RunPane runId={runId} tenantId={tenantId} readOnly workspace={workspace} /></TooltipProvider></UserContext.Provider></QueryClientProvider></MemoryRouter>)
    expect(await screen.findByRole('heading', { level: 2, name: '失败的运行' })).toBeInTheDocument()
    await waitFor(() => expect(analyses()).toBe(before + 1))

    await user.click(screen.getByRole('button', { name: '舞台' }))
    await user.click(await screen.findByRole('button', { name: `${agent.data.title}: 失败` }))
    await user.click(await screen.findByRole('button', { name: '打开节点详情' }))
    const details = await screen.findByLabelText('阶段详情')
    expect(await within(details).findByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent(/^v2 未应用：角色、自动重试/)
    // The second Agent has not run; its details show its own analysis list, not the first Agent's.
    const second = (JSON.parse(entry.graph) as { nodes: { id: string; data: { kind: string; title?: string } }[] }).nodes.filter((n) => n.data.kind === 'agent')[1]!
    await user.click(screen.getByRole('button', { name: `${second.data.title}: 未执行` }))
    await waitFor(() => expect(within(screen.getByLabelText('阶段详情')).getByRole('note', { name: 'v2 未应用的设置' })).toHaveTextContent(/^v2 未应用：MCP/))
    // The frozen graph never changes: switching views and reading details asks for no second analysis.
    await user.click(screen.getByRole('button', { name: '全图' }))
    await expect(waitFor(() => expect(analyses()).toBeGreaterThan(before + 1), { timeout: 300 })).rejects.toThrow()
  }, 20_000)

  it('keeps every node reachable when a run contains a form node', async () => {
    const graph = {
      nodes: [
        { id: 'start', data: { kind: 'start', title: '开始：巡检', description: '接收巡检输入' } },
        { id: 'review', data: { kind: 'form', title: '确认巡检问题', description: '人工确认巡检结果' } },
        { id: 'tasks', data: { kind: 'createTasks', title: 'tasks', description: '', createTasksConfig: { formNodeId: 'review' } } },
        { id: 'output', data: { kind: 'output', title: '输出处置结果', description: '', outputs: [] } },
      ].map((node, index) => ({ ...node, type: 'workflow', position: { x: index * 340, y: 100 } })),
      edges: [['start', 'review'], ['review', 'tasks'], ['tasks', 'output']].map(([source, target]) => ({ id: `${source}-${target}`, source, target })),
    }
    const created = workflows.backend.create({ name: '巡检节点验收', graph: JSON.stringify(graph) })
    workflows.backend.publish({ workflowId: created.workflow.id, version: 'v1' })

    const user = userEvent.setup()
    await renderPage()
    await createRunOf(user, /巡检节点验收/, '巡检节点验收运行')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await within(pane).findByRole('heading', { level: 2, name: '巡检节点验收运行' }, { timeout: 10_000 })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))

    // A finished run opens on its confirmation node; the test run has no form payload,
    // so the node shows the form's waiting state rather than a result act.
    expect(await within(pane).findByText('表单尚未生成，运行到确认步骤后会自动显示。', {}, { timeout: 15_000 })).toBeInTheDocument()

    // Clicking another node opens that node's own stage and leaves the form behind.
    await user.click(within(pane).getByRole('button', { name: '开始：巡检: 成功' }))
    expect(within(pane).getAllByText('接收巡检输入').length).toBeGreaterThan(0)
    expect(within(pane).queryByText('表单尚未生成，运行到确认步骤后会自动显示。')).not.toBeInTheDocument()

    await user.click(within(pane).getByRole('button', { name: 'tasks: 成功' }))
    expect(within(pane).getByRole('region', { name: '任务处理结果' })).toBeInTheDocument()
    expect(within(pane).queryByText('表单尚未生成，运行到确认步骤后会自动显示。')).not.toBeInTheDocument()
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

  it("tells a member that a running node's live session belongs to whoever started the run", async () => {
    runs.hold = true
    const user = userEvent.setup()
    await renderPage()
    await createRun(user, '别人的运行')
    const pane = await screen.findByRole('region', { name: '工作流运行' })
    await within(pane).findByRole('heading', { level: 2, name: '别人的运行' }, { timeout: 10_000 })
    await user.click(within(pane).getByRole('button', { name: '启动' }))
    await user.click(within(await screen.findByRole('dialog')).getByRole('button', { name: '启动' }))
    await user.click(await within(pane).findByRole('button', { name: '查看节点会话' }))
    expect(within(pane).getByText('正在准备…')).toBeInTheDocument()

    // v2-server answers 404 to anyone but the run's starter; the browser closes the stream for good.
    const [entry] = [...runs.runs.values()]
    const attempt = entry!.detail.nodes.find((n) => n.nodeType === 'agent')!
    const stream = FakeEventSource.all.find((s) => s.url.endsWith(`/nodes/${attempt.id}/events`))!
    act(() => stream.refuse())
    expect(within(pane).getByText('只有启动这次运行的人能查看实时会话；节点完成后会显示任务和回答。')).toBeInTheDocument()
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
