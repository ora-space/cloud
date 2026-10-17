import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Workflow, WorkflowRun, WorkflowSnapshot } from '@/api/generated.schemas'
import { createDefaultWorkflowCapabilities } from '@/features/workflows/runtime/capabilities'
import { WorkflowEditor } from '@/features/workflows/editor/workflow-editor'
import { installCloudSpaceHandlers, TEST_TENANT_ID, workflowFixture } from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { stubViewportGeometry } from '@/test/viewport'
import { renderWithProviders } from '@/test/render'

/** A graph with one iteration frame and its members, plus an outside card. */
function iterationGraphFixture() {
  const agentConfig = AGENT_CONFIG
  return {
    nodes: [
      {
        id: 'iteration-1',
        type: 'workflow',
        position: { x: 0, y: 0 },
        deletable: true,
        data: { kind: 'iteration', title: '迭代 1', description: '逐条处理' },
      },
      {
        id: 'agent-1',
        type: 'workflow',
        position: { x: 120, y: 100 },
        parentId: 'iteration-1',
        data: { kind: 'agent', title: '成员 Agent', description: '', agentConfig },
      },
      {
        id: 'agent-2',
        type: 'workflow',
        position: { x: 420, y: 100 },
        parentId: 'iteration-1',
        data: { kind: 'agent', title: '成员 Agent 2', description: '', agentConfig },
      },
      {
        id: 'output-1',
        type: 'workflow',
        position: { x: 900, y: 0 },
        data: { kind: 'output', title: '输出 1', description: '返回结果' },
      },
    ],
    edges: [
      {
        id: 'e-iter-agent-1',
        source: 'iteration-1',
        sourceHandle: 'iteration-entry',
        target: 'agent-1',
      },
      { id: 'e-agent-1-agent-2', source: 'agent-1', target: 'agent-2' },
      { id: 'e-agent-2-output', source: 'agent-2', target: 'output-1' },
    ],
    viewport: { x: 0, y: 0, zoom: 1 },
  }
}

/** The frame React Flow drew for one iteration node. */
function frame(id: string): HTMLElement {
  const element = document.querySelector(
    `[data-workflow-iteration-frame][data-workflow-node-id="${id}"]`,
  )
  if (!(element instanceof HTMLElement)) {
    throw new Error(`the canvas did not draw an iteration frame for "${id}"`)
  }
  return element
}

/** The internal start seam of an expanded iteration frame. */
function entrySeam(region: HTMLElement): HTMLElement {
  const seam = region.querySelector('[data-workflow-iteration-start]')
  if (!(seam instanceof HTMLElement)) {
    throw new Error('the frame has no internal start seam')
  }
  return seam
}

/** Execution contract the stored Agent node carries. */
const AGENT_CONFIG = createDefaultWorkflowCapabilities().defaultAgentConfig

/**
 * The graph the editor opens: a chain with one node per inspector panel, plus a
 * Condition node, which the palette cannot author yet but a stored graph can hold.
 *
 * The cards sit inside the stubbed viewport below, because React Flow hides a
 * node whose box does not intersect the one it measured.
 */
function graphFixture() {
  return {
    nodes: [
      {
        id: 'start-1',
        type: 'workflow',
        position: { x: 0, y: 0 },
        deletable: false,
        data: { kind: 'start', title: '开始 1', description: '定义输入', input: '整理需求' },
      },
      {
        id: 'agent-1',
        type: 'workflow',
        position: { x: 240, y: 0 },
        data: {
          kind: 'agent',
          title: 'Agent 1',
          description: '执行任务',
          agentConfig: AGENT_CONFIG,
        },
      },
      {
        id: 'condition-1',
        type: 'workflow',
        position: { x: 480, y: 0 },
        data: { kind: 'condition', title: '条件 1', description: '按结果分支' },
      },
      {
        id: 'output-1',
        type: 'workflow',
        position: { x: 720, y: 0 },
        data: { kind: 'output', title: '输出 1', description: '返回结果' },
      },
    ],
    edges: [
      { id: 'e-start-1-agent-1', source: 'start-1', target: 'agent-1' },
      { id: 'e-agent-1-condition-1', source: 'agent-1', target: 'condition-1' },
      { id: 'e-condition-1-output-1', source: 'condition-1', target: 'output-1' },
    ],
    viewport: { x: 0, y: 0, zoom: 1 },
  }
}

/** A graph with no nodes, for the states that only exist before anything is drawn. */
const EMPTY_GRAPH = { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } }

/** What a test may vary about the editor it mounts. */
interface EditorOptions {
  /** Stored graph to open; the default chain when omitted. */
  graph?: Workflow['graph']
  /** Status the save endpoint answers with. */
  saveStatus?: number
  /** Published snapshots the run entry sees; empty means the run button stays off. */
  snapshots?: WorkflowSnapshot[]
}

/** The run a toolbar 运行 click mints, as the create endpoint returns it. */
function runResultFixture(): WorkflowRun {
  return {
    id: 'run-on-demand',
    tenantId: TEST_TENANT_ID,
    workflowId: workflowFixture().id,
    snapshotId: 'snap-1',
    name: '安全审查',
    workflowName: '安全审查',
    status: 'succeeded',
    input: {},
    nodeStates: {},
    rounds: [],
    error: '',
    definitionSnapshot: null,
    startedAt: '2026-09-20T02:00:00.000Z',
    finishedAt: '2026-09-20T02:00:10.000Z',
    createdAt: '2026-09-20T10:00:00+08:00',
    updatedAt: '2026-09-20T10:00:10+08:00',
  }
}

/**
 * Mounts the editor and captures every graph save it sends, answering with the
 * next version or with a failure. The run entry also queries snapshots and run
 * history on mount, so both endpoints answer here too — otherwise the strict
 * mock server would reject every existing editor test.
 */
function renderEditor({
  graph = graphFixture(),
  saveStatus = 200,
  snapshots = [],
}: EditorOptions = {}) {
  const bodies: unknown[] = []
  let version = 1
  server.use(
    http.put(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid`, async ({ request }) => {
      bodies.push(await request.json())
      if (saveStatus >= 400) {
        return HttpResponse.json(
          { code: 'workflow_conflict', message: 'conflict' },
          { status: saveStatus },
        )
      }
      version += 1
      return HttpResponse.json(workflowFixture({ version }))
    }),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/snapshots`, () =>
      HttpResponse.json({ items: snapshots, nextCursor: '' }),
    ),
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/runs`, () =>
      HttpResponse.json({ items: [], nextCursor: '' }),
    ),
    http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/runs`, () =>
      HttpResponse.json({ resource: runResultFixture() }),
    ),
  )
  installCloudSpaceHandlers('admin')
  renderWithProviders(
    <WorkflowEditor tenantId={TEST_TENANT_ID} workflow={workflowFixture({ graph })} />,
  )
  return bodies
}

/** The card React Flow drew for one stored node. */
function nodeCard(id: string): HTMLElement {
  const card = document.querySelector(`[data-workflow-node-id="${id}"]`)
  if (!(card instanceof HTMLElement)) {
    throw new Error(`the canvas did not draw a card for "${id}"`)
  }
  return card
}

/** The palette dock floating over the canvas. */
function palette(): HTMLElement {
  return screen.getByRole('toolbar', { name: '添加工作流节点' })
}

/**
 * The body of the next saved graph, once the autosave has transmitted it.
 *
 * The debounced save can take a full debounce interval plus a request round
 * trip, so the wait deliberately outlasts the default timeout.
 */
async function nextSavedGraph(bodies: unknown[]): Promise<unknown> {
  await waitFor(() => expect(bodies).toHaveLength(1), { timeout: 4000 })
  return bodies[0]
}

/** Clicks 立即保存 and resolves with the body once that write was sent. */
async function saveNow(bodies: unknown[]): Promise<unknown> {
  fireEvent.click(screen.getByRole('button', { name: '立即保存' }))
  return nextSavedGraph(bodies)
}

/** Asserts the saved graph contains one node matching `expected` wholesale. */
function expectSavedNode(saved: unknown, expected: Record<string, unknown>): void {
  expect(saved).toMatchObject({
    graph: {
      nodes: expect.arrayContaining([expect.objectContaining(expected)]),
    },
  })
}

/** The scroll container inside the palette dock. */
function paletteScroller(): HTMLElement {
  const scroller = within(palette()).getByRole('button', { name: '开始' }).parentElement
    ?.parentElement
  if (scroller === null || scroller === undefined) {
    throw new Error('the palette has no scroll container')
  }
  return scroller
}

/** The configuration rail. */
function inspector(): HTMLElement {
  const rail = document.querySelector('[data-workflow-inspector]')
  if (!(rail instanceof HTMLElement)) {
    throw new Error('the inspector is not mounted')
  }
  return rail
}

/**
 * Drags a palette entry from the dock to a point on the canvas.
 *
 * The gesture has to end with a release, otherwise the panel separator's
 * document-level drag stays armed and outlives the test.
 */
