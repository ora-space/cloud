import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { Workflow as CloudWorkflow, WorkflowSnapshot } from '@/api/generated.schemas'
import { WorkflowPublishDialog } from '@/features/workflows/editor/workflow-publish-dialog'
import { WorkflowVersionHistory } from '@/features/workflows/editor/workflow-version-history'
import { WorkflowVersionTools } from '@/features/workflows/editor/workflow-version-tools'
import {
  installCloudSpaceHandlers,
  installWorkflowPublish,
  TEST_TENANT_ID,
  workflowFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderWithProviders } from '@/test/render'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'

/** A published snapshot as the backend stores it. */
function snapshotFixture(version: number, name = `版本 ${version}`): WorkflowSnapshot {
  return {
    id: `snap-${version}`,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    name,
    version,
    graph: { nodes: [], edges: [], viewport: { x: 0, y: 0, zoom: 1 } },
    createdAt: '2026-09-20T10:00:00+08:00',
  }
}

/** Installs the version endpoints: history listing plus the rollback write. */
function installHistory(
  snapshots: WorkflowSnapshot[],
  restored?: CloudWorkflow,
  restoreStatus = 200,
): void {
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/snapshots`, () =>
      HttpResponse.json({ items: snapshots, nextCursor: '' }),
    ),
    http.put(
      `/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/snapshots/:snapshotId/restore`,
      () => {
        if (restoreStatus >= 400 || restored === undefined) {
          return HttpResponse.json(
            { code: 'workflow_conflict', message: 'conflict' },
            { status: restoreStatus },
          )
        }
        return HttpResponse.json(restored)
      },
    ),
  )
}

/** Installs the publish endpoint against the shared test handler. */
function installPublish(onBody: (body: unknown) => void, status = 200): void {
  installWorkflowPublish(onBody, snapshotFixture(1, '发布 v1'), status)
}

/** Mounts the publish dialog, always open, with the given close callback. */
function renderPublishDialog(onOpenChange: (open: boolean) => void): void {
  renderWithProviders(
    <WorkflowPublishDialog
      tenantId={TEST_TENANT_ID}
      workflowId={WORKFLOW_ID}
      open
      onOpenChange={onOpenChange}
    />,
  )
}

/**
 * Mounts the history popover with the given snapshots behind it, returning the
 * rollback spy the workflow is handed once a restore lands.
 */
function renderHistory(
  snapshots: WorkflowSnapshot[],
  restored?: CloudWorkflow,
  restoreStatus = 200,
): (workflow: CloudWorkflow) => void {
  installHistory(snapshots, restored, restoreStatus)
  const onRestored = vi.fn<(workflow: CloudWorkflow) => void>()
  renderWithProviders(
    <WorkflowVersionHistory
      tenantId={TEST_TENANT_ID}
      workflowId={WORKFLOW_ID}
      committedVersion={2}
      onRestored={onRestored}
    />,
  )
  return onRestored
}

beforeEach(() => {
  // The version tools mount no page, but `renderWithProviders` still resolves
  // the current space, so the session trios must answer and `:wfid` snapshot
  // history must exist before anything asks for it.
  installCloudSpaceHandlers('admin')
})

describe('WorkflowPublishDialog', () => {
  it('publishes the typed name and closes on success', async () => {
    const bodies: unknown[] = []
    installPublish((body) => bodies.push(body))
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    fireEvent.change(screen.getByLabelText('版本名称（可选）'), {
      target: { value: 'v1.0 里程碑' },
    })
    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    expect(bodies).toEqual([{ name: 'v1.0 里程碑' }])
  })

  it('publishes an empty body when the name is left blank', async () => {
    const bodies: unknown[] = []
    installPublish((body) => bodies.push(body))
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    expect(bodies).toEqual([{}])
  })

  it('submits on Enter', async () => {
    const bodies: unknown[] = []
    installPublish((body) => bodies.push(body))
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    const input = screen.getByLabelText('版本名称（可选）')
    fireEvent.change(input, { target: { value: '割接版' } })
    fireEvent.keyDown(input, { key: 'Enter' })

    await waitFor(() => expect(onOpenChange).toHaveBeenCalledWith(false))
    expect(bodies).toEqual([{ name: '割接版' }])
  })

  it('surfaces a backend fault and leaves the dialog open', async () => {
    installPublish(() => {}, 409)
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    expect(await screen.findByText('已存在同名工作流。')).toBeInTheDocument()
    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it('explains the unsupported structured output at the rejected node without closing', async () => {
    server.use(
      http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/publish`, () =>
        HttpResponse.json(
          {
            code: 'workflow_graph_invalid',
            params: {
              problems: [
                { code: 'node_config_unsupported', nodeId: 'agent-1', field: 'outputContract' },
              ],
            },
          },
          { status: 400 },
        ),
      ),
    )
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent('工作流配置未通过发布检查，请修正以下问题后重新发布。')
    expect(alert).toHaveTextContent('节点 agent-1')
    expect(alert).toHaveTextContent(
      'Cloud 当前不支持结构化输出，请关闭该节点的「结构化输出」后重新发布。',
    )
    expect(alert).not.toHaveTextContent('操作失败，请稍后重试。')
    expect(onOpenChange).not.toHaveBeenCalled()
  })

  it.each([undefined, { problems: 'invalid' }, { problems: [null, 3, {}] }])(
    'keeps a useful graph validation summary when problem details are malformed: %j',
    async (params) => {
      server.use(
        http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/publish`, () =>
          HttpResponse.json({ code: 'workflow_graph_invalid', params }, { status: 400 }),
        ),
      )
      renderPublishDialog(vi.fn())

      fireEvent.click(screen.getByRole('button', { name: '发布' }))

      expect(await screen.findByRole('alert')).toHaveTextContent(
        '工作流配置未通过发布检查，请检查节点、连线和变量配置后重新发布。',
      )
    },
  )

  it('lists distinct problem locations and safe guidance for known and future rejections', async () => {
    server.use(
      http.post(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/:wfid/publish`, () =>
        HttpResponse.json(
          {
            code: 'workflow_graph_invalid',
            params: {
              problems: [
                {
                  code: 'agent_schema_version_unsupported',
                  nodeId: 'a',
                  field: 'agentConfig.schemaVersion',
                },
                { code: 'node_config_unsupported', nodeId: 'b', field: 'interactive' },
                { code: 'node_kind_unsupported', nodeId: 'c' },
                {
                  code: 'future_problem',
                  slot: 'reviewer',
                  name: 'vars.result',
                  reason: 'PRIVATE SERVER DETAIL',
                },
                { code: 'future_problem' },
                { code: 'node_kind_unsupported', nodeId: 'c' },
              ],
            },
          },
          { status: 400 },
        ),
      ),
    )
    renderPublishDialog(vi.fn())

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    const alert = await screen.findByRole('alert')
    expect(within(alert).getAllByRole('listitem')).toHaveLength(5)
    expect(alert).toHaveTextContent('节点 a · agentConfig.schemaVersion')
    expect(alert).toHaveTextContent('Cloud 只支持 Agent 配置版本 3')
    expect(alert).toHaveTextContent('节点 b · interactive')
    expect(alert).toHaveTextContent('请关闭或移除后重新发布')
    expect(alert).toHaveTextContent('节点 c')
    expect(alert).toHaveTextContent('请替换或移除该节点')
    expect(alert).toHaveTextContent('reviewer · vars.result')
    expect(alert).not.toHaveTextContent('PRIVATE SERVER DETAIL')
    expect(alert).not.toHaveTextContent('future_problem')
  })

  it('blanks the typed name the moment the dialog closes', () => {
    const onOpenChange = vi.fn<(open: boolean) => void>()
    renderPublishDialog(onOpenChange)

    fireEvent.change(screen.getByLabelText('版本名称（可选）'), {
      target: { value: '各件版本' },
    })
    fireEvent.click(screen.getByRole('button', { name: '取消' }))

    expect(onOpenChange).toHaveBeenCalledWith(false)
    expect(screen.getByLabelText('版本名称（可选）')).toHaveValue('')
  })
})

