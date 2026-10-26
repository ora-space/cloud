import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it, vi } from 'vitest'
import { renderWithProviders } from '@/test/render'
import { server } from '@/test/msw-server'
import type {
  AssistSuggestion,
  CollaborationTargetSummary,
  FormDescriptor,
  IssueInteraction,
  IssueRun,
} from '@/features/issues/types'
import { WorkflowInteractionComposer } from './workflow-interaction-composer'

const target: CollaborationTargetSummary = {
  type: 'workflow',
  id: 'wf1',
  displayName: 'Security Review Workflow',
  description: 'demo',
  interactionDescriptor: { mode: 'form', requiresTask: false, formRef: 'security-review' },
}

/**
 * A descriptor with deliberately chosen keys: the renderer must draw exactly what it declares, which is
 * how we prove no workflow id is hard-coded in the frontend.
 */
const descriptor: FormDescriptor = {
  formRef: 'security-review',
  title: 'Security Review',
  description: 'fixture descriptor',
  fields: [
    { key: 'repository', label: 'Repository', type: 'text', required: true },
    {
      key: 'scope',
      label: 'Review scope',
      type: 'select',
      required: true,
      options: [
        { value: 'current-issue', label: 'Current issue changes' },
        { value: 'changed-files', label: 'Changed files' },
        { value: 'full-repo', label: 'Full repository' },
      ],
    },
    {
      key: 'severity',
      label: 'Severity',
      type: 'select',
      required: false,
      options: [
        { value: 'low', label: 'Low' },
        { value: 'high', label: 'High' },
      ],
    },
    { key: 'notes', label: 'Notes', type: 'textarea', required: false },
  ],
}

/**
 * The descriptor the backend projects once the form is opened *for an issue*: the platform's two
 * fields lead it, prefilled from the issue's project repository and the workflow's Start prompt. They
 * are ordinary fields as far as the renderer is concerned — that is the point of projecting them into
 * the descriptor rather than special-casing them in the UI.
 */
const platformDescriptor: FormDescriptor = {
  formRef: 'security-review',
  fields: [
    {
      key: 'repository',
      label: '仓库地址',
      type: 'text',
      required: true,
      defaultValue: 'https://github.com/ora/cloud',
    },
    {
      key: 'prompt',
      label: '提示词',
      type: 'textarea',
      required: false,
      defaultValue: 'Review the diff',
    },
    { key: 'notes', label: 'Notes', type: 'textarea', required: false },
  ],
}

/**
 * The same descriptor once the launch fields are added: a required branch, the workflow's published
 * versions to run, and the references this run may carry on top of the issue's own. They are ordinary
 * fields too — the only one the surface reads back out is `context_refs`.
 */
const launchDescriptor: FormDescriptor = {
  formRef: 'security-review',
  fields: [
    {
      key: 'repository',
      label: '仓库地址',
      type: 'text',
      required: true,
      defaultValue: 'https://github.com/ora/cloud',
    },
    { key: 'branch', label: '分支', type: 'text', required: true, defaultValue: 'main' },
    {
      key: 'snapshotId',
      label: '运行版本',
      type: 'select',
      required: false,
      defaultValue: 'snap-3',
      options: [
        { value: 'snap-3', label: 'v3 · 发布 3' },
        { value: 'snap-2', label: 'v2 · 发布 2' },
      ],
    },
    {
      key: 'prompt',
      label: '提示词',
      type: 'textarea',
      required: false,
      defaultValue: 'Review the diff',
    },
    {
      key: 'context_refs',
      label: '补充上下文引用',
      type: 'multi_select',
      required: false,
      options: [
        { value: 'project:p1', label: '本项目' },
        { value: 'parent_issue:p2', label: '父任务' },
      ],
    },
  ],
}

const suggestion: AssistSuggestion = {
  suggestedValues: { scope: 'changed-files', severity: 'high' },
  suggestedContextRefs: [],
  explanations: { scope: 'because scope', severity: 'because severity' },
}

