import { describe, expect, it } from 'vitest'
import type { WorkflowNodeRun, WorkflowRunDetail } from '@/lib/workflows/api/generated'
import { normalizeWorkflowDefinition } from '@/lib/workflows/runtime'
import { toGraphWorkflowRun } from './server-run-adapter'

const definition = normalizeWorkflowDefinition({
  id: 'wf',
  name: 'v1',
  description: '',
  updatedAt: new Date(0).toISOString(),
  viewport: { x: 0, y: 0, zoom: 1 },
  nodes: [
    { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始', description: '' } },
    { id: 'a', type: 'workflow', position: { x: 1, y: 0 }, data: { kind: 'agent', title: '起草', description: '' } },
    { id: 'out', type: 'workflow', position: { x: 2, y: 0 }, data: { kind: 'output', title: '输出', description: '' } },
  ],
  edges: [
    { id: 'e1', source: 'start', target: 'a' },
    { id: 'e2', source: 'a', target: 'out' },
  ],
})

function nodeRun(over: Partial<WorkflowNodeRun>): WorkflowNodeRun {
  return {
    id: 'nr', runId: 'run', scopeId: 'root:run', nodeId: 'start', nodeType: 'start', sessionId: null, status: 'succeeded',
    input: null, output: null, error: null, payload: null, iteration: null, startedAt: 1_000, finishedAt: 2_000, createdAt: 1_000, updatedAt: 2_000,
    ...over,
  }
}

function graphNode(id: string, kind: (typeof definition.nodes)[number]['data']['kind'], parentId?: string, containerId?: string): (typeof definition.nodes)[number] {
  return { id, type: 'workflow', position: { x: 0, y: 0 }, parentId, data: { kind, title: id, description: '', containerId } }
}

function detail(over: Partial<WorkflowRunDetail['run']>, nodes: WorkflowNodeRun[], conditionDecisions: Record<string, string> = {}): WorkflowRunDetail {
  return {
    run: {
      id: 'run', workflowId: 'wf', snapshotId: 'snap', projectId: 'p', name: '运行', status: 'running', state: '{"current_nodes":["a"]}',
      input: '需求', output: null, error: null, startedAt: 1_000, finishedAt: null, createdAt: 500, updatedAt: 2_000,
      ...over,
    },
    nodes,
    scopes: [],
    conditionDecisions,
    iterationConditionDecisions: {},
  }
}

describe('toGraphWorkflowRun', () => {
  it('keeps Loop scope sessions and Condition branches separate from root and from other rounds', () => {
    const graph = normalizeWorkflowDefinition({ ...definition, nodes: [
      graphNode('start', 'start'), graphNode('loop', 'loop'),
      graphNode('entry', 'start', undefined, 'loop'), graphNode('gate', 'condition', undefined, 'loop'), graphNode('a', 'agent', undefined, 'loop'),
    ], edges: [{ id: 'outer', source: 'start', target: 'loop' }, { id: 'body', source: 'entry', target: 'gate' }, { id: 'branch', source: 'gate', sourceHandle: 'yes', target: 'a' }] })
    const wire = detail({}, [nodeRun({ id: 'parent', nodeId: 'loop', nodeType: 'loop', status: 'running' }),
      nodeRun({ id: 'gate1', scopeId: 'round1', nodeId: 'gate', nodeType: 'condition' }),
      nodeRun({ id: 'gate2', scopeId: 'round2', nodeId: 'gate', nodeType: 'condition' }),
      nodeRun({ id: 'a1', scopeId: 'round1', nodeId: 'a', nodeType: 'agent', sessionId: 'session1', output: 'first', payload: JSON.stringify({ executor: { id: 'first-executor' }, model: { name: 'first-model' }, unapplied: ['retry'] }) }),
    ], { gate: 'unrelated root decision' })
    wire.scopes = [1, 2].map((index) => ({ id: `round${index}`, runId: 'run', parentLoopNodeRunId: 'parent', roundIndex: index, status: 'succeeded', state: JSON.stringify({ conditionDecisions: { gate: index === 1 ? 'yes' : 'else' } }), createdAt: index * 1_000, updatedAt: index * 1_000 }))
    const run = toGraphWorkflowRun(wire, graph)
    expect(run.nodeStates.a).toEqual({ status: 'idle' })
    expect(run.rounds?.[0]?.nodeStates.a).toMatchObject({ nodeRunId: 'a1', sessionId: 'session1', output: { detail: 'first' }, executor: { id: 'first-executor' }, model: { name: 'first-model' }, unapplied: ['retry'] })
    expect(run.rounds?.[1]?.nodeStates.a).toEqual({ status: 'inactive' })
    expect(run.rounds?.map((round) => round.nodeStates.gate?.selectedBranchId)).toEqual(['yes', 'else'])
  })

  it('projects all Iteration members per index including skipped collect targets, without copying prior sessions', () => {
    const graph = normalizeWorkflowDefinition({ ...definition, nodes: [
      graphNode('start', 'start'), graphNode('iter', 'iteration'), graphNode('gate', 'condition', 'iter'), graphNode('a', 'agent', 'iter'),
    ], edges: [{ id: 'outer', source: 'start', target: 'iter' }, { id: 'body', source: 'iter', target: 'gate' }, { id: 'branch', source: 'gate', sourceHandle: 'yes', target: 'a' }] })
    const wire = detail({ status: 'succeeded' }, [nodeRun({ id: 'parent', nodeId: 'iter', nodeType: 'iteration' }),
      nodeRun({ id: 'gate0', nodeId: 'gate', nodeType: 'condition', iteration: 0 }),
      nodeRun({ id: 'gate1', nodeId: 'gate', nodeType: 'condition', iteration: 1 }),
      nodeRun({ id: 'a0', nodeId: 'a', nodeType: 'agent', iteration: 0, sessionId: 'first', output: 'collected', payload: JSON.stringify({ executor: { id: 'collected-executor' }, model: { name: 'collected-model' }, unapplied: ['skill'] }) }),
    ])
    wire.iterationConditionDecisions = { 'gate#0': 'yes', 'gate#1': 'else' }
    const run = toGraphWorkflowRun(wire, graph)
    expect(run.roundStates?.a).toHaveLength(2)
    expect(run.roundStates?.a?.[0]).toMatchObject({ iteration: 0, nodeRunId: 'a0', sessionId: 'first', output: { detail: 'collected' }, executor: { id: 'collected-executor' }, model: { name: 'collected-model' }, unapplied: ['skill'] })
    expect(run.roundStates?.a?.[1]).toEqual({ iteration: 1, status: 'inactive' })
    expect(run.nodeStates.a).toEqual({ iteration: 1, status: 'inactive' })
    expect(run.roundStates?.gate?.map((state) => state.selectedBranchId)).toEqual(['yes', 'else'])
  })

  it('projects the run, leaves nodes without an attempt idle and shows an Agent turn', () => {
    const run = toGraphWorkflowRun(
      detail({ status: 'succeeded', output: '{"draft":"初稿"}', finishedAt: 3_000, state: '{"current_nodes":[]}' }, [
        nodeRun({ id: 'n1', output: '需求' }),
        nodeRun({
          id: 'n2', nodeId: 'a', nodeType: 'agent', sessionId: 's-1', input: '起草：{{#start.input#}}', output: '初稿',
          payload: JSON.stringify({ stop_reason: 'end_turn', task: '起草：需求' }),
        }),
      ]),
      definition,
    )
    expect(run).toMatchObject({ id: 'run', definitionId: 'wf', snapshotId: 'snap', status: 'succeeded', kickoffInput: '需求', finalOutput: '{"draft":"初稿"}', openHitls: [] })
    expect(run.finishedAt).toBe(new Date(3_000).toISOString())
    expect(run.nodeStates.out).toEqual({ status: 'idle' })
    const a = run.nodeStates.a!
    // The attempt row is kept, so the run page can follow its session.
    expect(a).toMatchObject({ status: 'succeeded', nodeRunId: 'n2', sessionId: 's-1', stopReason: 'end_turn', output: { summary: '初稿', detail: '初稿' } })
    // The rendered task, not the template the attempt input keeps, is what the Agent was asked.
    expect(a.conversation?.map((item) => item.kind === 'message' && [item.role, item.markdown])).toEqual([
      ['user', '起草：需求'],
      ['assistant', '初稿'],
    ])
  })

  it('reads a failure detail written with the engine\'s snake_case keys', () => {
    const run = toGraphWorkflowRun(
      detail({ status: 'failed', error: '会话失败' }, [
        nodeRun({
          nodeId: 'a', nodeType: 'agent', status: 'failed', error: '会话失败',
          payload: JSON.stringify({ error_detail: { kind: 'session', message: '会话失败', source_chain: [], attempt: 1, resumable: true, injects_previous_failure: false, auto_retryable: true, recorded_at: 9 } }),
        }),
      ]),
      definition,
    )
    expect(run.status).toBe('failed')
    expect(run.nodeStates.a).toMatchObject({
      status: 'failed',
      errorMessage: '会话失败',
      errorDetail: { kind: 'session', message: '会话失败', sourceChain: [], attempt: 1, resumable: true, injectsPreviousFailure: false, autoRetryable: true, recordedAt: 9 },
    })
    expect(run.nodeStates.a!.conversation).toBeUndefined()
  })

  it('reads the executor and model an Agent ran on and the settings v2 did not apply', () => {
    const run = toGraphWorkflowRun(
      detail({ status: 'succeeded', state: '{"current_nodes":[]}' }, [
        nodeRun({
          nodeId: 'a', nodeType: 'agent', output: '初稿',
          payload: JSON.stringify({
            stop_reason: 'end_turn',
            executor: { id: 'opencode:fixture', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '1.4.0' },
            model: { id: 'fixture', name: 'Fixture', requested: 'claude-sonnet-5-5', fallback: true },
            unapplied: ['role', 'retry', 'webhook'],
          }),
        }),
      ]),
      definition,
    )
    expect(run.nodeStates.a).toMatchObject({
      executor: { id: 'opencode:fixture', agentId: 'ora/opencode', name: 'OpenCode', pluginVersion: '1.4.0' },
      model: { id: 'fixture', name: 'Fixture', requested: 'claude-sonnet-5-5', fallback: true },
      // A name this version does not know is dropped rather than shown raw.
      unapplied: ['role', 'retry'],
    })
  })

  it('records no executor or model for an attempt without them', () => {
    const run = toGraphWorkflowRun(
      detail({}, [nodeRun({ nodeId: 'a', nodeType: 'agent', payload: JSON.stringify({ executor: { name: 'OpenCode' }, model: {}, unapplied: 'role' }) })]),
      definition,
    )
    expect(run.nodeStates.a).not.toHaveProperty('executor')
    expect(run.nodeStates.a).not.toHaveProperty('model')
    expect(run.nodeStates.a).not.toHaveProperty('unapplied')
  })

  it('shows an Agent without an answer yet, and malformed payloads, without inventing anything', () => {
    const run = toGraphWorkflowRun(detail({}, [nodeRun({ nodeId: 'a', nodeType: 'agent', status: 'running', finishedAt: null, payload: '{oops' })]), definition)
    expect(run.status).toBe('running')
    expect(run.nodeStates.a).toEqual({ status: 'running', nodeRunId: 'nr', startedAt: new Date(1_000).toISOString() })
  })

  it('shows the nodes behind a Condition branch not taken as inactive, and the join they share still pending', () => {
    const branching = normalizeWorkflowDefinition({
      id: 'wf', name: 'v1', description: '', updatedAt: new Date(0).toISOString(), viewport: { x: 0, y: 0, zoom: 1 },
      nodes: [
        { id: 'start', type: 'workflow', position: { x: 0, y: 0 }, data: { kind: 'start', title: '开始', description: '' } },
        { id: 'cond', type: 'workflow', position: { x: 1, y: 0 }, data: { kind: 'condition', title: '判断', description: '', cases: [{ id: 'ok', logic: 'and', conditions: [] }] } },
        { id: 'ship', type: 'workflow', position: { x: 2, y: 0 }, data: { kind: 'agent', title: '发布', description: '' } },
        { id: 'fix', type: 'workflow', position: { x: 2, y: 1 }, data: { kind: 'agent', title: '返工', description: '' } },
        { id: 'after', type: 'workflow', position: { x: 3, y: 0 }, data: { kind: 'agent', title: '发布后', description: '' } },
        { id: 'join', type: 'workflow', position: { x: 4, y: 0 }, data: { kind: 'aggregator', title: '汇合', description: '' } },
      ],
      edges: [
        { id: 'e1', source: 'start', target: 'cond' },
        { id: 'e2', source: 'cond', sourceHandle: 'ok', target: 'ship' },
        { id: 'e3', source: 'cond', sourceHandle: 'else', target: 'fix' },
        { id: 'e4', source: 'ship', target: 'after' },
        { id: 'e5', source: 'after', target: 'join' },
        { id: 'e6', source: 'fix', target: 'join' },
      ],
    })
    const run = toGraphWorkflowRun(
      detail({}, [
        nodeRun({ id: 'n1' }),
        nodeRun({ id: 'n2', nodeId: 'cond', nodeType: 'condition' }),
        nodeRun({ id: 'n3', nodeId: 'fix', nodeType: 'agent', status: 'running', finishedAt: null }),
      ], { cond: 'else' }),
      branching,
    )
    expect(Object.fromEntries(Object.entries(run.nodeStates).map(([id, state]) => [id, state.status]))).toEqual({
      start: 'succeeded', cond: 'succeeded', ship: 'inactive', after: 'inactive', fix: 'running', join: 'idle',
    })
  })
})
