import { effectiveWorkflowContainer } from '@/features/workflows/runtime/container-layout'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'
import { parseRunNodeStates, type RunNodeEntry } from '@/features/workflows/run/run-node-state'

/**
 * Round projection of a run's Loop and Iteration containers.
 *
 * The backend keeps one round document inside the container's own `node_states` row (loop ADR
 * D1, iteration ADR D5): settled rounds are archived in `completed[]`, the active round's members
 * stay flat at the top level under their own ids, and the final round is both archived and left
 * at the top level. This module is the one place that turns that into "a list of rounds, each
 * with every member's state", so the de-duplication rule lives here and nowhere else. It reads
 * only committed facts and never re-evaluates a condition.
 */

/** How one round ended, as far as the committed state can tell. */
export type RunRoundOutcome =
  /** The round is in progress, or ended mid-round with its container. */
  | 'running'
  | 'failed'
  | 'cancelled'
  /** A Loop round followed by another round. */
  | 'continued'
  /** The final Loop round, by how the Loop finished. */
  | 'conditionMet'
  | 'loopExit'
  /** The Loop ran out of rounds under `succeed`, as its recorded success reason says. */
  | 'limitReached'
  /** The Loop used its last round on `succeed` and recorded no reason: `until` may also have matched. */
  | 'completed'
  /** An Iteration item that collected a value. */
  | 'succeeded'
  /** An Iteration item that failed and was absorbed by the `continue` strategy. */
  | 'absorbed'

/** One round of a container, with every member's state in that round. */
export interface RunRound {
  /** The backend's round number: Loop rounds count from 1, Iteration items from 0. */
  index: number
  /** Settled rounds come from the archive; the active one from the top-level members. */
  source: 'archived' | 'active'
  outcome: RunRoundOutcome
  /** Every member of the container: one that did not run this round is `inactive`. */
  states: Record<string, RunNodeEntry>
  /** Iteration only: the frozen input item this round processed. */
  item?: unknown
  /** Iteration only: the ledger entry the round settled into. */
  entry?: RunIterationLedgerEntry
}

/** One settled Iteration item as the ledger records it. */
export interface RunIterationLedgerEntry {
  status: 'succeeded' | 'failed'
  output: unknown
  error: string | null
}

/** A Loop or Iteration container of the run, projected into rounds. */
export interface RunContainer {
  id: string
  kind: 'loop' | 'iteration'
  status: RunNodeEntry['status']
  /** Members by the frozen snapshot, in snapshot order. */
  memberIds: string[]
  /** Loop: its round ceiling. Iteration: the number of frozen items. */
  limit: number
  /** The container's stable failure reason (`output.reason`), when it failed or was cancelled. */
  reason?: string
  /** Iteration only: settled items and how many of them failed. */
  settled?: number
  failed?: number
  rounds: RunRound[]
}

/** The reader's chosen round per container; a missing entry means "the latest round". */
export type RunRoundSelection = Readonly<Record<string, number>>

/** The frozen node fields the projection reads. */
export interface RunFrozenNode {
  id: string
  parentId?: string | undefined
  data: WorkflowNodeData
}

/**
 * Projects every Loop and Iteration container of a run into rounds.
 *
 * A container whose round document is missing or malformed is left out, so the canvas and
 * inspector fall back to the flat per-node view rather than guessing — the same stance the
 * flat decoder takes towards a row it cannot read.
 *
 * @param raw - The run's raw `nodeStates` value.
 * @param frozenNodes - The nodes of the snapshot the run pinned, for membership.
 * @returns The containers keyed by node id.
 */
export function parseRunContainers(
  raw: Record<string, unknown> | undefined,
  frozenNodes: readonly RunFrozenNode[],
): Record<string, RunContainer> {
  const containers: Record<string, RunContainer> = {}
  if (raw === undefined) {
    return containers
  }
  const flat = parseRunNodeStates(raw)
  for (const node of frozenNodes) {
    const kind = node.data.kind
    const entry = flat[node.id]
    const value = raw[node.id]
    if ((kind !== 'loop' && kind !== 'iteration') || entry === undefined || !isRecord(value)) {
      continue
    }
    const memberIds = frozenNodes
      .filter((candidate) => effectiveWorkflowContainer(candidate) === node.id)
      .map((candidate) => candidate.id)
    const kinds = new Map(frozenNodes.map((candidate) => [candidate.id, candidate.data.kind]))
    const context = { node, entry, memberIds, kinds, flat, output: value['output'] }
    const container =
      kind === 'loop'
        ? readLoopContainer(value['loop'], context)
        : readIterationContainer(value['iteration'], context)
    if (container !== undefined) {
      containers[node.id] = container
    }
  }
  return containers
}