/** A suggestion that proposes one reference the user can also tick in the form, to prove the dedup. */
const refSuggestion: AssistSuggestion = {
  suggestedValues: {},
  suggestedContextRefs: [{ refType: 'project', refId: 'p1' }],
  explanations: {},
}

const interaction: IssueInteraction = {
  id: 'ix1',
  tenantId: 't1',
  issueId: 'i1',
  commentId: 'c1',
  targetType: 'workflow',
  targetId: 'wf1',
  mode: 'form',
  task: '',
  runId: null,
  input: {},
  createdAt: '2026-01-01T00:00:00.000Z',
}

function run(): IssueRun {
  return {
    id: 'r1',
    tenantId: 't1',
    issueId: 'i1',
    executorType: 'workflow',
    executorId: 'wf1',
    input: {},
    status: 'queued',
    createdAt: '2026-01-01T00:00:00.000Z',
    updatedAt: '2026-01-01T00:00:00.000Z',
  }
}

/** Serves the descriptor plus assist/comment/interactions/confirm; returns spies on the mutations. */
function serveComposer(overrides: { descriptor?: unknown; suggestion?: AssistSuggestion } = {}) {
  const assist = vi.fn<(body: unknown) => void>()
  const comment = vi.fn<(body: unknown) => void>()
  const confirm = vi.fn<(body: unknown) => void>()
  server.use(
    http.get('/api/v1/tenants/t1/collaboration/forms/security-review', () =>
      HttpResponse.json(overrides.descriptor ?? descriptor),
    ),
    http.get('/api/v1/tenants/t1/workflows/wf1/snapshots', () =>
      HttpResponse.json({ items: [{ id: 'snap-3' }, { id: 'snap-2' }], nextCursor: '' }),
    ),
    // The composer opens on the platform's default version, so a form that selects one is read from
    // that version's snapshot rather than from the live descriptor.
    http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/snap-3/form', () =>
      HttpResponse.json({ ...launchDescriptor, snapshotId: 'snap-3' }),
    ),
    http.post('/api/v1/tenants/t1/issues/i1/collaboration/assist', async ({ request }) => {
      assist(await request.json())
      return HttpResponse.json(overrides.suggestion ?? suggestion)
    }),
    http.post('/api/v1/tenants/t1/issues/i1/comments', async ({ request }) => {
      comment(await request.json())
      return HttpResponse.json({ resource: { id: 'c1' } })
    }),
    http.get('/api/v1/tenants/t1/issues/i1/interactions', () =>
      HttpResponse.json({ items: [interaction], nextCursor: '' }),
    ),
    http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', async ({ request }) => {
      confirm(await request.json())
      return HttpResponse.json({ resource: run() })
    }),
  )
  return { assist, comment, confirm }
}

function renderComposer() {
  const onConfirmed = vi.fn<() => void>()
  const onRemove = vi.fn<() => void>()
  renderWithProviders(
    <WorkflowInteractionComposer
      slug="t1"
      issueId="i1"
      target={target}
      onConfirmed={onConfirmed}
      onRemove={onRemove}
    />,
  )
  return { onConfirmed, onRemove }
}

/** Anchors on a suggestion's explanation text to scope assertions to one suggestion row. */
function suggestionRow(explanation: string): HTMLElement {
  const node = screen.getByText(explanation).parentElement
  if (!node) throw new Error('suggestion row not found')
  return node
}

/** The Select trigger is labelled by its field label through the generated `workflow-field-<key>` id. */
function selectTrigger(label: string): HTMLElement {
  return screen.getByLabelText(new RegExp(label))
}

async function fillRequired(user: ReturnType<typeof userEvent.setup>) {
  await user.type(await screen.findByLabelText(/Repository/), 'ora-space/cloud')
  await user.click(selectTrigger('Review scope'))
  await user.click(await screen.findByRole('option', { name: 'Changed files' }))
}

/** The two clicks every confirm goes through: into Review, then the confirm itself. */
async function reviewAndConfirm(user: ReturnType<typeof userEvent.setup>) {
  await user.click(screen.getByRole('button', { name: '审阅' }))
  await user.click(await screen.findByRole('button', { name: '确认执行' }))
}

