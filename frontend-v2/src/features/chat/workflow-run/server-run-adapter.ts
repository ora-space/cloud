import type { WorkflowNodeRun, WorkflowRunDetail } from '@/lib/workflows/api/generated'
import {
  computeInactiveNodes,
  projectNodeStatus,
  projectRunStatus,
  type GraphWorkflowNodeState,
  type GraphWorkflowRun,
  type WorkflowAgentUnappliedCapability,
  type WorkflowDefinition,
  type WorkflowNodeConversationItem,
  type WorkflowNodeErrorDetail,
  type WorkflowNodeExecutorUsed,
  type WorkflowNodeModelUsed,
} from '@/lib/workflows/runtime'

const iso = (ms: number | null): string | undefined => (ms === null ? undefined : new Date(ms).toISOString())

function parseObject(raw: string | null): Record<string, unknown> {
  if (raw === null) return {}
  try {
    const value: unknown = JSON.parse(raw)
    return value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : {}
  } catch {
    return {}
  }
}

/** Reads `payload.error_detail`, which the engine writes with desktop's snake_case keys. */
function errorDetail(payload: Record<string, unknown>): WorkflowNodeErrorDetail | undefined {
  const d = payload.error_detail
  if (d === null || typeof d !== 'object') return undefined
  const detail = d as Record<string, unknown>
  if (typeof detail.kind !== 'string') return undefined
  return {
    kind: detail.kind,
    message: typeof detail.message === 'string' ? detail.message : '',
    sourceChain: Array.isArray(detail.source_chain) ? detail.source_chain.filter((s): s is string => typeof s === 'string') : [],
    attempt: typeof detail.attempt === 'number' ? detail.attempt : 1,
    resumable: detail.resumable === true,
    injectsPreviousFailure: typeof detail.injects_previous_failure === 'boolean' ? detail.injects_previous_failure : undefined,
    autoRetryable: typeof detail.auto_retryable === 'boolean' ? detail.auto_retryable : undefined,
    recordedAt: typeof detail.recorded_at === 'number' ? detail.recorded_at : 0,
  }
}

const text = (value: unknown): string => (typeof value === 'string' ? value : '')

const record = (value: unknown): Record<string, unknown> | undefined =>
  value !== null && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : undefined

/** Reads `payload.executor`, the executor a successful Agent attempt ran on. */
function executorUsed(payload: Record<string, unknown>): WorkflowNodeExecutorUsed | undefined {
  const e = record(payload.executor)
  if (e === undefined || text(e.id) === '') return undefined
  return { id: text(e.id), agentId: text(e.agentId), name: text(e.name), pluginVersion: text(e.pluginVersion) }
}

/** Reads `payload.model`, the model a successful Agent attempt ran on and whether it was a fallback. */
function modelUsed(payload: Record<string, unknown>): WorkflowNodeModelUsed | undefined {
  const m = record(payload.model)
  if (m === undefined || (text(m.id) === '' && text(m.name) === '')) return undefined
  return { id: text(m.id), name: text(m.name), requested: text(m.requested), fallback: m.fallback === true }
}

const unappliedNames: ReadonlySet<string> = new Set<WorkflowAgentUnappliedCapability>(['role', 'skill', 'mcp', 'retry', 'interactive'])

/** Reads `payload.unapplied`, the configured settings v2 ran the attempt without; unknown names are dropped. */
function unapplied(payload: Record<string, unknown>): WorkflowAgentUnappliedCapability[] | undefined {
  if (!Array.isArray(payload.unapplied)) return undefined
  return payload.unapplied.filter((name): name is WorkflowAgentUnappliedCapability => typeof name === 'string' && unappliedNames.has(name))
}

/**
 * The node's turn as its conversation: what it was asked and what it answered. v2-server does not
 * serve node session transcripts yet, so the task (as the engine rendered it, else the attempt's
 * template input) and the settled answer are the two messages; nothing is invented while it runs.
 */