function dragPaletteEntry(name: string, to: { x: number; y: number }): void {
  const entry = within(palette()).getByRole('button', { name })
  fireEvent.pointerDown(entry, {
    button: 0,
    isPrimary: true,
    pointerId: 1,
    clientX: 40,
    clientY: 40,
  })
  fireEvent.pointerMove(entry, { pointerId: 1, clientX: to.x, clientY: to.y })
  fireEvent.pointerUp(entry, { pointerId: 1, clientX: to.x, clientY: to.y })
}

/**
 * Clicks undo, waits for the redo button to arm, then re-reads the inspector's
 * description against the restored card. Undo clears the selection, so the card
 * must be re-picked before the field is reachable again.
 */
async function undoDescription(expected: string): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: '撤销' }))
  await waitFor(() => expect(screen.getByRole('button', { name: '重做' })).toBeEnabled())
  fireEvent.click(nodeCard('agent-1'))
  await waitFor(() => expect(within(inspector()).getByLabelText('描述')).toHaveValue(expected))
}

/** Mirrors {@link undoDescription}, driven by the redo button instead. */
async function redoDescription(expected: string): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: '重做' }))
  await waitFor(() => expect(screen.getByRole('button', { name: '撤销' })).toBeEnabled())
  fireEvent.click(nodeCard('agent-1'))
  await waitFor(() => expect(within(inspector()).getByLabelText('描述')).toHaveValue(expected))
}

/**
 * Adds one Start input variable through the Start panel's create dialog, and
 * waits for the saved row to come back through the node-data channel.
 */
function addVariableRecipient(): void {
  fireEvent.click(nodeCard('start-1'))
  fireEvent.click(within(inspector()).getByRole('button', { name: '添加输入变量' }))
  fireEvent.change(screen.getByLabelText('变量名'), { target: { value: 'recipient_email' } })
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
  expect(within(inspector()).getByText('recipient_email')).toBeInTheDocument()
}

/** Adds one custom variable through the 全局变量 dialog and saves it. */
async function addGlobalVariableAlliance(value: string): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: '全局变量' }))
  fireEvent.click(screen.getByRole('button', { name: '添加全局变量' }))
  fireEvent.change(screen.getByLabelText('全局变量 2 名称'), {
    target: { value: 'global.region' },
  })
  fireEvent.change(screen.getByLabelText('全局变量 2 值'), { target: { value } })
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
}

/** The default chain with a launch-field declaration and Start variables added. */
function declaredGraphFixture(launchFields: unknown[], inputVariables: unknown[] = []) {
  const graph = graphFixture()
  // The chain's Start node is its first, and the only node these tests reach into. The fixture is
  // built fresh on every call and shared with nobody, so it is patched in place.
  const start = graph.nodes[0]
  if (start !== undefined) {
    start.data = Object.assign({}, start.data, { inputVariables })
  }
  return { ...graph, launchFields }
}

/** Opens the `@` form fields dialog from the toolbar. */
function openLaunchFields(): void {
  fireEvent.click(screen.getByRole('button', { name: '@ 表单字段' }))
}

/** The ask switch of one launch-field row, found by the row's title. */
function launchAskSwitch(field: string): HTMLElement {
  const card = screen.getByText(field).closest('.rounded-lg')
  if (!(card instanceof HTMLElement)) {
    throw new Error(`launch field ${field} has no row container`)
  }
  const control = within(card).getAllByRole('switch')[0]
  if (control === undefined) {
    throw new Error(`launch field ${field} has no ask switch`)
  }
  return control
}

/**
 * Interactions go through `fireEvent` rather than `userEvent` here because
 * `userEvent` dispatches pointer events, and the panel separator behind the
 * canvas claims the press, which both moves focus off the field being typed
 * into and arms a panel drag that outlives the test.
 */