/** Renders the composer and confirms once against a server that refuses the submission. */
async function confirmIntoRejection(code: string, status: number) {
  server.use(
    http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', () =>
      HttpResponse.json({ code, params: { field: 'prompt' } }, { status }),
    ),
  )
  const user = userEvent.setup()
  const { onConfirmed } = renderComposer()
  await screen.findByLabelText(/提示词/)
  await reviewAndConfirm(user)
  return { user, onConfirmed }
}

function serveOldSnapshot() {
  const read = vi.fn<(issueId: string | null) => void>()
  server.use(
    http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/snap-2/form', ({ request }) => {
      read(new URL(request.url).searchParams.get('issueId'))
      return HttpResponse.json({
        ...launchDescriptor,
        snapshotId: 'snap-2',
        fields: [
          ...launchDescriptor.fields.filter((field) => field.key !== 'prompt'),
          {
            key: 'oldInput',
            label: 'Old input',
            type: 'text',
            required: true,
            defaultValue: 'old default',
          },
        ],
      })
    }),
  )
  return read
}

describe('WorkflowInteractionComposer', () => {
  it('replays a lost comment response with the same key and resolves an already confirmed interaction', async () => {
    const { confirm } = serveComposer()
    const keys: (string | null)[] = []
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', ({ request }) => {
        keys.push(request.headers.get('Idempotency-Key'))
        if (keys.length === 1) return HttpResponse.error()
        return HttpResponse.json({ resource: { id: 'c1' } })
      }),
      http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', () =>
        HttpResponse.json({ code: 'interaction_already_confirmed', params: {} }, { status: 409 }),
      ),
    )
    const user = userEvent.setup()
    const { onConfirmed } = renderComposer()
    await fillRequired(user)
    await reviewAndConfirm(user)
    await screen.findByText('确认失败，请重试。')
    await user.click(screen.getByRole('button', { name: '确认执行' }))
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledOnce())
    expect(keys).toHaveLength(2)
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBe(keys[0])
    expect(confirm).not.toHaveBeenCalled()
  })

  it.each([
    ['workflow_no_published_snapshot', '尚无已发布版本'],
    ['workflow_executor_unconfigured', '个人模型或执行器槽位尚未配置'],
    ['forbidden', '没有执行此工作流的权限'],
    ['not_found', '工作流、版本或任务已不可用'],
  ])('explains a %s refusal without discarding the form', async (code, message) => {
    serveComposer({ descriptor: launchDescriptor })
    const { onConfirmed } = await confirmIntoRejection(code, 409)
    expect(await screen.findByRole('alert')).toHaveTextContent(message)
    expect(onConfirmed).not.toHaveBeenCalled()
    expect(screen.getByRole('button', { name: '返回修改' })).toBeEnabled()
  })

  it('lets the user leave a forbidden form without creating an interaction', async () => {
    const { comment, confirm } = serveComposer()
    server.use(
      http.get('/api/v1/tenants/t1/collaboration/forms/security-review', () =>
        HttpResponse.json({ code: 'forbidden', params: {} }, { status: 403 }),
      ),
    )
    const { onRemove } = renderComposer()
    expect(await screen.findByRole('alert')).toHaveTextContent('没有执行此工作流的权限')
    await userEvent.setup().click(screen.getByRole('button', { name: '取消' }))
    expect(onRemove).toHaveBeenCalledOnce()
    expect(comment).not.toHaveBeenCalled()
    expect(confirm).not.toHaveBeenCalled()
  })
  it('renders exactly the fields the descriptor declares', async () => {
    serveComposer()
    renderComposer()

    expect(await screen.findByText('Security Review')).toBeInTheDocument()
    expect(screen.getByLabelText(/Repository/)).toBeInTheDocument()
    expect(screen.getByText('Review scope')).toBeInTheDocument()
    expect(screen.getByText('Notes')).toBeInTheDocument()
  })

  it('surfaces an unsupported field type instead of guessing at it', async () => {
    serveComposer({
      descriptor: {
        formRef: 'security-review',
        fields: [{ key: 'weird', label: 'Weird', type: 'date', required: false }],
      },
    })
    renderComposer()

    expect(await screen.findByText(/不支持的字段类型/)).toBeInTheDocument()
  })

  it('gates Review on the required fields', async () => {
    serveComposer()
    const user = userEvent.setup()
    renderComposer()

    const review = await screen.findByRole('button', { name: '审阅' })
    expect(review).toBeDisabled()
    await fillRequired(user)
    expect(review).toBeEnabled()
  })

  it('shows suggestions, applies one, ignores another, and writes nothing', async () => {
    const { assist, comment, confirm } = serveComposer()
    const user = userEvent.setup()
    renderComposer()

    await user.click(await screen.findByRole('button', { name: 'AI Assist' }))
    expect(await screen.findByText('because scope')).toBeInTheDocument()
    expect(assist).toHaveBeenCalledWith({ targetId: 'wf1', values: {} })

    await user.click(within(suggestionRow('because scope')).getByRole('button', { name: '应用' }))
    expect(await screen.findByText('Changed files')).toBeInTheDocument()
    await user.click(
      within(suggestionRow('because severity')).getByRole('button', { name: '忽略' }),
    )
    expect(screen.queryByText('because severity')).not.toBeInTheDocument()

    // A draft form writes nothing: no comment, no interaction, no run.
    expect(comment).not.toHaveBeenCalled()
    expect(confirm).not.toHaveBeenCalled()
  })

  it('writes nothing when the draft is abandoned', async () => {
    const { comment, confirm } = serveComposer()
    const user = userEvent.setup()
    const { onRemove } = renderComposer()

    await fillRequired(user)
    await user.click(screen.getByRole('button', { name: '取消' }))

    expect(comment).not.toHaveBeenCalled()
    expect(confirm).not.toHaveBeenCalled()
    expect(onRemove).toHaveBeenCalled()
  })

  it('prefills the platform fields and gates Review on the repository alone', async () => {
    const { confirm } = serveComposer({ descriptor: platformDescriptor })
    const user = userEvent.setup()
    renderComposer()

    const repository = await screen.findByLabelText(/仓库地址/)
    expect(repository).toHaveValue('https://github.com/ora/cloud')
    const prompt = screen.getByLabelText(/提示词/)
    expect(prompt).toHaveValue('Review the diff')

    // The prompt is optional: clearing it is how the run says "use the workflow's own Start prompt".
    await user.clear(prompt)
    expect(screen.getByRole('button', { name: '审阅' })).toBeEnabled()

    // The repository is not optional, and an issue whose project has no repository arrives empty.
    await user.clear(repository)
    expect(screen.getByRole('button', { name: '审阅' })).toBeDisabled()
    await user.type(repository, 'ora-space/cloud')

    await reviewAndConfirm(user)

    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: { repository: 'ora-space/cloud', prompt: '' },
      }),
    )
  })

  it('requires an explicit Review, and Confirm creates the comment + interaction + run once', async () => {
    const { comment, confirm } = serveComposer()
    const user = userEvent.setup()
    const { onConfirmed } = renderComposer()

    await fillRequired(user)
    expect(confirm).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: '审阅' }))
    expect(await screen.findByText('确认后才会创建执行（IssueRun）')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: '返回修改' }))
    expect(await screen.findByRole('button', { name: '审阅' })).toBeInTheDocument()

    await reviewAndConfirm(user)

    await waitFor(() => expect(comment).toHaveBeenCalledTimes(1))
    expect(comment).toHaveBeenCalledWith({
      body: '@Security Review Workflow',
      targets: [{ type: 'workflow', id: 'wf1' }],
    })
    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: { repository: 'ora-space/cloud', scope: 'changed-files' },
      }),
    )
    await waitFor(() => expect(onConfirmed).toHaveBeenCalled())
  })

  it('opens on the default published version by reading that version, so rendering and validation match', async () => {
    const { confirm } = serveComposer({ descriptor: launchDescriptor })
    const read = vi.fn<(issueId: string | null) => void>()
    server.use(
      http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/snap-3/form', ({ request }) => {
        read(new URL(request.url).searchParams.get('issueId'))
        return HttpResponse.json({
          ...launchDescriptor,
          snapshotId: 'snap-3',
          fields: [
            ...launchDescriptor.fields,
            {
              key: 'v3Only',
              label: 'V3 only',
              type: 'text',
              required: true,
              defaultValue: 'frozen v3',
            },
          ],
        })
      }),
    )
    const user = userEvent.setup()
    const { onConfirmed } = renderComposer()

    // The live descriptor has no such field: seeing it proves the form is the snapshot's own, and the
    // confirm submits exactly the fields this version asked for.
    expect(await screen.findByLabelText(/V3 only/)).toHaveValue('frozen v3')
    expect(selectTrigger('运行版本')).toHaveTextContent('v3 · 发布 3')
    expect(read).toHaveBeenCalledWith('i1')

    await reviewAndConfirm(user)
    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: {
          repository: 'https://github.com/ora/cloud',
          branch: 'main',
          snapshotId: 'snap-3',
          prompt: 'Review the diff',
          v3Only: 'frozen v3',
        },
      }),
    )
    await waitFor(() => expect(onConfirmed).toHaveBeenCalled())
  })

  it('names the rejected field when a confirm fails validation', async () => {
    serveComposer({ descriptor: launchDescriptor })
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', () =>
        HttpResponse.json(
          { code: 'invalid_field_value', params: { field: 'branch' } },
          { status: 400 },
        ),
      ),
    )
    const user = userEvent.setup()
    renderComposer()
    await screen.findByLabelText(/分支/)
    await reviewAndConfirm(user)

    expect(await screen.findByText('字段「分支」的取值不合法，请修改后重试。')).toBeInTheDocument()
  })

  it('prefills the branch and the newest published version, and gates Review on the branch too', async () => {
    const { confirm } = serveComposer({ descriptor: launchDescriptor })
    const user = userEvent.setup()
    renderComposer()

    const branch = await screen.findByLabelText(/分支/)
    expect(branch).toHaveValue('main')
    // The version opens on the newest published snapshot, shown by its version number.
    expect(selectTrigger('运行版本')).toHaveTextContent('v3 · 发布 3')

    // The branch pairs with the repository: clearing it gates Review exactly as clearing that does.
    await user.clear(branch)
    expect(screen.getByRole('button', { name: '审阅' })).toBeDisabled()
    await user.type(branch, 'release/1.0')

    await reviewAndConfirm(user)

    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: {
          repository: 'https://github.com/ora/cloud',
          branch: 'release/1.0',
          snapshotId: 'snap-3',
          prompt: 'Review the diff',
        },
      }),
    )
  })

  it('locks both confirm clicks while the first interaction is still being created', async () => {
    const { comment, confirm } = serveComposer({ descriptor: platformDescriptor })
    let release = vi.fn<() => void>()
    const gate = new Promise<void>((resolve) => {
      release = vi.fn<() => void>(resolve)
    })
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', async ({ request }) => {
        comment(await request.json())
        await gate
        return HttpResponse.json({ resource: { id: 'c1' } })
      }),
    )
    const user = userEvent.setup()
    const { onConfirmed } = renderComposer()
    await user.click(await screen.findByRole('button', { name: '审阅' }))
    const button = screen.getByRole('button', { name: '确认执行' })
    fireEvent.click(button)
    fireEvent.click(button)

    try {
      await waitFor(() => expect(comment).toHaveBeenCalledTimes(1))
      expect(screen.getByRole('button', { name: '确认中…' })).toBeDisabled()
      expect(screen.getByRole('button', { name: '返回修改' })).toBeDisabled()
      expect(screen.getByRole('button', { name: '取消' })).toBeDisabled()
      expect(confirm).not.toHaveBeenCalled()
    } finally {
      release()
    }
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
    expect(comment).toHaveBeenCalledTimes(1)
    expect(confirm).toHaveBeenCalledTimes(1)
  })

  it('reuses its interaction after a rejected confirm, including edits before retry', async () => {
    const { comment, confirm } = serveComposer({ descriptor: platformDescriptor })
    let attempts = 0
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', async ({ request }) => {
        confirm(await request.json())
        attempts += 1
        return attempts === 1
          ? HttpResponse.json({ code: 'invalid_input' }, { status: 400 })
          : HttpResponse.json({ resource: run() })
      }),
    )
    const user = userEvent.setup()
    const { onConfirmed } = renderComposer()
    await screen.findByLabelText(/仓库地址/)
    await reviewAndConfirm(user)
    expect(await screen.findByText('确认失败，请重试。')).toBeInTheDocument()
    expect(onConfirmed).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: '返回修改' }))
    await user.type(screen.getByLabelText('Notes'), 'retry notes')
    await reviewAndConfirm(user)

    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
    expect(comment).toHaveBeenCalledTimes(1)
    expect(confirm).toHaveBeenCalledTimes(2)
    expect(confirm).toHaveBeenLastCalledWith({
      values: {
        repository: 'https://github.com/ora/cloud',
        prompt: 'Review the diff',
        notes: 'retry notes',
      },
    })
  })

  it('shows interaction creation failures and unlocks the form for a retry', async () => {
    serveComposer({ descriptor: platformDescriptor })
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/comments', () =>
        HttpResponse.json({ code: 'unavailable' }, { status: 503 }),
      ),
    )
    const user = userEvent.setup()
    renderComposer()
    await screen.findByLabelText(/仓库地址/)
    await reviewAndConfirm(user)

    expect(await screen.findByText('确认失败，请重试。')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '确认执行' })).toBeEnabled()
    expect(screen.getByRole('button', { name: '返回修改' })).toBeEnabled()
  })

  it('carries the ticked context references, once each even when Assist suggested one too', async () => {
    const { confirm } = serveComposer({
      descriptor: launchDescriptor,
      suggestion: refSuggestion,
    })
    const user = userEvent.setup()
    renderComposer()

    // Assist suggests the project reference, which the user applies and then also ticks below: the run
    // must receive it once, because it appends what it is given without checking.
    await user.click(await screen.findByRole('button', { name: 'AI Assist' }))
    await user.click(await screen.findByRole('button', { name: '应用' }))
    expect(await screen.findByText('已应用')).toBeInTheDocument()

    await user.click(screen.getByRole('checkbox', { name: '本项目' }))
    await user.click(screen.getByRole('checkbox', { name: '父任务' }))

    await reviewAndConfirm(user)

    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: {
          repository: 'https://github.com/ora/cloud',
          branch: 'main',
          snapshotId: 'snap-3',
          prompt: 'Review the diff',
          context_refs: ['project:p1', 'parent_issue:p2'],
        },
        contextRefs: [
          { refType: 'project', refId: 'p1' },
          { refType: 'parent_issue', refId: 'p2' },
        ],
      }),
    )
  })

  it('renders the selected snapshot defaults and retains its echo instead of the newest default', async () => {
    const { confirm } = serveComposer({ descriptor: launchDescriptor })
    const read = serveOldSnapshot()
    const user = userEvent.setup()
    renderComposer()
    await user.type(await screen.findByLabelText(/提示词/), ' obsolete draft')
    await user.click(selectTrigger('运行版本'))
    await user.click(await screen.findByRole('option', { name: 'v2 · 发布 2' }))

    expect(await screen.findByLabelText(/Old input/)).toHaveValue('old default')
    expect(screen.queryByLabelText(/提示词/)).not.toBeInTheDocument()
    expect(selectTrigger('运行版本')).toHaveTextContent('v2 · 发布 2')
    expect(read).toHaveBeenCalledWith('i1')
    await reviewAndConfirm(user)
    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({
        values: {
          repository: 'https://github.com/ora/cloud',
          branch: 'main',
          snapshotId: 'snap-2',
          oldInput: 'old default',
        },
      }),
    )
  })

  it('blocks stale review while a selected snapshot loads and after a read failure', async () => {
    serveComposer({ descriptor: launchDescriptor })
    let release = vi.fn<() => void>()
    const gate = new Promise<void>((resolve) => {
      release = vi.fn<() => void>(resolve)
    })
    server.use(
      http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/snap-2/form', async () => {
        await gate
        return HttpResponse.json({ code: 'not_found' }, { status: 404 })
      }),
    )
    const user = userEvent.setup()
    renderComposer()
    await user.click(await screen.findByLabelText(/运行版本/))
    await user.click(await screen.findByRole('option', { name: 'v2 · 发布 2' }))
    try {
      expect(screen.queryByLabelText(/提示词/)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: '审阅' })).not.toBeInTheDocument()
    } finally {
      release()
    }
    expect(await screen.findByText(/无法加载工作流表单/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '审阅' })).not.toBeInTheDocument()
    const read = serveOldSnapshot()
    await user.click(screen.getByRole('button', { name: '重试加载表单' }))
    expect(await screen.findByLabelText(/Old input/)).toHaveValue('old default')
    expect(read).toHaveBeenCalledTimes(1)
  })

  it('keeps an authors text variable named snapshotId as ordinary input', async () => {
    const { confirm } = serveComposer({
      descriptor: {
        formRef: 'security-review',
        fields: [
          { key: 'snapshotId', label: 'Author snapshot name', type: 'text', required: true },
        ],
      },
    })
    const user = userEvent.setup()
    renderComposer()
    await user.type(await screen.findByLabelText(/Author snapshot name/), 'my snapshot')
    await reviewAndConfirm(user)
    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({ values: { snapshotId: 'my snapshot' } }),
    )
  })

  it('keeps an authors select named snapshotId as ordinary input when any option is not published', async () => {
    const { confirm } = serveComposer({
      descriptor: {
        formRef: 'security-review',
        fields: [
          {
            key: 'snapshotId',
            label: 'Author snapshot choice',
            type: 'select',
            required: true,
            defaultValue: 'snap-3',
            options: [
              { value: 'snap-3', label: 'Author default' },
              { value: 'custom-choice', label: 'Author custom choice' },
            ],
          },
        ],
      },
    })
    const formRead = vi.fn<() => void>()
    server.use(
      http.get('/api/v1/tenants/t1/workflows/wf1/snapshots/:id/form', () => {
        formRead()
        return HttpResponse.json({ code: 'not_found' }, { status: 404 })
      }),
    )
    const user = userEvent.setup()
    renderComposer()
    await user.click(await screen.findByLabelText(/Author snapshot choice/))
    await user.click(await screen.findByRole('option', { name: 'Author custom choice' }))
    expect(selectTrigger('Author snapshot choice')).toHaveTextContent('Author custom choice')
    await reviewAndConfirm(user)
    await waitFor(() =>
      expect(confirm).toHaveBeenCalledWith({ values: { snapshotId: 'custom-choice' } }),
    )
    expect(formRead).not.toHaveBeenCalled()
  })

  it('keeps the same interaction when a rejected confirm is retried after switching snapshot', async () => {
    const { comment, confirm } = serveComposer({ descriptor: launchDescriptor })
    serveOldSnapshot()
    const { user, onConfirmed } = await confirmIntoRejection('required_field_missing', 400)
    await screen.findByText('请填写必填项：提示词')
    await user.click(screen.getByRole('button', { name: '返回修改' }))
    await user.click(selectTrigger('运行版本'))
    await user.click(await screen.findByRole('option', { name: 'v2 · 发布 2' }))
    await screen.findByLabelText(/Old input/)
    server.use(
      http.post('/api/v1/tenants/t1/issues/i1/interactions/ix1/confirm', async ({ request }) => {
        confirm(await request.json())
        return HttpResponse.json({ resource: run() })
      }),
    )
    await reviewAndConfirm(user)
    await waitFor(() => expect(onConfirmed).toHaveBeenCalledTimes(1))
    expect(comment).toHaveBeenCalledTimes(1)
    expect(confirm).toHaveBeenCalledWith(
      expect.objectContaining({ values: expect.objectContaining({ snapshotId: 'snap-2' }) }),
    )
  })
})
