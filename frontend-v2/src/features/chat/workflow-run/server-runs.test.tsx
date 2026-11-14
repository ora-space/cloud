import '@/lib/workflows/test/dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderHook, waitFor } from '@testing-library/react'
import { useQuery } from '@tanstack/react-query'
import type { ReactNode } from 'react'
import { beforeEach, describe, expect, it } from 'vitest'
import { sampleWorkflowBackend, serveWorkflows, type WorkflowServer } from '@/lib/workflows/test/workflow-server'
import { chatBackend, tenantId } from '../test-backend'
import { runSnapshotQuery, useRunStartPreflight, useRunUnappliedSettings } from './server-runs'
import { serveRuns, type RunServer } from './test-run-server'

const wrapper = ({ children }: { children: ReactNode }) => (
  <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>{children}</QueryClientProvider>
)

describe('useRunStartPreflight', () => {
  let runs: RunServer
  beforeEach(async () => {
    chatBackend()
    const workflows = serveWorkflows(tenantId, await sampleWorkflowBackend())
    runs = serveRuns(tenantId, workflows.backend)
  })

  it('analyzes the run, and reports a failed analysis as failed', async () => {
    const { result } = renderHook(() => useRunStartPreflight(tenantId, 'run-missing'), { wrapper })
    expect(result.current).toEqual({ unsupported: [], analyzing: true, failed: false })
    await waitFor(() => expect(result.current).toEqual({ unsupported: [], analyzing: false, failed: true }))
    expect(runs.seen).toEqual([`GET /api/v2/tenants/${tenantId}/workflow-runs/run-missing/snapshot`])
  })

  it('treats an analysis it cannot ask for without a tenant as none, not as in flight', () => {
    const { result } = renderHook(() => useRunStartPreflight('', 'run-pending'), { wrapper })
    // The start dialog must not stay in "checking"; server start still validates.
    expect(result.current).toEqual({ unsupported: [], analyzing: false, failed: false })
  })
})

describe('the run analysis', () => {
  let runs: RunServer
  let workflows: WorkflowServer
  const runId = 'run-frozen'
  beforeEach(async () => {
    chatBackend()
    workflows = serveWorkflows(tenantId, await sampleWorkflowBackend())
    runs = serveRuns(tenantId, workflows.backend)
    const [{ id: workflowId }] = workflows.backend.list().workflows
    const { published } = workflows.backend.get({ workflowId })
    runs.runs.set(runId, {
      detail: {
        run: { id: runId, workflowId, snapshotId: published!.id, projectId: 'p', name: '冻结', status: 'failed', state: '{}', input: null, output: null, error: null, startedAt: 1, finishedAt: 2, createdAt: 1, updatedAt: 2 },
        nodes: [], scopes: [], conditionDecisions: {}, iterationConditionDecisions: {}, variables: [], failedAttempts: [],
      },
      graph: published!.graph,
      version: published!.version,
    })
  })
  const snapshotReads = () => runs.seen.filter((request) => request.endsWith(`/${runId}/snapshot`)).length
  const analyses = () => workflows.seen.filter((request) => request.endsWith('/workflows/analyze')).length

  it('reuses the snapshot the pane already read instead of downloading it again', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const own = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>
    const pane = renderHook(() => useQuery(runSnapshotQuery(tenantId, runId)), { wrapper: own })
    await waitFor(() => expect(pane.result.current.data).toBeDefined())
    const { result } = renderHook(() => useRunUnappliedSettings(tenantId, runId), { wrapper: own })
    await waitFor(() => expect(analyses()).toBe(1))
    await waitFor(() => expect(result.current).toBeInstanceOf(Map))
    expect(snapshotReads()).toBe(1)
  })

  it('is not run again when a form save or submit invalidates the run views', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    const own = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>
    renderHook(() => useRunUnappliedSettings(tenantId, runId), { wrapper: own })
    await waitFor(() => expect(analyses()).toBe(1))
    // What run-form.tsx does after saving a draft or submitting.
    await client.invalidateQueries({ queryKey: ['workflow-runs', tenantId] })
    await client.invalidateQueries({ queryKey: ['workflow-runs', tenantId] })
    await expect(waitFor(() => expect(analyses()).toBeGreaterThan(1), { timeout: 300 })).rejects.toThrow()
    expect(snapshotReads()).toBe(1)
  })
})