beforeEach(() => {
  stubViewportGeometry()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('WorkflowEditor', () => {
  it('draws the stored graph with its counts and version', () => {
    renderEditor()

    expect(nodeCard('start-1')).toBeInTheDocument()
    expect(nodeCard('agent-1')).toBeInTheDocument()
    expect(nodeCard('condition-1')).toBeInTheDocument()
    expect(nodeCard('output-1')).toBeInTheDocument()
    expect(screen.getByText(/4 个节点/)).toBeInTheDocument()
    expect(screen.getByText(/3 条连线/)).toBeInTheDocument()
    expect(screen.getByText('版本 1')).toBeInTheDocument()
    expect(screen.getByText('已保存')).toBeInTheDocument()
  })

  it('summarizes a configured node on its card', () => {
    renderEditor()

    expect(within(nodeCard('start-1')).getByText('整理需求')).toBeInTheDocument()
    expect(within(nodeCard('agent-1')).getByText('角色')).toBeInTheDocument()
  })

  it('asks for a selection until a node is picked', () => {
    renderEditor()

    expect(screen.getByText('未选择节点')).toBeInTheDocument()
    expect(document.querySelector('[data-workflow-inspector]')).not.toBeInTheDocument()
  })

  it('opens the selected node in the inspector, and writes an edit back', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    const description = within(inspector()).getByLabelText('描述')
    expect(description).toHaveValue('执行任务')

    fireEvent.change(description, { target: { value: '复核改动' } })

    const saved = await saveNow(bodies)
    expect(saved).toMatchObject({ version: 1 })
    expectSavedNode(saved, {
      id: 'agent-1',
      data: expect.objectContaining({ description: '复核改动' }),
    })
  })

  it('saves an edit on its own once the debounce elapses', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('start-1'))
    fireEvent.change(within(inspector()).getByLabelText('初始提示词'), {
      target: { value: '写一份周报' },
    })

    expect(await screen.findByText('未保存')).toBeInTheDocument()
    expectSavedNode(await nextSavedGraph(bodies), {
      id: 'start-1',
      data: expect.objectContaining({ input: '写一份周报' }),
    })
    expect(await screen.findByText('已保存')).toBeInTheDocument()
  })

  it('reports a rejected write instead of pretending it landed', async () => {
    renderEditor({ saveStatus: 409 })

    fireEvent.click(nodeCard('agent-1'))
    fireEvent.change(within(inspector()).getByLabelText('描述'), { target: { value: '被拒绝' } })
    fireEvent.click(screen.getByRole('button', { name: '立即保存' }))

    expect(await screen.findByText('保存失败')).toBeInTheDocument()
  })

  it('advances the version badge when a write lands', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    fireEvent.change(within(inspector()).getByLabelText('描述'), { target: { value: '已落库' } })
    await saveNow(bodies)

    await waitFor(() => expect(screen.getByText('版本 2')).toBeInTheDocument())
  })

  it('rolls the canvas back to a published snapshot from history', async () => {
    const restored = workflowFixture({ version: 5, graph: EMPTY_GRAPH })
    const published: WorkflowSnapshot = {
      id: 'snap-1',
      tenantId: TEST_TENANT_ID,
      workflowId: '44444444-4444-4444-4444-444444444444',
      name: '首发',
      version: 1,
      graph: EMPTY_GRAPH,
      createdAt: '2026-09-20T10:00:00+08:00',
    }
    // Only the PUT needs a bespoke handler; the snapshots GET comes from
    // renderEditor's own mock, because registering it here (before renderEditor)
    // would be shadowed by the later-registered one and the popover would open
    // onto an empty history.
    server.use(
      http.put(
        `/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/snapshots/:snapshotId/restore`,
        () => HttpResponse.json(restored),
      ),
    )
    renderEditor({ snapshots: [published] })

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))
    fireEvent.click(await screen.findByRole('button', { name: '回滚到版本 1' }))
    fireEvent.click(screen.getByRole('button', { name: '回滚' }))

    await waitFor(() => expect(screen.getByText('版本 5')).toBeInTheDocument())
    expect(screen.queryByText('开始 1')).not.toBeInTheDocument()
  })

  it('renames a node from the inspector header, and saves the new title', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    fireEvent.doubleClick(within(inspector()).getByTitle('双击重命名'))
    const name = within(inspector()).getByLabelText('名称')
    fireEvent.change(name, { target: { value: '审阅者' } })
    fireEvent.keyDown(name, { key: 'Enter' })

    expect(within(nodeCard('agent-1')).getByText('审阅者')).toBeInTheDocument()
    expectSavedNode(await nextSavedGraph(bodies), {
      id: 'agent-1',
      data: expect.objectContaining({ title: '审阅者' }),
    })
  })

  it('restores the previous title when the rename is cancelled', () => {
    renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    fireEvent.doubleClick(within(inspector()).getByTitle('双击重命名'))
    const name = within(inspector()).getByLabelText('名称')
    fireEvent.change(name, { target: { value: '废弃' } })
    fireEvent.keyDown(name, { key: 'Escape' })

    expect(within(nodeCard('agent-1')).getByText('Agent 1')).toBeInTheDocument()
  })

  it('renders an editable Condition panel for a stored node', () => {
    renderEditor()

    fireEvent.click(nodeCard('condition-1'))

    // A stored Condition node without authored cases opens one empty IF card with
    // its implicit else branch, so the panel is editable rather than a placeholder.
    expect(within(inspector()).getByText('IF')).toBeInTheDocument()
    expect(within(inspector()).getByText('ELSE')).toBeInTheDocument()
    expect(within(inspector()).getByRole('button', { name: '添加条件' })).toBeInTheDocument()
  })

  it('adds a node from the palette at the centre of the canvas', async () => {
    const bodies = renderEditor()

    fireEvent.click(within(palette()).getByRole('button', { name: '输出' }))

    expect(screen.getByText(/5 个节点/)).toBeInTheDocument()
    expectSavedNode(await saveNow(bodies), {
      id: 'output-2',
      data: expect.objectContaining({ kind: 'output' }),
    })
  })

  it('drops a dragged palette entry where the pointer was released', () => {
    renderEditor()

    dragPaletteEntry('Agent', { x: 120, y: 90 })

    expect(nodeCard('agent-2')).toBeInTheDocument()
    expect(screen.getByText(/5 个节点/)).toBeInTheDocument()
  })

  it('adds nothing when a palette drag is released outside the canvas', () => {
    renderEditor()

    dragPaletteEntry('Agent', { x: 1200, y: 900 })

    expect(screen.getByText(/4 个节点/)).toBeInTheDocument()
  })

  it('removes the selected node', async () => {
    renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    // React Flow confirms the removal through an awaited hook, so the graph
    // settles a tick after the press rather than in the same one.
    fireEvent.click(within(nodeCard('agent-1')).getByRole('button', { hidden: true }))

    await waitFor(() => {
      expect(document.querySelector('[data-workflow-node-id="agent-1"]')).not.toBeInTheDocument()
    })
    expect(screen.getByText(/3 个节点/)).toBeInTheDocument()
  })

  it('offers no delete control for the node the run starts from', () => {
    renderEditor()

    fireEvent.click(nodeCard('start-1'))

    expect(
      within(nodeCard('start-1')).queryByRole('button', { hidden: true }),
    ).not.toBeInTheDocument()
  })

  it('re-arranges the graph in execution order', () => {
    renderEditor()

    fireEvent.click(screen.getByRole('button', { name: '自动排列' }))

    expect(nodeCard('start-1')).toBeInTheDocument()
    expect(nodeCard('output-1')).toBeInTheDocument()
  })

  it('keeps a wheel over the palette from reaching the canvas behind it', () => {
    renderEditor()
    const scroller = paletteScroller()
    const reachedDocument: unknown[] = []
    const listener = (event: WheelEvent): void => {
      reachedDocument.push(event)
    }
    document.addEventListener('wheel', listener)

    fireEvent.wheel(scroller, { deltaY: 40 })

    document.removeEventListener('wheel', listener)
    expect(reachedDocument).toHaveLength(0)
    expect(scroller.scrollLeft).toBe(0)
  })

  it('disables the start entry once the graph already has one', () => {
    renderEditor()

    expect(within(palette()).getByRole('button', { name: '开始' })).toBeDisabled()
  })

  it('offers the start entry on an empty canvas', () => {
    renderEditor({ graph: EMPTY_GRAPH })

    expect(within(palette()).getByRole('button', { name: '开始' })).toBeEnabled()
    expect(screen.getByText(/0 个节点/)).toBeInTheDocument()
  })
})