/**
 * Overlays each container's selected round onto the flat node states.
 *
 * Members show the state they had in the chosen round (the latest one by default) and
 * `inactive` when they did not run in it — never a value borrowed from another round or from
 * the top-level copy of the final round.
 *
 * @param states - The flat per-node rows.
 * @param containers - The projected containers.
 * @param selection - The reader's per-container round choice.
 * @returns The rows the canvas paints.
 */
export function projectRoundStates(
  states: Record<string, RunNodeEntry>,
  containers: Record<string, RunContainer>,
  selection: RunRoundSelection,
): Record<string, RunNodeEntry> {
  const projected = { ...states }
  for (const container of Object.values(containers)) {
    const round = selectedRound(container, selection)
    for (const memberId of container.memberIds) {
      const state = round?.states[memberId]
      if (state === undefined) {
        delete projected[memberId]
      } else {
        projected[memberId] = state
      }
    }
  }
  return projected
}

/** The round the reader looks at: the chosen one, else the latest. */
export function selectedRound(
  container: RunContainer,
  selection: RunRoundSelection,
): RunRound | undefined {
  const chosen = selection[container.id]
  const match =
    chosen === undefined ? undefined : container.rounds.find((round) => round.index === chosen)
  return match ?? container.rounds.at(-1)
}

/** The fields shared by both container readers. */
interface ContainerContext {
  node: RunFrozenNode
  entry: RunNodeEntry
  memberIds: string[]
  /** Kind of every frozen node, to recognize a loopExit among the members. */
  kinds: ReadonlyMap<string, WorkflowNodeData['kind']>
  flat: Record<string, RunNodeEntry>
  output: unknown
}

/** Reads a Loop's `loop` document, or `undefined` when it is not one the projection trusts. */
function readLoopContainer(value: unknown, context: ContainerContext): RunContainer | undefined {
  if (!isRecord(value)) {
    return undefined
  }
  const round = readCount(value['round'])
  const maxIterations = readCount(value['maxIterations'])
  const completed = readArchive(value['completed'])
  if (round === undefined || maxIterations === undefined || completed === undefined) {
    return undefined
  }
  const rounds = completed.map((archived) => ({
    index: archived.index,
    source: 'archived' as const,
    outcome: 'continued' as RunRoundOutcome,
    states: memberStates(archived.states, context.memberIds),
  }))
  appendActiveRound(rounds, round, context)
  const last = rounds.at(-1)
  if (last !== undefined) {
    last.outcome = finalLoopOutcome(last, {
      context,
      round,
      maxIterations,
      recorded: RECORDED_LOOP_OUTCOMES.get(String(value['reason'])),
    })
  }
  return {
    ...containerBase(context, 'loop'),
    limit: maxIterations,
    rounds,
  }
}

/** Reads an Iteration's `iteration` document, or `undefined` when it is malformed. */
function readIterationContainer(
  value: unknown,
  context: ContainerContext,
): RunContainer | undefined {
  if (!isRecord(value)) {
    return undefined
  }
  const round = readCount(value['round'])
  const items = value['items']
  const ledger = value['ledger']
  const completed = readArchive(value['completed'])
  if (round === undefined || !Array.isArray(items) || !Array.isArray(ledger) || !completed) {
    return undefined
  }
  const entries = ledger.map(readLedgerEntry)
  const rounds: RunRound[] = completed.map((archived) => {
    const entry = entries[archived.index]
    const settled: RunRound = {
      index: archived.index,
      source: 'archived',
      outcome: entry?.status === 'failed' ? 'absorbed' : 'succeeded',
      states: memberStates(archived.states, context.memberIds),
      item: items[archived.index],
    }
    if (entry !== undefined) {
      settled.entry = entry
    }
    return settled
  })
  appendActiveRound(rounds, round, context)
  const active = rounds.at(-1)
  if (active?.source === 'active') {
    active.item = items[active.index]
  }
  const last = rounds.at(-1)
  // Under `fail` the first failed item ends the node without advancing `round`, so only that
  // item reads as failed. Under `continue` an absorbed item stays absorbed even if the container
  // later fails for another reason (a later item's budget, an invalid round document).
  if (last?.entry?.status === 'failed' && value['strategy'] === 'fail' && last.index === round) {
    last.outcome = 'failed'
  }
  return {
    ...containerBase(context, 'iteration'),
    limit: items.length,
    settled: entries.filter((entry) => entry !== undefined).length,
    failed: entries.filter((entry) => entry?.status === 'failed').length,
    rounds,
  }
}

/**
 * Adds the active round from the top-level members when it is not archived yet.
 *
 * A round already in the archive is the final round's compatibility copy and is skipped. A
 * running container shows its active round even before a member started, so the reader sees
 * which round is under way.
 *
 * A container that failed or was cancelled after `round` advanced but before any member of the
 * new round committed has no active round to show; its last archived round then carries the
 * terminal outcome. The scheduler commits the archive and the next round's first dispatch
 * together, so that state is not expected to be persisted.
 */