describe('WorkflowVersionHistory', () => {
  it('lists published versions newest first', async () => {
    renderHistory([
      snapshotFixture(3, '第三版'),
      snapshotFixture(1, '首发'),
      snapshotFixture(2, '第二版'),
    ])

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))

    const list = await screen.findByRole('list')
    const labels = within(list)
      .getAllByRole('button')
      .map((row) => row.getAttribute('aria-label') ?? '')
    expect(labels).toEqual(['回滚到版本 3', '回滚到版本 2', '回滚到版本 1'])
  })

  it('shows the empty state when nothing was published', async () => {
    renderHistory([])

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))

    expect(await screen.findByText('还没有已发布的版本。发布一个以随时回滚。')).toBeInTheDocument()
  })

  it('filters the list by a version name', async () => {
    renderHistory([snapshotFixture(2, '巡检报告'), snapshotFixture(1, '每周发布')])

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))
    await screen.findByRole('button', { name: '回滚到版本 2' })
    fireEvent.change(screen.getByLabelText('搜索版本名称或编号'), {
      target: { value: '巡检' },
    })

    expect(screen.getByRole('button', { name: '回滚到版本 2' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '回滚到版本 1' })).not.toBeInTheDocument()
  })

  it('restores a snapshot over the live document', async () => {
    const restored = workflowFixture({ version: 4 })
    const onRestored = renderHistory([snapshotFixture(1, '首发')], restored)

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))
    fireEvent.click(await screen.findByRole('button', { name: '回滚到版本 1' }))
    expect(await screen.findByRole('heading', { name: '回滚到版本 1？' })).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: '回滚' }))

    await waitFor(() => expect(onRestored).toHaveBeenCalledWith(restored))
  })

  it('cancelling the confirmation leaves the document untouched', async () => {
    const onRestored = renderHistory([snapshotFixture(1, '首发')])

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))
    fireEvent.click(await screen.findByRole('button', { name: '回滚到版本 1' }))
    fireEvent.click(screen.getByRole('button', { name: '取消' }))

    expect(screen.queryByRole('heading', { name: '回滚到版本 1？' })).not.toBeInTheDocument()
    expect(onRestored).not.toHaveBeenCalled()
  })

  it('reports a rejected restore over the confirmation', async () => {
    const onRestored = renderHistory([snapshotFixture(1, '首发')], undefined, 409)

    fireEvent.click(screen.getByRole('button', { name: '版本历史' }))
    fireEvent.click(await screen.findByRole('button', { name: '回滚到版本 1' }))
    fireEvent.click(screen.getByRole('button', { name: '回滚' }))

    expect(await screen.findByText('已存在同名工作流。')).toBeInTheDocument()
    expect(onRestored).not.toHaveBeenCalled()
  })
})