describe('WorkflowEditor iteration regions', () => {
  it('renders the frame with its member count and internal start seam', () => {
    renderEditor({ graph: iterationGraphFixture() })

    const region = frame('iteration-1')
    expect(within(region).getByText('2 个成员')).toBeInTheDocument()
    expect(entrySeam(region)).toBeInTheDocument()
    // The empty hint is only for a region with no members.
    expect(within(region).queryByText('从入口添加节点开始搭建')).not.toBeInTheDocument()
  })

  it('hides the members and their edges when the region folds, and saves it', async () => {
    const bodies = renderEditor({ graph: iterationGraphFixture() })

    // The frame is drawn `visibility: hidden` because React Flow has not
    // measured its box, so its buttons compute an empty accessible name and
    // role queries cannot match them; the aria-label is matched directly.
    fireEvent.click(within(frame('iteration-1')).getByLabelText('折叠迭代区域'))

    expect(within(frame('iteration-1')).getByLabelText('展开迭代区域')).toBeInTheDocument()
    expectSavedNode(await saveNow(bodies), {
      id: 'iteration-1',
      data: expect.objectContaining({ collapsed: true }),
    })
  })

  it('refuses a palette drop that would land a card inside a region', () => {
    renderEditor({ graph: iterationGraphFixture() })

    dragPaletteEntry('Agent', { x: 200, y: 120 })

    // The drop was refused: no new card appeared (still four nodes)...
    expect(screen.getByText(/4 个节点/)).toBeInTheDocument()
    // ...and the hint explains why.
    expect(screen.getByText(/成员请从迭代区域入口添加/)).toBeInTheDocument()
  })

  it('asks before deleting a region with members, and honours the refusal', async () => {
    renderEditor({ graph: iterationGraphFixture() })

    fireEvent.click(frame('iteration-1'))
    fireEvent.click(within(frame('iteration-1')).getByLabelText(/删除/))

    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(
      screen.getByText('该区域包含 2 个成员，删除区域会一并删除它们及其连线。确定继续吗？'),
    ).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '取消' }))

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(frame('iteration-1')).toBeInTheDocument()
    expect(nodeCard('agent-1')).toBeInTheDocument()
  })

  it('removes the whole region once the author confirms the deletion', async () => {
    renderEditor({ graph: iterationGraphFixture() })

    fireEvent.click(frame('iteration-1'))
    fireEvent.click(within(frame('iteration-1')).getByLabelText(/删除/))
    fireEvent.click(screen.getByRole('button', { name: '删除' }))

    await waitFor(() => {
      expect(
        document.querySelector('[data-workflow-node-id="iteration-1"]'),
      ).not.toBeInTheDocument()
    })
    expect(document.querySelector('[data-workflow-node-id="agent-1"]')).not.toBeInTheDocument()
    expect(screen.getByText(/1 个节点/)).toBeInTheDocument()
  })
})

