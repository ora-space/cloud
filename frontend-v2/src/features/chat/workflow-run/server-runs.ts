import { cdaseWorkflow, cdaseDefinition, createCdaseRun, readCdaseRun, cdaseProjectRuns, isCdaseRun, startCdaseRun, updateCdaseInput, endCdaseRun } from '@/lib/demo/cdase'
import { demoMockEnabled } from '@/lib/demo/flags'
import { queryOptions, useQuery } from '@tanstack/react-query'
import { useMemo } from 'react'
import {
  analyzeWorkflow,
  cancelWorkflowRun,
  createWorkflowRun,
  deleteWorkflowRun,
  getWorkflowRun,
  getWorkflowRunSnapshot,
  listWorkflowRuns,
  startWorkflowRun,
  updateWorkflowRunInput,
  type WorkflowRunSummary,
  type WorkflowAnalysis,
  type WorkflowUnsupportedNode,
} from '@/lib/workflows/api/generated'
import { ContractError } from '@/lib/workflows/contracts'
import { contractError, type WorkflowContracts } from '@/lib/workflows/data/contracts-client'
import {
  isTerminalRunStatus,
  normalizeWorkflowDefinition,
  parseWorkflowGraph,
  toDisplayRunStatus,
  type GraphWorkflowRun,
  type GraphWorkflowRunStatus,
  type WorkflowDefinition,
} from '@/lib/workflows/runtime'
import { toGraphWorkflowRun } from './server-run-adapter'

/** A workflow the "运行工作流" menu offers: one with a published version. */
export interface PublishedWorkflow {
  id: string
  name: string
  version: string
  /**
   * The published snapshot itself. A version name can come back with another graph once the old
   * version is deleted, so the run is checked against this, never against the name.
   */
  snapshotId: string
}

/**
 * How often an open run is read again while it executes: every second, or, while its events stream
 * is connected and announces each change, every four seconds as desktop's fallback.
 */
export const runRefreshMs = { interval: 1_000, followed: 4_000 }

async function call<T>(request: () => Promise<T>): Promise<T> {
  try {
    return await request()
  } catch (error) {
    throw contractError(error)
  }
}

/**
 * Workflows that have a published version, newest first, as desktop's menu lists them. The list
 * names versions only, so each published workflow is read once more for its snapshot's identity;
 * one deleted or unpublished in between is left out.
 */
export async function listPublishedWorkflows(library: WorkflowContracts, signal?: AbortSignal): Promise<PublishedWorkflow[]> {
  const { workflows } = await library.list({}, { signal })
  const offered = await Promise.all(
    workflows
      .filter((w) => w.publishedVersion !== null)
      .map(async (w): Promise<PublishedWorkflow | null> => {
        try {
          const { workflow, published } = await library.get({ workflowId: w.id }, { signal })
          return published && { id: workflow.id, name: workflow.name, version: published.version, snapshotId: published.id }
        } catch (error) {
          if (error instanceof ContractError && error.code === 'workflow_not_found') return null
          throw error
        }
      }),
  )
  return [...(demoMockEnabled() ? [cdaseWorkflow] : []), ...offered.filter((w) => w !== null)]
}

/**
 * The frozen graph a run executes, as the editor's definition model. The snapshot does not carry
 * the workflow's name, and the run views never show it, so the version name stands in.
 */
function definitionOf(workflowId: string, name: string, graph: string, updatedAt: number): WorkflowDefinition {
  const parsed = parseWorkflowGraph(graph)
  return normalizeWorkflowDefinition({
    id: workflowId,
    name,
    description: parsed.description ?? '',
    updatedAt: new Date(updatedAt).toISOString(),
    viewport: parsed.viewport,
    globalVariables: parsed.globalVariables,
    nodes: parsed.nodes,
    edges: parsed.edges,
  })
}

/** Query keys of the run views; each is scoped by tenant. */
export const runKeys = {
  project: (tenantId: string, projectId: string) => ['workflow-runs', tenantId, 'project', projectId] as const,
  run: (tenantId: string, runId: string) => ['workflow-runs', tenantId, 'run', runId] as const,
  snapshot: (tenantId: string, runId: string) => ['workflow-runs', tenantId, 'snapshot', runId] as const,
  // A frozen snapshot and its analysis never change, so they live outside the 'workflow-runs'
  // prefix that run mutations (form saves, starts) invalidate wholesale.
  frozen: (tenantId: string, runId: string) => ['workflow-run-frozen', tenantId, 'snapshot', runId] as const,
  analysis: (tenantId: string, runId: string) => ['workflow-run-frozen', tenantId, 'analysis', runId] as const,
}

/** What v2-server's analysis says about a run's frozen graph. */
export type RunAnalysis = Pick<WorkflowAnalysis, 'unsupportedNodes' | 'unappliedSettings'>