describe('WorkflowVersionTools', () => {
  it('blocks the publish dialog when the draft could not be flushed', async () => {
    installHistory([snapshotFixture(1)])
    const flushDraft = vi.fn<() => Promise<boolean>>().mockResolvedValue(false)
    renderWithProviders(
      <WorkflowVersionTools
        tenantId={TEST_TENANT_ID}
        workflowId={WORKFLOW_ID}
        committedVersion={1}
        flushDraft={flushDraft}
        onRestored={vi.fn<(workflow: CloudWorkflow) => void>()}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    await waitFor(() => expect(flushDraft).toHaveBeenCalled())
    expect(screen.queryByRole('heading', { name: '发布版本' })).not.toBeInTheDocument()
  })

  it('opens the publish dialog once the draft is flushed', async () => {
    installHistory([snapshotFixture(1)])
    installPublish(() => {}, 200)
    renderWithProviders(
      <WorkflowVersionTools
        tenantId={TEST_TENANT_ID}
        workflowId={WORKFLOW_ID}
        committedVersion={1}
        flushDraft={vi.fn<() => Promise<boolean>>().mockResolvedValue(true)}
        onRestored={vi.fn<(workflow: CloudWorkflow) => void>()}
      />,
    )

    fireEvent.click(screen.getByRole('button', { name: '发布' }))

    expect(await screen.findByRole('heading', { name: '发布版本' })).toBeInTheDocument()
  })
})
