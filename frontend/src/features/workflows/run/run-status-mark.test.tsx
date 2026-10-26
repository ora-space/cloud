import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type {
  GraphWorkflowNodeStatus,
  GraphWorkflowRunStatus,
} from '@/features/workflows/runtime/types'
import { RunStatusBadge, RunStatusMark } from '@/features/workflows/run/run-status-mark'
import { renderWithProviders } from '@/test/render'

type RunStatus = GraphWorkflowRunStatus | GraphWorkflowNodeStatus

/** The zh label each status resolves to in the workflow bundle. */
const ZHU_LABELS: ReadonlyArray<[RunStatus, string]> = [
  ['pending', '等待中'],
  ['running', '运行中'],
  ['retrying', '重试中'],
  ['succeeded', '成功'],
  ['failed', '失败'],
  ['cancelled', '已取消'],
  ['idle', '未执行'],
  ['inactive', '未到达'],
]

/** How many glyph SVGs the mark painted after the last `unmount`. */
function svgCount(): number {
  return document.querySelector('body')?.querySelectorAll('svg').length ?? 0
}

describe('RunStatusBadge', () => {
  it('labels every status with its translated text', () => {
    for (const [status, label] of ZHU_LABELS) {
      const { unmount } = renderWithProviders(<RunStatusBadge status={status} />)
      expect(screen.getByText(label)).toBeInTheDocument()
      unmount()
    }
  })
})

describe('RunStatusMark', () => {
  it('spins a working cue for a live running status', () => {
    const { unmount } = renderWithProviders(<RunStatusMark status="running" live />)
    expect(svgCount()).toBe(1)
    unmount()
  })

  it('spins a working cue while a node waits for its next retry', () => {
    const { unmount } = renderWithProviders(<RunStatusMark status="retrying" live />)
    expect(svgCount()).toBe(1)
    unmount()
  })

  it('keeps a plain dot for a live-worthy-looking but non-working status', () => {
    const { unmount } = renderWithProviders(<RunStatusMark status="pending" live />)
    expect(svgCount()).toBe(0)
    unmount()
  })

  it('paints a terminal glyph unless quieted', () => {
    const loud = renderWithProviders(<RunStatusMark status="succeeded" />)
    expect(svgCount()).toBe(1)
    loud.unmount()

    const quiet = renderWithProviders(<RunStatusMark status="succeeded" quiet />)
    expect(svgCount()).toBe(0)
    quiet.unmount()
  })
})