const noAnalysis: RunAnalysis = { unsupportedNodes: [], unappliedSettings: [] }

/**
 * v2-server's analysis of a run's frozen graph: the reachable nodes start will refuse (prompt and
 * tool, which desktop does not execute either; spare nodes are never listed) and, per Agent, the
 * settings a run saves but does not apply. The snapshot is analyzed as stored, since the editor's
 * parsed model does not round-trip it. A frozen snapshot never changes, so it is analyzed once per
 * run and never refetched.
 */
export function runAnalysisQuery(tenantId: string, runId: string) {
  return queryOptions({
    queryKey: runKeys.analysis(tenantId, runId),
    enabled: tenantId !== '',
    staleTime: Number.POSITIVE_INFINITY,
    retry: false,
    queryFn: async ({ client, signal }): Promise<RunAnalysis> => {
      if (isCdaseRun(runId)) return noAnalysis
      // The pane has already read the snapshot; reuse it rather than downloading it again.
      const snapshot = await client.ensureQueryData(runFrozenSnapshotQuery(tenantId, runId))
      const { unsupportedNodes, unappliedSettings } = await call(() => analyzeWorkflow(tenantId, { graph: snapshot.graph }, undefined, signal))
      return { unsupportedNodes, unappliedSettings }
    },
  })
}

/**
 * The settings v2-server's analysis of the run's frozen graph lists as unapplied, by Agent node id;
 * empty while the analysis is unknown. The run inspector falls back to it for an attempt that
 * recorded no list of its own (still running, failed, cancelled, interrupted, or from an old run).
 */
export function useRunUnappliedSettings(tenantId: string, runId: string): ReadonlyMap<string, readonly string[]> {
  const { data } = useQuery(runAnalysisQuery(tenantId, runId))
  return useMemo(() => new Map((data?.unappliedSettings ?? []).map((entry) => [entry.nodeId, entry.settings])), [data])
}

/** What the start dialog knows about a run's execution path before start. */
export interface RunStartPreflight {
  /** Reachable nodes start will refuse; empty while unknown. */
  unsupported: readonly WorkflowUnsupportedNode[]
  /** The analysis is in flight, so submitting would skip its warning. */
  analyzing: boolean
  /** The analysis failed; server start stays the only check. */
  failed: boolean
}

/**
 * The start dialog's view of the run's analysis. An analysis that cannot be asked for (no tenant)
 * counts as none rather than as in flight, so the dialog never waits on a request that will not be
 * made; server start validates either way.
 */
export function useRunStartPreflight(tenantId: string, runId: string): RunStartPreflight {
  // isLoading, not isPending: a disabled query stays pending forever without fetching.
  const { data, isLoading, isError } = useQuery(runAnalysisQuery(tenantId, runId))
  return { unsupported: data?.unsupportedNodes ?? noUnsupportedNodes, analyzing: isLoading, failed: isError }
}

const noUnsupportedNodes: readonly WorkflowUnsupportedNode[] = []

/**
 * A run's frozen snapshot as stored. It never changes, so it is downloaded once per run and shared
 * by the editor model below and the run analysis.
 */
function runFrozenSnapshotQuery(tenantId: string, runId: string) {
  return queryOptions({
    queryKey: runKeys.frozen(tenantId, runId),
    staleTime: Number.POSITIVE_INFINITY,
    queryFn: async ({ signal }) => (await call(() => getWorkflowRunSnapshot(tenantId, runId, undefined, signal))).snapshot,
  })
}

/** A run's frozen graph, as the editor's definition model. */
export function runSnapshotQuery(tenantId: string, runId: string) {
  return queryOptions({
    queryKey: runKeys.snapshot(tenantId, runId),
    staleTime: Number.POSITIVE_INFINITY,
    queryFn: async ({ client }) => {
      if (isCdaseRun(runId)) { readCdaseRun(tenantId, runId); return cdaseDefinition() }
      const snapshot = await client.ensureQueryData(runFrozenSnapshotQuery(tenantId, runId))
      return definitionOf(snapshot.workflowId, snapshot.version, snapshot.graph, snapshot.updatedAt ?? snapshot.createdAt)
    },
  })
}

/**
 * One run with its nodes, projected for Theater. Until the run reaches a terminal state it is read
 * again on every invalidation its events stream announces, and polled as a fallback: slowly while
 * that stream is `followed`, every second otherwise.
 */
