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

function detail(over: Partial<WorkflowRunDetail['run']>, nodes: WorkflowNodeRun[]): WorkflowRunDetail {
  return {
    run: {
      id: 'run', workflowId: 'wf', snapshotId: 'snap', projectId: 'p', name: '运行', status: 'running', state: '{"current_nodes":["a"]}',
      input: '需求', output: null, error: null, startedAt: 1_000, finishedAt: null, createdAt: 500, updatedAt: 2_000,
      ...over,
    },
    nodes,
    scopes: [],
  }
}

describe('toGraphWorkflowRun', () => {
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
    expect(a).toMatchObject({ status: 'succeeded', sessionId: 's-1', stopReason: 'end_turn', output: { summary: '初稿', detail: '初稿' } })
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
    expect(run.nodeStates.a).toEqual({ status: 'running', startedAt: new Date(1_000).toISOString() })
  })
})