function conversation(runId: string, node: WorkflowNodeRun, payload: Record<string, unknown>): WorkflowNodeConversationItem[] | undefined {
  if (node.nodeType !== 'agent' || node.output === null || node.output === '') return undefined
  const base = { runId, nodeId: node.nodeId, sessionId: node.sessionId ?? '', status: 'complete' as const }
  const asked = typeof payload.task === 'string' && payload.task !== '' ? payload.task : node.input
  const askedAt = iso(node.startedAt ?? node.createdAt) ?? ''
  const answeredAt = iso(node.finishedAt ?? node.updatedAt) ?? ''
  const items: WorkflowNodeConversationItem[] = []
  if (asked) items.push({ ...base, kind: 'message', role: 'user', id: `${node.id}:task`, createdAt: askedAt, updatedAt: askedAt, markdown: asked })
  items.push({ ...base, kind: 'message', role: 'assistant', id: `${node.id}:answer`, createdAt: answeredAt, updatedAt: answeredAt, markdown: node.output })
  return items
}

function nodeState(runId: string, node: WorkflowNodeRun): GraphWorkflowNodeState {
  const payload = parseObject(node.payload)
  const state: GraphWorkflowNodeState = { status: projectNodeStatus({ status: node.status }), nodeRunId: node.id }
  if (node.sessionId !== null) state.sessionId = node.sessionId
  if (node.startedAt !== null) state.startedAt = iso(node.startedAt)
  if (node.finishedAt !== null) state.finishedAt = iso(node.finishedAt)
  if (node.error !== null) state.errorMessage = node.error
  const detail = errorDetail(payload)
  if (detail) state.errorDetail = detail
  if (typeof payload.stop_reason === 'string') state.stopReason = payload.stop_reason
  const executor = executorUsed(payload)
  if (executor) state.executor = executor
  const model = modelUsed(payload)
  if (model) state.model = model
  const skipped = unapplied(payload)
  if (skipped) state.unapplied = skipped
  if (node.input !== null && node.input !== '') state.input = { summary: node.input.split('\n')[0]!, detail: node.input }
  if (node.output !== null && node.output !== '') state.output = { summary: node.output.split('\n')[0]!, detail: node.output }
  const items = conversation(runId, node, payload)
  if (items) state.conversation = items
  return state
}

function currentNodes(state: string | null): string[] {
  const nodes = parseObject(state).current_nodes
  return Array.isArray(nodes) ? nodes.filter((n): n is string => typeof n === 'string') : []
}

/**
 * Projects a v2-server run onto the run model Theater and Overview render, as desktop's backend
 * run adapter does. The detail lists each node's latest attempt; graph nodes without one stay idle,
 * except those behind a Condition branch not taken, which never get a row and are shown inactive,
 * projected from the run's Condition decisions as desktop does. Loop and Iteration rounds are not
 * executed by v2-server yet, so only root-scope rows are mapped.
 */
export function toGraphWorkflowRun(detail: WorkflowRunDetail, definition: WorkflowDefinition): GraphWorkflowRun {
  const { run } = detail
  const nodeStates: Record<string, GraphWorkflowNodeState> = {}
  for (const node of definition.nodes) nodeStates[node.id] = { status: 'idle' }
  for (const node of detail.nodes) {
    if (node.iteration !== null) continue
    nodeStates[node.nodeId] = nodeState(run.id, node)
  }
  const statuses = Object.fromEntries(Object.entries(nodeStates).map(([id, state]) => [id, state.status]))
  for (const id of computeInactiveNodes(definition, statuses, detail.conditionDecisions)) {
    nodeStates[id] = { ...nodeStates[id], status: 'inactive' }
  }
  const result: GraphWorkflowRun = {
    id: run.id,
    projectId: run.projectId,
    definitionId: run.workflowId,
    definitionSnapshot: definition,
    name: run.name,
    status: projectRunStatus(run.status, currentNodes(run.state)),
    nodeStates,
    openHitls: [],
    createdAt: iso(run.createdAt)!,
    updatedAt: iso(run.updatedAt)!,
    snapshotId: run.snapshotId,
  }
  if (run.input !== null) result.kickoffInput = run.input
  if (run.output !== null) result.finalOutput = run.output
  if (run.finishedAt !== null) result.finishedAt = iso(run.finishedAt)
  return result
}