function appendActiveRound(rounds: RunRound[], round: number, context: ContainerContext): void {
  if (rounds.some((candidate) => candidate.index === round)) {
    return
  }
  const live: Record<string, RunNodeEntry> = {}
  for (const memberId of context.memberIds) {
    const state = context.flat[memberId]
    if (state !== undefined) {
      live[memberId] = state
    }
  }
  if (Object.keys(live).length === 0 && context.entry.status !== 'running') {
    return
  }
  rounds.push({
    index: round,
    source: 'active',
    outcome: activeOutcome(context.entry.status),
    states: memberStates(live, context.memberIds),
  })
}

/** How the active round reads, from its container's status. */
function activeOutcome(status: RunNodeEntry['status']): RunRoundOutcome {
  if (status === 'failed' || status === 'cancelled') {
    return status
  }
  return 'running'
}

/**
 * Success reasons a Loop may record in its round document (loop ADR D3 / D4), by outcome.
 *
 * The approved D1 shape does not carry one yet; when the backend records it, it wins over the
 * inference below, and an unknown value is ignored rather than guessed at.
 */
const RECORDED_LOOP_OUTCOMES: ReadonlyMap<string, RunRoundOutcome> = new Map([
  ['condition_satisfied', 'conditionMet'],
  ['max_iterations', 'limitReached'],
  ['loop_exit', 'loopExit'],
])

/**
 * Names how a Loop's last round ended.
 *
 * A recorded success reason is used as is. Without one, only what the committed state proves
 * is said: a succeeded loopExit is an early exit; finishing before the ceiling, or at the
 * ceiling under `fail` (which would have failed instead), means `until` matched; at the ceiling
 * under `succeed` the two cannot be told apart.
 */
function finalLoopOutcome(
  last: RunRound,
  facts: {
    context: ContainerContext
    round: number
    maxIterations: number
    recorded: RunRoundOutcome | undefined
  },
): RunRoundOutcome {
  const status = facts.context.entry.status
  if (status !== 'succeeded') {
    return last.source === 'active' ? last.outcome : activeOutcome(status)
  }
  if (facts.recorded !== undefined) {
    return facts.recorded
  }
  const exited = Object.entries(last.states).some(
    ([id, state]) => state.status === 'succeeded' && facts.context.kinds.get(id) === 'loopExit',
  )
  if (exited) {
    return 'loopExit'
  }
  const behavior = facts.context.node.data.loopConfig?.maxIterationsBehavior ?? 'fail'
  return facts.round < facts.maxIterations || behavior === 'fail' ? 'conditionMet' : 'completed'
}

/** Status, members and failure reason every container carries. */
function containerBase(
  context: ContainerContext,
  kind: RunContainer['kind'],
): Pick<RunContainer, 'id' | 'kind' | 'status' | 'memberIds' | 'reason'> {
  const reason = isRecord(context.output) ? context.output['reason'] : undefined
  return {
    id: context.node.id,
    kind,
    status: context.entry.status,
    memberIds: context.memberIds,
    ...(context.entry.status !== 'succeeded' && typeof reason === 'string' ? { reason } : {}),
  }
}

/** One round's members: every member present, `inactive` unless the round recorded it. */
function memberStates(
  raw: Record<string, unknown>,
  memberIds: readonly string[],
): Record<string, RunNodeEntry> {
  const decoded = parseRunNodeStates(raw)
  const states: Record<string, RunNodeEntry> = {}
  for (const memberId of memberIds) {
    states[memberId] = decoded[memberId] ?? { status: 'inactive' }
  }
  return states
}

/** Reads `completed[]`, or `undefined` when it is not an array of `{index, states}`. */
function readArchive(
  value: unknown,
): { index: number; states: Record<string, unknown> }[] | undefined {
  if (!Array.isArray(value)) {
    return undefined
  }
  const archive: { index: number; states: Record<string, unknown> }[] = []
  for (const item of value) {
    const index = isRecord(item) ? readCount(item['index']) : undefined
    const states = isRecord(item) ? item['states'] : undefined
    if (index === undefined || !isRecord(states)) {
      return undefined
    }
    archive.push({ index, states })
  }
  return archive.toSorted((left, right) => left.index - right.index)
}

/** Reads one ledger entry; an unreadable one counts as not settled. */
function readLedgerEntry(value: unknown): RunIterationLedgerEntry | undefined {
  if (!isRecord(value)) {
    return undefined
  }
  const status = value['status']
  if (status !== 'succeeded' && status !== 'failed') {
    return undefined
  }
  const error = value['error']
  return { status, output: value['output'], error: typeof error === 'string' ? error : null }
}

/** A non-negative integer, or `undefined`. */
function readCount(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isInteger(value) && value >= 0 ? value : undefined
}

/** A non-null, non-array object. */
function isRecord(value: unknown): value is Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}