export function runQuery(tenantId: string, runId: string, definition: WorkflowDefinition | undefined, followed = false) {
  return queryOptions({
    queryKey: runKeys.run(tenantId, runId),
    enabled: definition !== undefined,
    staleTime: 0,
    queryFn: async ({ signal }) => isCdaseRun(runId) ? readCdaseRun(tenantId, runId) : toGraphWorkflowRun(await call(() => getWorkflowRun(tenantId, runId, undefined, signal)), definition!),
    refetchInterval: (query) => {
      const run = query.state.data
      if (run && isTerminalRunStatus(run.status)) return false
      return followed ? runRefreshMs.followed : runRefreshMs.interval
    },
  })
}

/** One row of a project's run list. */
export interface RunListItem {
  id: string
  name: string
  status: GraphWorkflowRunStatus
  createdAt: string
}

function listItem(summary: WorkflowRunSummary): RunListItem {
  return { id: summary.id, name: summary.name, status: toDisplayRunStatus(summary.status), createdAt: new Date(summary.createdAt).toISOString() }
}

/** A project's runs, newest first; read again while any of them still executes. */
export function projectRunsQuery(tenantId: string, projectId: string) {
  return queryOptions({
    queryKey: runKeys.project(tenantId, projectId),
    staleTime: 0,
    queryFn: async ({ signal }) => {
      const { runs } = await call(() => listWorkflowRuns(tenantId, { projectId }, undefined, signal))
      const local = cdaseProjectRuns(tenantId, projectId).map(({ id, name, status, createdAt }) => ({ id, name, status, createdAt }))
      return [...local, ...runs.map(listItem)].sort((a, b) => b.createdAt.localeCompare(a.createdAt))
    },
    refetchInterval: (query) => (query.state.data?.some((run) => !isTerminalRunStatus(run.status)) ? runRefreshMs.interval : false),
  })
}

/**
 * Creates a pending run of a workflow's published version in a project, as desktop's run
 * dialog does: v2-server freezes the published snapshot the menu offered, and Start begins it.
 * The snapshot must still be the published one; if someone activated another meanwhile, or
 * reused the version name for another graph, the run is refused rather than freezing a graph the
 * dialog did not name.
 */
export async function createRun(library: WorkflowContracts, tenantId: string, projectId: string, offered: PublishedWorkflow, name: string): Promise<string> {
  if (demoMockEnabled() && offered.id === cdaseWorkflow.id) return createCdaseRun(tenantId, projectId, name)
  const { published } = await library.get({ workflowId: offered.id })
  if (!published) throw new Error('这个工作流已经没有发布版本，请重新选择')
  if (published.id !== offered.snapshotId) {
    throw new Error(
      published.version === offered.version
        ? `${offered.version} 已经换成了另一份发布内容，请重新打开菜单选择`
        : `生效版本已从 ${offered.version} 变为 ${published.version}，请重新打开菜单选择`,
    )
  }
  const trimmed = name.trim()
  const { run } = await call(() =>
    createWorkflowRun(tenantId, offered.id, { projectId, snapshotId: offered.snapshotId, ...(trimmed ? { name: trimmed } : {}) }),
  )
  return run.id
}

/** Starts a pending run with the Start dialog's prompt and values. */
export async function startRun(tenantId: string, runId: string, prompt: string, variables: Record<string, unknown>): Promise<void> {
  if (isCdaseRun(runId)) { startCdaseRun(tenantId, runId, prompt); return }
  await call(() => updateWorkflowRunInput(tenantId, runId, { input: prompt, variables }))
  await call(() => startWorkflowRun(tenantId, runId))
}

/** Sets a pending run's kickoff input: the Start node's prompt, and the values when given. */
export async function updateRunInput(tenantId: string, runId: string, prompt: string, variables?: Record<string, unknown>): Promise<void> {
  if (isCdaseRun(runId)) { updateCdaseInput(tenantId, runId, prompt); return }
  await call(() => updateWorkflowRunInput(tenantId, runId, { input: prompt, ...(variables ? { variables } : {}) }))
}

/** Stops the run; v2-server answers once execution confirmed the stop. */
export async function cancelRun(tenantId: string, runId: string): Promise<void> {
  if (isCdaseRun(runId)) { endCdaseRun(tenantId, runId, false); return }
  await call(() => cancelWorkflowRun(tenantId, runId))
}

/** Deletes a finished run with its node rows. */
export async function deleteRun(tenantId: string, runId: string): Promise<void> {
  if (isCdaseRun(runId)) { endCdaseRun(tenantId, runId, true); return }
  await call(() => deleteWorkflowRun(tenantId, runId))
}

/** "再跑一次": a fresh pending run of the same frozen version, so the finished one stays in history. */
export async function rerun(tenantId: string, previous: GraphWorkflowRun): Promise<string> {
  const { run } = await call(() =>
    createWorkflowRun(tenantId, previous.definitionId, { projectId: previous.projectId, snapshotId: previous.snapshotId, name: previous.name }),
  )
  return run.id
}
