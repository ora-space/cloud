import { fireEvent, screen } from '@testing-library/react'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { WorkflowHistoryControls } from '@/features/workflows/editor/use-workflow-editor-state'
import { WorkflowHistoryTools } from '@/features/workflows/editor/workflow-history-controls'
import { workflowHistoryFingerprint } from '@/features/workflows/editor/workflow-history'
import type {
  WorkflowHistoryEvent,
  WorkflowHistorySnapshot,
  WorkflowHistoryStep,
} from '@/features/workflows/editor/workflow-history'
import { installCloudSpaceHandlers } from '@/test/cloud-handlers'
import { renderWithProviders } from '@/test/render'

/** A distinct frozen state so step fingerprints never collide. */
function snapshotWith(label: string): WorkflowHistorySnapshot {
  return {
    nodes: [],
    edges: [],
    annotations: [],
    globalVariables: [],
    launchFields: [],
    description: label,
  }
}

/** A history step carrying exactly the given label and subject. */
function step(
  id: string,
  event: WorkflowHistoryEvent,
  subject: string | undefined,
  snapshot: WorkflowHistorySnapshot,
): WorkflowHistoryStep {
  return {
    id,
    event,
    snapshot,
    fingerprint: workflowHistoryFingerprint(snapshot),
    ...(subject === undefined ? {} : { meta: { subject } }),
  }
}

/** A history surface driven entirely by spies the test installs. */
function controls(overrides: Partial<WorkflowHistoryControls> = {}): WorkflowHistoryControls {
  return {
    canUndo: true,
    canRedo: true,
    past: [],
    future: [],
    currentEvent: null,
    currentMeta: undefined,
    undo: vi.fn<() => void>(),
    redo: vi.fn<() => void>(),
    jump: vi.fn<(direction: 'past' | 'future', steps: number) => void>(),
    clear: vi.fn<() => void>(),
    ...overrides,
  }
}

/** Mounts the toolbar with the given history surface, returning its spies. */
function renderTools(history: WorkflowHistoryControls) {
  return renderWithProviders(<WorkflowHistoryTools history={history} />)
}

/**
 * Opens the change-history popover from the toolbar's history button.
 *
 * The control reads 「编辑历史」 rather than 「历史记录」: a workflow also carries a run history and
 * a version list, and this is the session's edits — the one that is neither of those.
 */
async function openPanel(): Promise<void> {
  fireEvent.click(screen.getByRole('button', { name: '编辑历史' }))
  await screen.findByRole('dialog', { hidden: true })
}

beforeEach(() => {
  // The toolbar mounts no page, but `renderWithProviders` still resolves the
  // current space, so the session trios must answer before anything asks.
  installCloudSpaceHandlers('admin')
})

describe('WorkflowHistoryTools', () => {
  it('renders undo and redo following the history gates and fires the actions', () => {
    const undo = vi.fn<() => void>()
    const redo = vi.fn<() => void>()
    renderTools(controls({ canUndo: true, canRedo: false, undo, redo }))

    const undoButton = screen.getByRole('button', { name: '撤销' })
    const redoButton = screen.getByRole('button', { name: '重做' })
    expect(undoButton).toBeEnabled()
    expect(redoButton).toBeDisabled()

    fireEvent.click(undoButton)
    expect(undo).toHaveBeenCalledOnce()
    expect(redo).not.toHaveBeenCalled()
  })

  it('undo and redo also answer the canvas shortcuts', () => {
    const undo = vi.fn<() => void>()
    const redo = vi.fn<() => void>()
    renderTools(controls({ undo, redo }))

    fireEvent.keyDown(window, { key: 'z', ctrlKey: true })
    expect(undo).toHaveBeenCalledOnce()

    fireEvent.keyDown(window, { key: 'z', ctrlKey: true, shiftKey: true })
    expect(redo).toHaveBeenCalledOnce()

    fireEvent.keyDown(window, { key: 'y', ctrlKey: true })
    expect(redo).toHaveBeenCalledTimes(2)
  })

  it('leaves typing targets to the browser native undo', () => {
    const undo = vi.fn<() => void>()
    const redo = vi.fn<() => void>()
    renderWithProviders(
      <div>
        <WorkflowHistoryTools history={controls({ undo, redo })} />
        <input aria-label="field" />
      </div>,
    )

    const field = screen.getByLabelText('field')
    fireEvent.keyDown(field, { key: 'z', ctrlKey: true })
    expect(undo).not.toHaveBeenCalled()
    fireEvent.keyDown(field, { key: 'y', ctrlKey: true })
    expect(redo).not.toHaveBeenCalled()
  })

  it('shows the empty state when no step exists yet', async () => {
    renderTools(controls({ canUndo: false, canRedo: false, currentEvent: null }))

    await openPanel()

    expect(screen.getByText('还没有可回退的操作。')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: '清空历史' })).not.toBeInTheDocument()
  })

  it('clears the whole session from the bottom of the panel', async () => {
    const clear = vi.fn<() => void>()
    renderTools(controls({ clear, past: [step('a', 'node.add', 'Agent 新', snapshotWith('a'))] }))

    await openPanel()

    fireEvent.click(screen.getByRole('button', { name: '清空历史' }))
    expect(clear).toHaveBeenCalledOnce()
  })

  it('lists rows newest first and jumps toward the one picked', async () => {
    const jump = vi.fn<(direction: 'past' | 'future', steps: number) => void>()
    const past = [
      step('a', 'node.add', 'Agent 新', snapshotWith('before-add')),
      step('b', 'node.move', 'Agent 新', snapshotWith('before-move')),
    ]
    const future = [step('f', 'edge.connect', 'A → B', snapshotWith('before-connect'))]
    renderTools(
      controls({
        past,
        future,
        currentEvent: 'node.edit',
        currentMeta: { subject: 'Agent 新' },
        jump,
      }),
    )

    await openPanel()

    // The oldest past row is labeled by the session baseline, not an edit.
    fireEvent.click(screen.getByRole('button', { name: /会话开始.*回溯 2 步/ }))
    expect(jump).toHaveBeenLastCalledWith('past', 2)

    // A past row shows the edit that preceded it, and one more step back.
    fireEvent.click(screen.getByRole('button', { name: /添加节点.*回溯 1 步/ }))
    expect(jump).toHaveBeenLastCalledWith('past', 1)

    // The current row carries the current edit and is not jumpable.
    const currentRow = screen.getByRole('button', { name: /编辑节点.*当前状态/ })
    expect(currentRow).toBeDisabled()

    // A future row restores a step the user had already undone.
    fireEvent.click(screen.getByRole('button', { name: /建立连线.*前移 1 步/ }))
    expect(jump).toHaveBeenLastCalledWith('future', 1)
  })
})