describe('WorkflowEditor session undo and redo', () => {
  it('starts with undo and redo disabled while nothing was edited', () => {
    renderEditor()

    expect(screen.getByRole('button', { name: '撤销' })).toBeDisabled()
    expect(screen.getByRole('button', { name: '重做' })).toBeDisabled()
  })

  it('undo removes a node added through the palette, and redo brings it back', async () => {
    renderEditor()

    dragPaletteEntry('Agent', { x: 120, y: 90 })
    expect(nodeCard('agent-2')).toBeInTheDocument()
    expect(screen.getByText(/5 个节点/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '撤销' }))

    await waitFor(() => {
      expect(document.querySelector('[data-workflow-node-id="agent-2"]')).not.toBeInTheDocument()
    })
    expect(screen.getByText(/4 个节点/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '重做' }))

    await waitFor(() => {
      expect(document.querySelector('[data-workflow-node-id="agent-2"]')).toBeInTheDocument()
    })
    expect(screen.getByText(/5 个节点/)).toBeInTheDocument()
  })

  it('the canvas undo shortcut restores the previous step', async () => {
    renderEditor()

    dragPaletteEntry('Agent', { x: 120, y: 90 })
    expect(screen.getByText(/5 个节点/)).toBeInTheDocument()

    fireEvent.keyDown(window, { key: 'z', ctrlKey: true })

    await waitFor(() => {
      expect(document.querySelector('[data-workflow-node-id="agent-2"]')).not.toBeInTheDocument()
    })
    expect(screen.getByText(/4 个节点/)).toBeInTheDocument()
  })

  it('merges consecutive edits to one field into a single undo step', async () => {
    renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    const description = within(inspector()).getByLabelText('描述')
    fireEvent.change(description, { target: { value: '第一稿' } })
    fireEvent.change(description, { target: { value: '第二稿' } })

    // Both keystrokes coalesced, so one undo lands on the originally stored
    // value rather than the first draft.
    await undoDescription('执行任务')
  })

  it('replays a field edit with redo after it was undone', async () => {
    renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    const description = within(inspector()).getByLabelText('描述')
    fireEvent.change(description, { target: { value: '已录入' } })

    await undoDescription('执行任务')

    // Re-applying the step returns the second edit's value to the field.
    await redoDescription('已录入')
  })
})

