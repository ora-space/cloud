import { create } from 'zustand'
import type { WorkflowContracts } from '@/lib/workflows/data/contracts-client'
import {
  computeInactiveNodes,
  normalizeWorkflowDefinition,
  parseWorkflowGraph,
  type GraphWorkflowRun,
  type WorkflowArtifact,
  type WorkflowNodeConversationItem,
  type WorkflowRuntime,
} from '@/lib/workflows/runtime'
import { createMemoryWorkflowRuntime } from '@/lib/workflows/runtime/memory'
import { planMockExecution } from '@/lib/workflows/runtime/mock-execution-plan'

/** A workflow the "运行工作流" menu offers: one with a published version. */
export interface PublishedWorkflow {
  id: string
  name: string
  version: string
}

/** How long each node of a demo run takes; tests shorten it before creating runs. */
export const demoRunClock = { nodeStepMs: 1500 }

interface RunState {
  runs: Record<string, GraphWorkflowRun>
  /** What each run's nodes said, in the order the engine produced it. */
  conversations: Record<string, WorkflowNodeConversationItem[]>
  /** Start values the person entered, kept for the Start node's output. */
  inputs: Record<string, Record<string, unknown>>
  /** Files the run's nodes produced, oldest first. */
  artifacts: Record<string, WorkflowArtifact[]>
  /** The artifact that arrived last, which Theater brings into view. */
  revealedArtifactIds: Record<string, string>
}

const emptyRuns: RunState = { runs: {}, conversations: {}, inputs: {}, artifacts: {}, revealedArtifactIds: {} }

/** Runs of the in-browser engine, kept current by its change and event streams. */
export const useDemoRuns = create<RunState>()(() => ({ ...emptyRuns }))

let runtime: WorkflowRuntime | undefined
const unsubscribes = new Map<string, () => void>()

/**
 * The in-browser stand-in for the workflow run backend: desktop's memory runtime advances the
 * frozen graph on a timer. It lives for the page's lifetime, so runs survive switching pages.
 */
function demoRuntime(): WorkflowRuntime {
  if (runtime) return runtime
  runtime = createMemoryWorkflowRuntime({ nodeStepMs: demoRunClock.nodeStepMs })
  runtime.runs.watch((run) => useDemoRuns.setState((s) => ({ runs: { ...s.runs, [run.id]: withInactiveBranches(run) } })))
  return runtime
}

/**
 * Marks the nodes behind a condition's untaken branch inactive, as desktop's run adapter does for
 * a backend run, so Theater leaves them off the path and counts the run complete. The engine keeps
 * them idle and does not record which branch a condition took; its path choice is deterministic,
 * so the same plan recovers each decision.
 */
export function withInactiveBranches(run: GraphWorkflowRun): GraphWorkflowRun {
  if (run.status === 'pending') return run
  const definition = run.definitionSnapshot
  const reachable = new Set(planMockExecution(definition, { kickoffInput: run.kickoffInput }).order)
  const decisions: Record<string, string> = {}
  for (const node of definition.nodes) {
    if (node.data.kind !== 'condition') continue
    const taken = definition.edges.find((edge) => edge.source === node.id && reachable.has(edge.target))
    if (taken) decisions[node.id] = taken.sourceHandle ?? 'else'
  }
  const statuses = Object.fromEntries(Object.entries(run.nodeStates).map(([id, state]) => [id, state.status]))
  const inactive = computeInactiveNodes(definition, statuses, decisions)
  if (inactive.size === 0) return run
  const nodeStates = { ...run.nodeStates }
  for (const id of inactive) nodeStates[id] = { ...nodeStates[id], status: 'inactive' }
  return { ...run, nodeStates }
}

/** Drops every demo run; tests call it between cases. */
export function resetDemoRuns(): void {
  for (const unsubscribe of unsubscribes.values()) unsubscribe()
  unsubscribes.clear()
  runtime?.dispose()
  runtime = undefined
  useDemoRuns.setState({ ...emptyRuns })
}

/** Workflows that have a published version, newest first, as desktop's menu lists them. */
export async function listPublishedWorkflows(library: WorkflowContracts, signal?: AbortSignal): Promise<PublishedWorkflow[]> {
  const { workflows } = await library.list({}, { signal })
  return workflows.flatMap((w) => (w.publishedVersion === null ? [] : [{ id: w.id, name: w.name, version: w.publishedVersion }]))
}

/**
 * Creates a pending run of a workflow's published version in a project, as desktop's run
 * dialog does: the server's published graph is frozen into the run, and Start begins it. The
 * version must still be the one the menu offered; if someone activated another meanwhile, the
 * run is refused rather than freezing a version the dialog did not name. The run itself is still
 * played by the in-browser engine until v2-server executes workflows.
 */
