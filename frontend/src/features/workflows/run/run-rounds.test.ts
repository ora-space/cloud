import { describe, expect, it } from 'vitest'
import {
  parseRunContainers,
  projectRoundStates,
  selectedRound,
  type RunFrozenNode,
} from '@/features/workflows/run/run-rounds'
import { parseRunNodeStates } from '@/features/workflows/run/run-node-state'
import type { WorkflowLoopConfig, WorkflowNodeData } from '@/features/workflows/runtime/types'

/** A frozen node; members name their container on `containerId`, as the factory writes it. */
function frozen(id: string, kind: WorkflowNodeData['kind'], containerId?: string): RunFrozenNode {
  return {
    id,
    data: {
      kind,
      title: id,
      description: '',
      ...(containerId === undefined ? {} : { containerId }),
    },
  }
}

/** A Loop node with its ceiling and exhaustion strategy. */
function loopNode(
  maxIterations: number,
  maxIterationsBehavior: WorkflowLoopConfig['maxIterationsBehavior'] = 'fail',
): RunFrozenNode {
  const node = frozen('loop-1', 'loop')
  node.data.loopConfig = {
    maxIterations,
    maxIterationsBehavior,
    variables: [],
    until: { logic: 'and', conditions: [] },
    outputs: [],
  }
  return node
}

/** The loop of loop ADR D1: an entry, a writer and an early exit. */
function loopSnapshot(
  maxIterations = 5,
  behavior: WorkflowLoopConfig['maxIterationsBehavior'] = 'fail',
) {
  return [
    loopNode(maxIterations, behavior),
    frozen('entry', 'start', 'loop-1'),
    frozen('writer', 'agent', 'loop-1'),
    frozen('exit', 'loopExit', 'loop-1'),
    frozen('outer', 'agent'),
  ]
}

const OK = { status: 'succeeded' }

/** One archived Loop round. */
function loopRound(index: number, states: Record<string, unknown>) {
  return { index, carried: { draft: `round ${index}` }, states }
}

