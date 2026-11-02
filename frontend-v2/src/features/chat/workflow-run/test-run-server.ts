import type { AxiosAdapter, AxiosResponse } from 'axios'
import { AxiosError } from 'axios'
import { http } from '@/lib/api-client'
import type { WorkflowNodeRun, WorkflowRun, WorkflowRunDetail } from '@/lib/workflows/api/generated'
import type { LocalWorkflowBackend } from '@/lib/workflows/data/local-workflow-backend'
import { parseWorkflowGraph } from '@/lib/workflows/runtime'

/** What the fake run server holds; tests read it and steer how runs execute. */
export interface RunServer {
  runs: Map<string, { detail: WorkflowRunDetail; graph: string; version: string }>
  seen: string[]
  /** When set, a started run stops at its first Agent, running, until cancelled. */
  hold: boolean
}

/**
 * Serves v2-server's run routes for `tenantId` over the workflow library the fake definition server
 * holds, layered in front of it. A started run executes at once, node by node in the graph's edge
 * order, each Agent answering `回答：<title>`; with `hold` it waits at the first Agent instead.
 */
export function serveRuns(tenantId: string, backend: LocalWorkflowBackend): RunServer {
  const server: RunServer = { runs: new Map(), seen: [], hold: false }
  const previous = http.defaults.adapter as AxiosAdapter
  let clock = 1_700_000_000_000
  let ids = 0
  const now = () => (clock += 1_000)
  const runsBase = `/api/v2/tenants/${tenantId}/workflow-runs`
  const createRun = new RegExp(`^/api/v2/tenants/${tenantId}/workflows/([^/]+)/runs$`)

  const respond = (config: Parameters<AxiosAdapter>[0], status: number, data: unknown): AxiosResponse => {
    const response: AxiosResponse = { status, statusText: String(status), data, headers: {}, config }
    if (status >= 400) throw new AxiosError(`HTTP ${status}`, String(status), config, undefined, response)
    return response
  }
  const refuse = (config: Parameters<AxiosAdapter>[0], status: number, code: string) => respond(config, status, { code, params: {}, requestId: 'r' })

  function execute(entry: { detail: WorkflowRunDetail; graph: string }) {
    const { run } = entry.detail
    const graph = parseWorkflowGraph(entry.graph)
    const order: string[] = []
    const start = graph.nodes.find((n) => n.data.kind === 'start')
    const queue = start ? [start.id] : []
    while (queue.length > 0) {
      const id = queue.shift()!
      if (order.includes(id)) continue
      order.push(id)
      for (const edge of graph.edges) if (edge.source === id) queue.push(edge.target)
    }
    const answers: Record<string, string> = {}
    for (const id of order) {
      const node = graph.nodes.find((n) => n.id === id)!
      const at = now()
      const row: WorkflowNodeRun = {
        id: `node-run-${++ids}`, runId: run.id, scopeId: `root:${run.id}`, nodeId: id, nodeType: node.data.kind, sessionId: null,
        status: 'succeeded', input: null, output: null, error: null, payload: null, iteration: null, startedAt: at, finishedAt: at, createdAt: at, updatedAt: at,
      }
      entry.detail.nodes.push(row)
      if (node.data.kind === 'start') row.output = run.input ?? ''
      if (node.data.kind === 'agent') {
        row.sessionId = `session-${ids}`
        if (server.hold) {
          row.status = 'running'
          row.finishedAt = null
          run.status = 'running'
          run.state = JSON.stringify({ current_nodes: [id] })
          return
        }
        row.output = `回答：${node.data.title ?? id}`
        row.payload = JSON.stringify({ stop_reason: 'end_turn', task: `请完成：${node.data.title ?? id}` })
        answers[id] = row.output
      }
      if (node.data.kind === 'output') row.output = JSON.stringify(answers)
    }
    run.status = 'succeeded'
    run.output = JSON.stringify(answers)
    run.finishedAt = now()
    run.state = JSON.stringify({ current_nodes: [] })
  }

  http.defaults.adapter = async (config) => {
    const url = config.url ?? ''
    const method = config.method?.toUpperCase() ?? 'GET'
    const body = (typeof config.data === 'string' ? JSON.parse(config.data) : (config.data ?? {})) as Record<string, unknown>
    const created = url.match(createRun)
    if (!created && url !== runsBase && !url.startsWith(`${runsBase}/`)) return previous(config)
    server.seen.push(`${method} ${url}`)
    if (created && method === 'POST') {
      const { published } = backend.get({ workflowId: created[1]! })
      if (!published || published.id !== body.snapshotId) return refuse(config, 400, 'workflow_no_published_snapshot')
      const at = now()
      const run: WorkflowRun = {
        id: `run-${++ids}`, workflowId: created[1]!, snapshotId: published.id, projectId: body.projectId as string,
        name: (body.name as string | undefined) ?? '', status: 'pending', state: JSON.stringify({ current_nodes: [] }),
        input: null, output: null, error: null, startedAt: null, finishedAt: null, createdAt: at, updatedAt: at,
      }
      server.runs.set(run.id, { detail: { run, nodes: [], scopes: [], conditionDecisions: {} }, graph: published.graph, version: published.version })
      return respond(config, 201, { run })
    }
    if (url === runsBase && method === 'GET') {
      const projectId = (config.params as { projectId?: string } | undefined)?.projectId
      const runs = [...server.runs.values()]
        .filter(({ detail }) => projectId === undefined || detail.run.projectId === projectId)
        .map(({ detail: { run }, version }) => ({ id: run.id, name: run.name, projectId: run.projectId, workflowId: run.workflowId, version, status: run.status, startedAt: run.startedAt, finishedAt: run.finishedAt, createdAt: run.createdAt }))
      return respond(config, 200, { runs })
    }
    const [, runId, action = ''] = url.slice(runsBase.length).match(/^\/([^/]+)(\/[a-z]+)?$/) ?? []
    const entry = runId ? server.runs.get(runId) : undefined
    if (!entry) return refuse(config, 404, 'workflow_run_not_found')
    const { run } = entry.detail
    if (action === '' && method === 'GET') return respond(config, 200, entry.detail)
    if (action === '/snapshot') {
      return respond(config, 200, { snapshot: { id: run.snapshotId, workflowId: run.workflowId, version: entry.version, graph: entry.graph, createdAt: run.createdAt, updatedAt: run.createdAt } })
    }
    if (action === '/input') {
      if (run.status !== 'pending') return refuse(config, 409, 'workflow_run_not_editable')
      run.input = body.input as string | null
      return respond(config, 200, { run })
    }
    if (action === '/start') {
      if (run.status !== 'pending') return refuse(config, 400, 'workflow_run_start_failed')
      run.status = 'running'
      run.startedAt = now()
      execute(entry)
      return respond(config, 200, { run })
    }
    if (action === '/cancel') {
      if (run.status !== 'pending' && run.status !== 'running' && run.status !== 'cancelled') return refuse(config, 409, 'workflow_run_state_conflict')
      run.status = 'cancelled'
      run.finishedAt = now()
      for (const node of entry.detail.nodes) if (node.status === 'running') node.status = 'cancelled'
      return respond(config, 200, { run })
    }
    if (action === '' && method === 'DELETE') {
      if (run.status === 'running') return refuse(config, 400, 'workflow_run_active')
      server.runs.delete(run.id)
      return respond(config, 200, { runId: run.id })
    }
    return refuse(config, 404, 'not_found')
  }
  return server
}
