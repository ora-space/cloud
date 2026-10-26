import { fireEvent, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import type { Project, WorkflowRun, WorkflowSnapshot } from '@/api/generated.schemas'
import { WorkflowRunDialog } from '@/features/workflows/editor/workflow-run-dialog'
import {
  TEST_PROJECT_ID,
  TEST_TENANT_ID,
  installCloudSpaceHandlers,
  chooseProject,
  installSpaceProjectHandlers,
  projectFixture,
} from '@/test/cloud-handlers'
import { server } from '@/test/msw-server'
import { renderWithProviders } from '@/test/render'

const WORKFLOW_ID = '44444444-4444-4444-4444-444444444444'

/** One Start variable as the editor writes it into the graph. */
interface StartVariable {
  name: string
  displayName?: string
  fieldType?: string
  valueType: string
  required?: boolean
  options?: string[]
  value?: unknown
}

/** A published version whose Start node declares `variables`. */
function snapshotOf(options: {
  id: string
  version: number
  variables?: StartVariable[]
  prompt?: string
}): WorkflowSnapshot {
  return {
    id: options.id,
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    name: `版本 ${options.version}`,
    version: options.version,
    graph: {
      nodes: [
        {
          id: 'start',
          type: 'workflow',
          position: { x: 0, y: 0 },
          data: {
            kind: 'start',
            title: 'Start',
            description: '',
            ...(options.prompt === undefined ? {} : { input: options.prompt }),
            ...(options.variables === undefined ? {} : { inputVariables: options.variables }),
          },
        },
      ],
      edges: [],
      viewport: { x: 0, y: 0, zoom: 1 },
    },
    createdAt: '2026-09-20T10:00:00+08:00',
  }
}

/** A created run row, so the success path has something to hand back. */
function runFixture(): WorkflowRun {
  return {
    id: 'run-new',
    tenantId: TEST_TENANT_ID,
    workflowId: WORKFLOW_ID,
    snapshotId: 'snap-2',
    name: '巡检运行',
    workflowName: '安全审查',
    status: 'pending',
    input: {},
    nodeStates: {},
    rounds: [],
    error: '',
    definitionSnapshot: null,
    cancelRequestedAt: null,
    pluginVersion: '1.0.0',
    startedAt: null,
    finishedAt: null,
    createdAt: '2026-09-20T10:00:00+08:00',
    updatedAt: '2026-09-20T10:00:00+08:00',
  }
}

interface DialogRequests {
  bodies: unknown[]
  keys: Array<string | null>
}

/**
 * Serves the two endpoints the dialog reads, recording what the create request carried.
 *
 * The response is whatever `respond` returns, so one helper covers the accepted start, a refusal
 * the member has to act on, and a transport failure a retry follows.
 */
function installDialogHandlers(
  snapshots: WorkflowSnapshot[],
  respond: () => Response = () => HttpResponse.json({ resource: runFixture() }),
): DialogRequests {
  const requests: DialogRequests = { bodies: [], keys: [] }
  server.use(
    http.get(`/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/snapshots`, () =>
      HttpResponse.json({ items: snapshots, nextCursor: '' }),
    ),
    http.post(
      `/api/v1/tenants/${TEST_TENANT_ID}/workflows/${WORKFLOW_ID}/runs`,
      async ({ request }) => {
        requests.bodies.push(await request.json())
        requests.keys.push(request.headers.get('Idempotency-Key'))
        return respond()
      },
    ),
  )
  return requests
}

/**
 * Mounts the dialog open in the `cloud-dev` space, with the started-run callback under the caller's
 * control.
 *
 * The versions and the space's Projects arrive over the network from whatever the handlers were
 * given, so mounting takes no lists of its own; `projects` is the space's own list, which a test
 * empties to exercise the guidance a Project-less space shows.
 */
function renderDialog(
  onStarted = vi.fn<(run: WorkflowRun) => void>(),
  projects: Project[] = [projectFixture()],
) {
  installCloudSpaceHandlers('admin')
  installSpaceProjectHandlers(projects)
  renderWithProviders(
    <WorkflowRunDialog
      tenantId={TEST_TENANT_ID}
      workflowId={WORKFLOW_ID}
      open
      onOpenChange={() => {}}
      onStarted={onStarted}
    />,
    { slug: 'cloud-dev' },
  )
  return onStarted
}

/** The start button, which is the dialog's own submit rather than the toolbar's. */
function startButton(): HTMLElement {
  return screen.getByRole('button', { name: '开始运行' })
}

/**
 * The start button, once the version list has resolved and the form is mounted.
 *
 * The dialog reads the published versions over the network, so on the first tick it has nothing to
 * offer and renders no submit at all.
 */
function findStartButton(): Promise<HTMLElement> {
  return screen.findByRole('button', { name: '开始运行' })
}

describe('WorkflowRunDialog', () => {
  it('opens on the newest version and prefills the defaults it declares', async () => {
    const versions = [
      snapshotOf({
        id: 'snap-2',
        version: 2,
        prompt: '总结这次发布',
        variables: [
          { name: 'topic', fieldType: 'text-input', valueType: 'string', value: 'release' },
        ],
      }),
      snapshotOf({ id: 'snap-1', version: 1 }),
    ]
    installDialogHandlers(versions)
    renderDialog()

    // The version selector opens on the newest version, and the fields it asks for are that
    // version's own — the prompt it declares plus the variable, already holding its default.
    expect(await screen.findByText('v2 · 版本 2')).toBeInTheDocument()
    expect(screen.getByDisplayValue('总结这次发布')).toBeInTheDocument()
    expect(screen.getByDisplayValue('release')).toBeInTheDocument()
  })

  it('labels the platform’s own field in the reader’s language', async () => {
    installDialogHandlers([snapshotOf({ id: 'snap-2', version: 2, prompt: '总结这次发布' })])
    renderDialog()

    // The projection names the prompt field by its translation key, because the platform owns that
    // wording and the runtime layer does not know what language the member reads. The surface is
    // what turns the key into text — the same key the editor's launch-field table shows.
    expect(await screen.findByLabelText(/^提示词/)).toBeInTheDocument()
    expect(screen.queryByLabelText(/^prompt/)).not.toBeInTheDocument()
  })

  it('sends the selected version and the answers read from it', async () => {
    const versions = [
      snapshotOf({
        id: 'snap-2',
        version: 2,
        variables: [{ name: 'topic', fieldType: 'text-input', valueType: 'string' }],
      }),
    ]
    const requests = installDialogHandlers(versions)
    const onStarted = renderDialog()

    await chooseProject()
    fireEvent.change(await screen.findByLabelText(/^topic/), { target: { value: 'release' } })
    fireEvent.click(startButton())

    await waitFor(() => expect(onStarted).toHaveBeenCalledTimes(1))
    expect(requests.bodies).toEqual([
      { projectId: TEST_PROJECT_ID, snapshotId: 'snap-2', input: { topic: 'release' } },
    ])
  })

  it('asks the switched-to version’s fields and sends that version', async () => {
    const versions = [
      snapshotOf({
        id: 'snap-2',
        version: 2,
        variables: [{ name: 'topic', fieldType: 'text-input', valueType: 'string' }],
      }),
      snapshotOf({ id: 'snap-1', version: 1 }),
    ]
    const requests = installDialogHandlers(versions)
    renderDialog()

    await chooseProject()
    // The version list is a listbox, not a native `<select>`: only real pointer events move it.
    const user = userEvent.setup()
    await user.click(await screen.findByRole('combobox', { name: '版本' }))
    await user.click(await screen.findByRole('option', { name: 'v1 · 版本 1' }))

    // The older version declares no variable, so the question is gone rather than carried over.
    await waitFor(() => expect(screen.queryByLabelText(/^topic/)).not.toBeInTheDocument())
    fireEvent.click(startButton())

    await waitFor(() => expect(requests.bodies).toHaveLength(1))
    // The Project belongs to the run rather than to the version, so switching versions keeps it.
    expect(requests.bodies).toEqual([
      { projectId: TEST_PROJECT_ID, snapshotId: 'snap-1', input: {} },
    ])
  })

  it('refuses to start while a required answer is missing', async () => {
    const versions = [
      snapshotOf({
        id: 'snap-2',
        version: 2,
        variables: [
          { name: 'topic', fieldType: 'text-input', valueType: 'string', required: true },
        ],
      }),
    ]
    const requests = installDialogHandlers(versions)
    renderDialog()

    await chooseProject()
    expect(await screen.findByText('还需要填写：topic')).toBeInTheDocument()
    expect(startButton()).toBeDisabled()

    fireEvent.change(screen.getByLabelText(/^topic/), { target: { value: 'release' } })

    await waitFor(() => expect(startButton()).toBeEnabled())
    expect(screen.queryByText('还需要填写：topic')).not.toBeInTheDocument()
    expect(requests.bodies).toEqual([])
  })

  it('keeps Start disabled until a Project is chosen', async () => {
    const requests = installDialogHandlers([snapshotOf({ id: 'snap-2', version: 2 })])
    renderDialog()

    // The version resolves and asks nothing, so the Project is the only thing still missing.
    expect(await findStartButton()).toBeDisabled()
    expect(
      await screen.findByText('先选一个项目：运行的工作区建在这个项目里。'),
    ).toBeInTheDocument()
    expect(requests.bodies).toEqual([])

    await chooseProject()

    await waitFor(() => expect(startButton()).toBeEnabled())
    expect(screen.queryByText('先选一个项目：运行的工作区建在这个项目里。')).not.toBeInTheDocument()
  })

  it('says the space has no Project rather than offering an empty picker', async () => {
    installDialogHandlers([snapshotOf({ id: 'snap-2', version: 2 })])
    renderDialog(undefined, [])

    // An empty list is not a choice: the member needs to know creating a Project is the next step,
    // not stare at a disabled button with no reason why.
    expect(
      await screen.findByText('这个空间还没有项目，先创建一个项目再运行。'),
    ).toBeInTheDocument()
    expect(screen.queryByRole('combobox', { name: '项目' })).not.toBeInTheDocument()
    expect(startButton()).toBeDisabled()
  })

  it('reports the server’s refusal on the form and starts nothing', async () => {
    installDialogHandlers([snapshotOf({ id: 'snap-2', version: 2 })], () =>
      HttpResponse.json({ code: 'required_field_missing' }, { status: 400 }),
    )
    const onStarted = renderDialog()
    await chooseProject()

    fireEvent.click(await findStartButton())

    expect(await screen.findByText('还有必填项没有填写，请补齐后再运行。')).toBeInTheDocument()
    // A refusal is not a success: the dialog stays open and nothing claims a run exists.
    expect(startButton()).toBeInTheDocument()
    expect(onStarted).not.toHaveBeenCalled()
  })

  it('retries a failed start under the same idempotency key', async () => {
    let attempt = 0
    const requests = installDialogHandlers([snapshotOf({ id: 'snap-2', version: 2 })], () => {
      attempt += 1
      return attempt === 1
        ? HttpResponse.json({ code: 'internal_error' }, { status: 500 })
        : HttpResponse.json({ resource: runFixture() })
    })
    const onStarted = renderDialog()
    await chooseProject()

    fireEvent.click(await findStartButton())
    await waitFor(() => expect(requests.bodies).toHaveLength(1))
    fireEvent.click(startButton())
    await waitFor(() => expect(onStarted).toHaveBeenCalledTimes(1))

    // Both attempts are the same logical start, so the server can recognise the replay instead of
    // creating a second run.
    const body = { projectId: TEST_PROJECT_ID, snapshotId: 'snap-2', input: {} }
    expect(requests.bodies).toEqual([body, body])
    expect(requests.keys[0]).not.toBeNull()
    expect(requests.keys[1]).toBe(requests.keys[0])
  })
})

describe('WorkflowRunDialog without a published version', () => {
  it('says there is nothing to run rather than offering an empty form', async () => {
    installDialogHandlers([])
    renderDialog()

    expect(await screen.findByText('这个工作流还没有已发布的版本，无法运行。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '开始运行' })).not.toBeInTheDocument()
  })
})