describe('WorkflowEditor global variables', () => {
  it('writes a committed declaration into the saved graph', async () => {
    const bodies = renderEditor()

    await addGlobalVariableAlliance('华东')

    const saved = await nextSavedGraph(bodies)
    expect(saved).toMatchObject({
      graph: {
        globalVariables: [
          { name: 'global.region', valueType: 'string', value: '华东' },
          { name: 'sys.workflow_id', valueType: 'string' },
          { name: 'sys.timestamp', valueType: 'number' },
        ],
      },
    })
  })

  it('reopens the dialog on the declarations it committed', async () => {
    renderEditor()

    await addGlobalVariableAlliance('华东')
    fireEvent.click(screen.getByRole('button', { name: '全局变量' }))

    expect(screen.getByLabelText('全局变量 1 名称')).toHaveValue('global.region')
    expect(screen.getByLabelText('全局变量 1 值')).toHaveValue('华东')
  })

  it('undo restores the variable set the save replaced', async () => {
    renderEditor()

    await addGlobalVariableAlliance('华东')
    fireEvent.click(screen.getByRole('button', { name: '撤销' }))
    fireEvent.click(screen.getByRole('button', { name: '全局变量' }))

    // The empty custom list keeps only the add button.
    expect(screen.queryByLabelText('全局变量 1 名称')).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: '添加全局变量' })).toBeInTheDocument()
  })
})

describe('WorkflowEditor launch fields', () => {
  it('writes a committed declaration into the saved graph', async () => {
    const bodies = renderEditor({ graph: declaredGraphFixture([]) })

    openLaunchFields()
    fireEvent.click(launchAskSwitch('运行版本'))
    fireEvent.click(screen.getByRole('button', { name: '保存' }))

    const saved = await nextSavedGraph(bodies)
    expect(saved).toMatchObject({
      graph: {
        launchFields: [
          { key: 'repository', enabled: true, required: true },
          { key: 'branch', enabled: true, required: true },
          { key: 'snapshotId', enabled: false, required: false },
          { key: 'prompt', enabled: true, required: false },
          { key: 'context_refs', enabled: true, required: false },
        ],
      },
    })
  })

  it('reopens the dialog on the declaration it committed', async () => {
    renderEditor({ graph: declaredGraphFixture([]) })

    openLaunchFields()
    fireEvent.click(launchAskSwitch('运行版本'))
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    openLaunchFields()

    expect(launchAskSwitch('运行版本')).not.toBeChecked()
    expect(launchAskSwitch('提示词')).toBeChecked()
  })

  it('opens a stored declaration on the answers it carries', () => {
    renderEditor({
      graph: declaredGraphFixture([{ key: 'prompt', enabled: true, required: true }]),
    })

    openLaunchFields()

    expect(launchAskSwitch('提示词')).toBeChecked()
    expect(screen.getByText('提示词').closest('.rounded-lg')).toHaveTextContent('必填')
  })

  it('marks a row the Start node owns as declared, not editable', () => {
    renderEditor({
      graph: declaredGraphFixture([], [{ name: 'repository', valueType: 'string' }]),
    })

    openLaunchFields()

    expect(screen.getByText('已由开始节点声明，这一行由该变量决定。')).toBeInTheDocument()
  })

  it('undo restores the declaration the save replaced', async () => {
    renderEditor({ graph: declaredGraphFixture([{ key: 'snapshotId', enabled: false }]) })

    openLaunchFields()
    fireEvent.click(launchAskSwitch('运行版本'))
    fireEvent.click(screen.getByRole('button', { name: '保存' }))
    fireEvent.click(screen.getByRole('button', { name: '撤销' }))
    openLaunchFields()

    expect(launchAskSwitch('运行版本')).not.toBeChecked()
  })
})

describe('WorkflowEditor start variables', () => {
  it('writes a committed Start input variable into the saved graph', async () => {
    const bodies = renderEditor()

    addVariableRecipient()

    expectSavedNode(await nextSavedGraph(bodies), {
      id: 'start-1',
      data: expect.objectContaining({
        inputVariables: [
          expect.objectContaining({
            name: 'recipient_email',
            valueType: 'string',
            fieldType: 'text-input',
            required: false,
          }),
        ],
      }),
    })
  })

  it('shows the committed Start variable in the Start panel list', () => {
    renderEditor()

    addVariableRecipient()
    expect(within(inspector()).getByText(/单行文本/)).toBeInTheDocument()
  })

  it('restores the previous declaration list when the edit is undone', () => {
    renderEditor()

    addVariableRecipient()

    fireEvent.click(screen.getByRole('button', { name: '撤销' }))
    // Undo clears the selection, so the Start card is picked again before the
    // panel's list — which the step emptied — can be read.
    fireEvent.click(nodeCard('start-1'))

    expect(within(inspector()).queryByText('recipient_email')).not.toBeInTheDocument()
  })
})

/** The contract the Agent switch seeds a fresh schema from. */
const DEFAULT_OUTPUT_CONTRACT = {
  type: 'structured',
  schema: { type: 'object', properties: {}, required: [], additionalProperties: false },
} as const

