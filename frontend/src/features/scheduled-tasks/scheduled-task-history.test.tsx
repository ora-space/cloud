import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { server } from '@/test/msw-server'
import type { ScheduledTaskOccurrence } from '@/api/generated.schemas'
import { ScheduledTaskHistory } from './scheduled-task-history'

const HISTORY = '/api/v1/tenants/tid1/scheduled-tasks/st1/history'

function entry(overrides: Partial<ScheduledTaskOccurrence> = {}): ScheduledTaskOccurrence {
  return {
    scheduledAt: '2026-10-22T01:00:00Z',
    outcome: 'completed',
    taskRevision: 1,
    observedAt: '2026-10-22T01:00:05Z',
    runId: 'r1',
    ...overrides,
  }
}

function renderHistory(entries: ScheduledTaskOccurrence[]) {
  server.use(http.get(HISTORY, () => HttpResponse.json({ taskId: 'st1', entries })))
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <ScheduledTaskHistory
        tid="tid1"
        issueId="i1"
        taskId="st1"
        title="每日同步"
        onClose={() => {}}
      />
    </QueryClientProvider>,
  )
}

describe('ScheduledTaskHistory', () => {
  it('shows the reported verdict and the artifact counts', async () => {
    renderHistory([
      entry({
        verdict: 'pass',
        verdictDetail: '两项检查通过',
        artifactCount: 2,
        rejectedArtifactCount: 1,
      }),
    ])
    expect(await screen.findByText(/结论 通过/)).toBeInTheDocument()
    expect(screen.getByText(/两项检查通过/)).toBeInTheDocument()
    // Registered artifacts and refused candidates are separate facts the reader must both see.
    expect(screen.getByText(/产物 2 项（1 项被拒）/)).toBeInTheDocument()
  })

  it('distinguishes expired artifacts from a run that produced none', async () => {
    renderHistory([entry({ artifactsExpired: true, artifactCount: 0 })])
    expect(await screen.findByText(/产物已过期/)).toBeInTheDocument()
    expect(screen.queryByText(/产物 0 项/)).not.toBeInTheDocument()
  })

  it('says 未报告 for a run that never reported a verdict', async () => {
    renderHistory([entry()])
    expect(await screen.findByText(/结论 未报告/)).toBeInTheDocument()
    expect(screen.getByText(/产物 0 项/)).toBeInTheDocument()
  })

  it('points a task conversation refusal at the conversation instead of a raw code', async () => {
    renderHistory([
      entry({
        scheduledAt: '2026-10-22T02:00:00Z',
        outcome: 'failed',
        failureReason: 'task_paused',
        runId: null,
      }),
      entry({ outcome: 'failed', failureReason: 'task_handoff_pending', runId: null }),
    ])
    expect(
      await screen.findByText('任务对话已暂停或有失败消息，请先在任务对话中处理'),
    ).toBeInTheDocument()
    expect(screen.getByText('任务对话中有等待执行的消息，本次未运行')).toBeInTheDocument()
    expect(screen.queryByText('task_paused')).not.toBeInTheDocument()
  })

  it('keeps other failure reasons as their code', async () => {
    renderHistory([entry({ outcome: 'failed', failureReason: 'model_unavailable', runId: null })])
    expect(await screen.findByText('model_unavailable')).toBeInTheDocument()
  })
})