describe('parseRunContainers for a Loop', () => {
  it('reads the ADR D1 example as one archived round and one active round', () => {
    const raw = {
      'loop-1': {
        status: 'running',
        loop: {
          round: 2,
          maxIterations: 5,
          carried: { draft: '第一轮产出' },
          completed: [loopRound(1, { entry: OK, writer: { status: 'succeeded', output: 'a' } })],
        },
      },
      entry: OK,
      writer: { status: 'running' },
    }
    const loop = parseRunContainers(raw, loopSnapshot())['loop-1']

    expect(loop?.rounds.map((round) => [round.index, round.source, round.outcome])).toEqual([
      [1, 'archived', 'continued'],
      [2, 'active', 'running'],
    ])
    expect(loop?.rounds[0]?.states).toEqual({
      entry: { status: 'succeeded' },
      writer: { status: 'succeeded', output: 'a' },
      exit: { status: 'inactive' },
    })
    expect(loop?.rounds[1]?.states['writer']).toEqual({ status: 'running' })
    expect(loop?.limit).toBe(5)
  })

  it('shows the final round once when it is both archived and left at the top level', () => {
    const raw = {
      'loop-1': {
        status: 'succeeded',
        output: { result: 'done' },
        loop: {
          round: 2,
          maxIterations: 5,
          carried: {},
          completed: [
            loopRound(1, { entry: OK, writer: OK }),
            loopRound(2, { entry: OK, writer: OK }),
          ],
        },
      },
      entry: OK,
      writer: OK,
    }
    const loop = parseRunContainers(raw, loopSnapshot())['loop-1']

    expect(loop?.rounds.map((round) => [round.index, round.outcome])).toEqual([
      [1, 'continued'],
      [2, 'conditionMet'],
    ])
    expect(loop?.reason).toBeUndefined()
  })

  it('names an early exit from the succeeded loopExit, leaving skipped members inactive', () => {
    const raw = {
      'loop-1': {
        status: 'succeeded',
        loop: {
          round: 1,
          maxIterations: 5,
          carried: {},
          completed: [loopRound(1, { entry: OK, exit: OK, writer: { status: 'inactive' } })],
        },
      },
    }
    const round = parseRunContainers(raw, loopSnapshot())['loop-1']?.rounds[0]

    expect(round?.outcome).toBe('loopExit')
    expect(round?.states['writer']).toEqual({ status: 'inactive' })
  })

  it.each([
    ['fail', 'conditionMet'],
    ['succeed', 'completed'],
  ] as const)(
    'at the ceiling under %s, says only what the state proves (%s)',
    (behavior, outcome) => {
      const raw = {
        'loop-1': {
          status: 'succeeded',
          loop: {
            round: 2,
            maxIterations: 2,
            carried: {},
            completed: [loopRound(1, { entry: OK }), loopRound(2, { entry: OK })],
          },
        },
      }
      expect(
        parseRunContainers(raw, loopSnapshot(2, behavior))['loop-1']?.rounds.at(-1)?.outcome,
      ).toBe(outcome)
    },
  )

  /** The outcome of a loop that succeeded on its last round under `succeed`, with a recorded reason. */
  function outcomeAtCeiling(reason: string): string | undefined {
    const raw = {
      'loop-1': {
        status: 'succeeded',
        loop: {
          round: 2,
          maxIterations: 2,
          reason,
          carried: {},
          completed: [loopRound(1, { entry: OK }), loopRound(2, { entry: OK })],
        },
      },
    }
    return parseRunContainers(raw, loopSnapshot(2, 'succeed'))['loop-1']?.rounds.at(-1)?.outcome
  }

  it.each([
    ['max_iterations', 'limitReached'],
    ['condition_satisfied', 'conditionMet'],
    ['loop_exit', 'loopExit'],
  ] as const)('uses a recorded success reason %s over the inference', (reason, outcome) => {
    expect(outcomeAtCeiling(reason)).toBe(outcome)
  })

  it('ignores an unknown recorded reason and falls back to the inference', () => {
    expect(outcomeAtCeiling('something_new')).toBe('completed')
  })

  it('keeps archived rounds and the stable reason when the loop is cancelled mid-round', () => {
    const raw = {
      'loop-1': {
        status: 'cancelled',
        loop: { round: 2, maxIterations: 5, carried: {}, completed: [loopRound(1, { entry: OK })] },
      },
      entry: OK,
      writer: { status: 'cancelled' },
    }
    const loop = parseRunContainers(raw, loopSnapshot())['loop-1']

    expect(loop?.status).toBe('cancelled')
    expect(loop?.rounds.map((round) => [round.index, round.source, round.outcome])).toEqual([
      [1, 'archived', 'continued'],
      [2, 'active', 'cancelled'],
    ])
  })

  it('surfaces a state-size failure with its reason and the rounds it had archived', () => {
    const raw = {
      'loop-1': {
        status: 'failed',
        error: 'state too large',
        output: { reason: 'workflow_state_size_exceeded' },
        loop: { round: 1, maxIterations: 5, carried: {}, completed: [loopRound(1, { entry: OK })] },
      },
    }
    const loop = parseRunContainers(raw, loopSnapshot())['loop-1']

    expect(loop?.reason).toBe('workflow_state_size_exceeded')
    expect(loop?.rounds.map((round) => round.outcome)).toEqual(['failed'])
  })

  it.each([
    ['no loop document', { status: 'running' }],
    [
      'a non-numeric round',
      { status: 'running', loop: { round: 'x', maxIterations: 5, completed: [] } },
    ],
    [
      'an archive that is not a list',
      { status: 'running', loop: { round: 1, maxIterations: 5, completed: {} } },
    ],
    [
      'an archive entry without states',
      { status: 'running', loop: { round: 1, maxIterations: 5, completed: [{ index: 1 }] } },
    ],
  ])('leaves a loop with %s to the flat view', (_name, row) => {
    expect(parseRunContainers({ 'loop-1': row }, loopSnapshot())).toEqual({})
  })
})

/** An iteration of three items whose body is one aggregator. */
function iterationSnapshot() {
  return [frozen('iter-1', 'iteration'), frozen('pick', 'aggregator', 'iter-1')]
}

/** One archived Iteration round. */
function iterationRound(index: number, status = 'succeeded') {
  return { index, states: { pick: { status } } }
}

