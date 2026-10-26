import { describe, expect, it } from 'vitest'
import { isNodeWorking, runStatusTone } from '@/features/workflows/runtime/run-status-style'
import {
  isTerminalRunStatus,
  type GraphWorkflowNodeStatus,
  type GraphWorkflowRunStatus,
} from '@/features/workflows/runtime/types'

describe('isNodeWorking', () => {
  it('marks executing and retrying statuses as live', () => {
    expect(isNodeWorking('running')).toBe(true)
    expect(isNodeWorking('retrying')).toBe(true)
  })

  it('is quiet for every awaited or settled status', () => {
    const quiet = ['succeeded', 'failed', 'cancelled', 'pending', 'idle', 'inactive'] as const
    for (const status of quiet) {
      expect(isNodeWorking(status)).toBe(false)
    }
  })
})

describe('runStatusTone', () => {
  it('maps every known status to its own label key and a non-empty surface', () => {
    const expectedKeys: ReadonlyArray<[GraphWorkflowRunStatus | GraphWorkflowNodeStatus, string]> =
      [
        ['pending', 'workflows.run.status.pending'],
        ['running', 'workflows.run.status.running'],
        ['retrying', 'workflows.run.status.retrying'],
        ['succeeded', 'workflows.run.status.succeeded'],
        ['failed', 'workflows.run.status.failed'],
        ['cancelled', 'workflows.run.status.cancelled'],
        ['idle', 'workflows.run.nodeStatus.idle'],
        ['inactive', 'workflows.run.nodeStatus.inactive'],
      ]
    for (const [status, labelKey] of expectedKeys) {
      const tone = runStatusTone(status)
      expect(tone.labelKey).toBe(labelKey)
      expect(tone.dot).not.toBe('')
      expect(tone.badge).not.toBe('')
    }
  })

  it('paints an unknown future status flat rather than inventing colour', () => {
    const tone = runStatusTone(unknownServerStatus('on-hold'))
    expect(tone.labelKey).toBe('workflows.run.nodeStatus.inactive')
    expect(tone.dot).toBe('bg-muted-foreground/20')
  })
})

/**
 * Brings any server payload across as a typed status, so `runStatusTone` lands
 * in its default branch.
 *
 * A run's status reaches the viewer as an untyped string from the backend; the
 * `typeof` guard is the honest boundary it crosses there. The declared return
 * type keeps the call legal while the runtime value stays whatever the payload
 * really was — today a string the switch does not yet know.
 */
function unknownServerStatus(payload: string): GraphWorkflowRunStatus | GraphWorkflowNodeStatus {
  if (isStatusString(payload)) {
    return payload
  }
  return 'idle'
}

/** Grammar-level narrowing: any payload string is declared a status by the wire perimeter. */
function isStatusString(
  payload: string,
): payload is GraphWorkflowRunStatus | GraphWorkflowNodeStatus {
  return typeof payload === 'string'
}

describe('isTerminalRunStatus', () => {
  it('is final only for the three completed statuses', () => {
    expect(isTerminalRunStatus('succeeded')).toBe(true)
    expect(isTerminalRunStatus('failed')).toBe(true)
    expect(isTerminalRunStatus('cancelled')).toBe(true)
    expect(isTerminalRunStatus('pending')).toBe(false)
    expect(isTerminalRunStatus('running')).toBe(false)
  })
})