/** Turns the picked Agent's structured-output switch on. */
function enableStructuredOutput(): void {
  fireEvent.click(within(inspector()).getByRole('switch', { name: '结构化输出' }))
}

/** Seeds the empty contract with one root field through the dialog. */
function addStructuredOutputField(): void {
  fireEvent.click(screen.getByRole('button', { name: '配置' }))
  fireEvent.click(screen.getByRole('button', { name: /^添加字段$/ }))
  fireEvent.click(screen.getByRole('button', { name: '保存' }))
}

/**
 * Forcibly writes the draft and waits for the body that write lands, so a suite
 * that saves several times can keep up with the debounced autosave stream.
 */
async function flushStructuredOutput(bodies: unknown[]): Promise<unknown> {
  const before = bodies.length
  fireEvent.click(screen.getByRole('button', { name: '立即保存' }))
  await waitFor(() => expect(bodies.length).toBeGreaterThan(before), { timeout: 4000 })
  return bodies.at(-1)
}

describe('WorkflowEditor structured output', () => {
  it('enables the contract and saves the empty default schema', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    enableStructuredOutput()

    expect(within(inspector()).getByText('还没有配置任何字段')).toBeInTheDocument()
    expectSavedNode(await flushStructuredOutput(bodies), {
      id: 'agent-1',
      data: expect.objectContaining({
        agentConfig: expect.objectContaining({ outputContract: DEFAULT_OUTPUT_CONTRACT }),
      }),
    })
  })

  it('adds a field through the dialog and saves the nested schema', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    enableStructuredOutput()
    addStructuredOutputField()

    expectSavedNode(await flushStructuredOutput(bodies), {
      id: 'agent-1',
      data: expect.objectContaining({
        agentConfig: expect.objectContaining({
          outputContract: {
            type: 'structured',
            schema: {
              type: 'object',
              properties: { field_1: { type: 'string' } },
              required: [],
              additionalProperties: false,
            },
          },
        }),
      }),
    })
  })

  it('removes the contract from the graph when the switch is turned off', async () => {
    const bodies = renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    enableStructuredOutput()
    await flushStructuredOutput(bodies)
    enableStructuredOutput()

    // `outputContract` must leave the document entirely rather than become
    // `undefined`, which a later save would never serialize.
    expect(JSON.stringify(await flushStructuredOutput(bodies))).not.toContain('outputContract')
  })

  it('undo drops the contract the toggle added', () => {
    renderEditor()

    fireEvent.click(nodeCard('agent-1'))
    enableStructuredOutput()
    expect(within(inspector()).getByText('还没有配置任何字段')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: '撤销' }))
    // Undo cleared the selection, so the card is picked again before the
    // panel's summary — which the step removed — can be read.
    fireEvent.click(nodeCard('agent-1'))

    expect(within(inspector()).queryByRole('button', { name: '配置' })).not.toBeInTheDocument()
  })
})

describe('WorkflowEditor run tools', () => {
  it('disables the run button and shows an empty history before any snapshot exists', async () => {
    renderEditor()

    // The button only drops out once the empty snapshot list resolves; until
    // then there is no answer either way, so the gate must wait.
    const runButton = screen.getByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeDisabled())

    fireEvent.click(screen.getByRole('button', { name: '运行历史' }))
    expect(screen.getByText('还没有运行记录。')).toBeInTheDocument()
  })

  it('starts no run from the toolbar, which cannot state the Project a run freezes', async () => {
    const published: WorkflowSnapshot = {
      id: 'snap-1',
      tenantId: TEST_TENANT_ID,
      workflowId: workflowFixture().id,
      name: '版本 1',
      version: 1,
      graph: { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } },
      createdAt: '2026-09-20T10:00:00+08:00',
    }
    renderEditor({ snapshots: [published] })
    const runButton = screen.getByRole('button', { name: '运行' })
    await waitFor(() => expect(runButton).toBeEnabled())

    // MSW answers requests with the newest handler first, so a capture
    // registered here — after renderEditor's own POST — wins over the default
    // one and records the body the toolbar actually sends.
    const bodies: unknown[] = []
    server.use(
      http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/runs`, async ({ request }) => {
        bodies.push(await request.json())
        return HttpResponse.json({ resource: runResultFixture() })
      }),
    )
    fireEvent.click(runButton)

    // The run form that picks the Project is not built yet, so the mutation refuses the run before
    // the request is sent and this capture stays empty.
    await waitFor(() => expect(runButton).toBeEnabled())
    expect(bodies).toEqual([])
  })
})