describe('parseRunContainers for an Iteration', () => {
  it('aligns the ledger with the rounds and marks absorbed failures under continue', () => {
    const raw = {
      'iter-1': {
        status: 'succeeded',
        iteration: {
          round: 2,
          items: ['甲', '乙', '丙'],
          strategy: 'continue',
          failedCount: 1,
          output: ['甲', '丙'],
          ledger: [
            { item: '甲', status: 'succeeded', output: '甲', error: null },
            { item: '乙', status: 'failed', output: null, error: 'aggregator_no_match' },
            { item: '丙', status: 'succeeded', output: '丙', error: null },
          ],
          completed: [iterationRound(0), iterationRound(1, 'failed'), iterationRound(2)],
        },
      },
      pick: OK,
    }
    const iteration = parseRunContainers(raw, iterationSnapshot())['iter-1']

    expect(iteration?.rounds.map((round) => [round.index, round.item, round.outcome])).toEqual([
      [0, '甲', 'succeeded'],
      [1, '乙', 'absorbed'],
      [2, '丙', 'succeeded'],
    ])
    expect(iteration?.rounds[1]?.entry).toEqual({
      status: 'failed',
      output: null,
      error: 'aggregator_no_match',
    })
    expect([iteration?.limit, iteration?.settled, iteration?.failed]).toEqual([3, 3, 1])
  })

  /** A failed container whose second of three items failed, under the given strategy. */
  function secondItemFailed(strategy: string, round: number, reason: string) {
    const raw = {
      'iter-1': {
        status: 'failed',
        output: { reason },
        iteration: {
          round,
          items: ['甲', '乙', '丙'],
          strategy,
          failedCount: 1,
          output: ['甲'],
          ledger: [
            { item: '甲', status: 'succeeded', output: '甲', error: null },
            { item: '乙', status: 'failed', output: null, error: 'no match' },
          ],
          completed: [iterationRound(0), iterationRound(1, 'failed')],
        },
      },
    }
    return parseRunContainers(raw, iterationSnapshot())['iter-1']
  }

  it('stops at the failed item under fail, with no later rounds', () => {
    const iteration = secondItemFailed('fail', 1, 'aggregator_no_match')

    expect(iteration?.rounds.map((round) => round.outcome)).toEqual(['succeeded', 'failed'])
    expect(iteration?.reason).toBe('aggregator_no_match')
  })

  it('keeps an absorbed item absorbed when the container later fails for another reason', () => {
    // Under continue the round advanced past the absorbed item before the container failed.
    const iteration = secondItemFailed('continue', 2, 'workflow_state_size_exceeded')

    expect(iteration?.rounds.map((round) => round.outcome)).toEqual(['succeeded', 'absorbed'])
    expect(iteration?.reason).toBe('workflow_state_size_exceeded')
  })

  it('has no rounds for an empty source', () => {
    const raw = {
      'iter-1': {
        status: 'succeeded',
        iteration: { round: 0, items: [], ledger: [], output: [], failedCount: 0, completed: [] },
      },
    }
    const iteration = parseRunContainers(raw, iterationSnapshot())['iter-1']

    expect(iteration?.rounds).toEqual([])
    expect(iteration?.limit).toBe(0)
  })

  it('shows the item under way as the active round', () => {
    const raw = {
      'iter-1': {
        status: 'running',
        iteration: {
          round: 1,
          items: ['甲', '乙'],
          ledger: [{ item: '甲', status: 'succeeded', output: '甲', error: null }],
          output: ['甲'],
          failedCount: 0,
          completed: [iterationRound(0)],
        },
      },
      pick: { status: 'running' },
    }
    const active = parseRunContainers(raw, iterationSnapshot())['iter-1']?.rounds.at(-1)

    expect(active).toEqual({
      index: 1,
      source: 'active',
      outcome: 'running',
      states: { pick: { status: 'running' } },
      item: '乙',
    })
  })
})

describe('projectRoundStates', () => {
  const raw = {
    'loop-1': {
      status: 'running',
      loop: {
        round: 2,
        maxIterations: 5,
        carried: {},
        completed: [loopRound(1, { entry: OK, writer: { status: 'failed', error: 'first' } })],
      },
    },
    entry: OK,
    writer: { status: 'running' },
    outer: OK,
  }
  const states = parseRunNodeStates(raw)
  const containers = parseRunContainers(raw, loopSnapshot())

  it('paints the latest round by default and leaves other nodes as they are', () => {
    expect(projectRoundStates(states, containers, {})).toMatchObject({
      'loop-1': { status: 'running' },
      writer: { status: 'running' },
      exit: { status: 'inactive' },
      outer: { status: 'succeeded' },
    })
  })

  it('paints the chosen round, never borrowing the top-level state', () => {
    const projected = projectRoundStates(states, containers, { 'loop-1': 1 })
    expect(projected['writer']).toEqual({ status: 'failed', error: 'first' })
    expect(projected['exit']).toEqual({ status: 'inactive' })
  })

  it('falls back to the latest round when the chosen one no longer exists', () => {
    const loop = containers['loop-1']
    expect(loop === undefined ? undefined : selectedRound(loop, { 'loop-1': 9 })?.index).toBe(2)
  })
})