export async function createDemoRun(
  library: WorkflowContracts,
  projectId: string,
  offered: PublishedWorkflow,
  name: string,
): Promise<GraphWorkflowRun> {
  const { workflow, published } = await library.get({ workflowId: offered.id })
  if (!published) throw new Error('这个工作流已经没有发布版本，请重新选择')
  if (published.version !== offered.version) {
    throw new Error(`生效版本已从 ${offered.version} 变为 ${published.version}，请重新打开菜单选择`)
  }
  const graph = parseWorkflowGraph(published.graph)
  const rt = demoRuntime()
  await rt.host.mount(
    projectId,
    normalizeWorkflowDefinition({
      id: workflow.id,
      name: workflow.name,
      description: graph.description ?? '',
      updatedAt: new Date(Number(published.updatedAt)).toISOString(),
      viewport: graph.viewport,
      globalVariables: graph.globalVariables,
      nodes: graph.nodes,
      edges: graph.edges,
    }),
  )
  const created = await rt.runs.create({ projectId, definitionId: workflow.id })
  const run = name.trim() && name.trim() !== created.name ? await rt.runs.rename(created.id, name) : created
  followConversation(rt, run.id)
  return run
}

/** Keeps the run's node conversation and artifacts in the store until the run is deleted. */
function followConversation(rt: WorkflowRuntime, runId: string): void {
  const unsubscribe = rt.runs.subscribe(
    runId,
    (event) => {
      if (event.type === 'artifact_added') {
        useDemoRuns.setState((s) => ({
          artifacts: { ...s.artifacts, [runId]: [...(s.artifacts[runId] ?? []), event.artifact] },
          revealedArtifactIds: { ...s.revealedArtifactIds, [runId]: event.artifact.id },
        }))
        return
      }
      if (event.type !== 'node_conversation_item_upserted') return
      useDemoRuns.setState((s) => {
        const list = s.conversations[runId] ?? []
        const index = list.findIndex((item) => item.id === event.item.id)
        const next = index < 0 ? [...list, event.item] : list.map((item, i) => (i === index ? event.item : item))
        return { conversations: { ...s.conversations, [runId]: next } }
      })
    },
    { afterCursor: null },
  )
  unsubscribes.set(runId, unsubscribe)
}

/** Sets a run's kickoff input: the Start node's prompt, and the values when given. */
export async function updateDemoRunInput(runId: string, initialPrompt: string, variables?: Record<string, unknown>): Promise<void> {
  const rt = demoRuntime()
  const run = await rt.runs.get(runId)
  const start = run?.definitionSnapshot.nodes.find((node) => node.data.kind === 'start')
  if (start) await rt.runs.updateSnapshotNode(runId, start.id, { input: initialPrompt })
  if (variables) useDemoRuns.setState((s) => ({ inputs: { ...s.inputs, [runId]: variables } }))
}

/** Starts a pending run with the Start dialog's prompt and values. */
export async function startDemoRun(runId: string, initialPrompt: string, variables: Record<string, unknown>): Promise<void> {
  await updateDemoRunInput(runId, initialPrompt, variables)
  await demoRuntime().runs.start(runId)
}

/** Answers one open human-in-the-loop gate; the run carries on when no gate is left. */
export async function submitDemoHitl(runId: string, requestId: string, payload: Record<string, unknown>): Promise<GraphWorkflowRun> {
  return demoRuntime().runs.submitHitl(runId, requestId, payload)
}

export async function cancelDemoRun(runId: string): Promise<void> {
  await demoRuntime().runs.cancel(runId)
}

export async function deleteDemoRun(runId: string): Promise<void> {
  await demoRuntime().runs.delete(runId)
  unsubscribes.get(runId)?.()
  unsubscribes.delete(runId)
  useDemoRuns.setState((s) => {
    const { [runId]: _run, ...runs } = s.runs
    const { [runId]: _conversation, ...conversations } = s.conversations
    const { [runId]: _artifacts, ...artifacts } = s.artifacts
    return { runs, conversations, artifacts }
  })
}

/** "再跑一次": a fresh pending run of the same frozen graph, so the finished one stays in history. */
export async function rerunDemoRun(runId: string): Promise<GraphWorkflowRun> {
  const rt = demoRuntime()
  const previous = useDemoRuns.getState().runs[runId]
  if (!previous) throw new Error('找不到这次运行')
  const created = await rt.runs.create({ projectId: previous.projectId, definitionId: previous.definitionId })
  const run = await rt.runs.rename(created.id, previous.name)
  followConversation(rt, run.id)
  return run
}
