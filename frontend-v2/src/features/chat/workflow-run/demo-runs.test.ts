import { waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import {
  cancelDemoRun,
  createDemoRun,
  deleteDemoRun,
  demoRunClock,
  listPublishedWorkflows,
  rerunDemoRun,
  resetDemoRuns,
  startDemoRun,
  updateDemoRunInput,
  useDemoRuns,
  type PublishedWorkflow,
} from './demo-runs'
import type { WorkflowContracts } from '@/lib/workflows/data/contracts-client'
import { localWorkflowContracts, sampleWorkflowBackend } from '@/lib/workflows/test/workflow-server'

let library: WorkflowContracts
let review: PublishedWorkflow

const sample = () => review

describe('demo workflow runs', () => {
  beforeEach(async () => {
    library = localWorkflowContracts(await sampleWorkflowBackend())
    const found = (await listPublishedWorkflows(library)).find((w) => w.name === '示例：代码审查')
    if (!found) throw new Error('the review sample is missing')
    review = found
    resetDemoRuns()
    demoRunClock.nodeStepMs = 5
  })
  afterEach(() => {
    resetDemoRuns()
    demoRunClock.nodeStepMs = 1500
  })

  it('offers the published workflows of the library', async () => {
    expect((await listPublishedWorkflows(library)).map((w) => [w.name, w.version])).toContainEqual(['示例：代码审查', 'v1'])
  })

  it('refuses a run when the active version changed after the menu offered it', async () => {
    await expect(createDemoRun(library, 'cloud', { ...sample(), version: 'v0' }, '')).rejects.toThrow(
      '生效版本已从 v0 变为 v1，请重新打开菜单选择',
    )
    expect(useDemoRuns.getState().runs).toEqual({})
  })

  it('creates a pending run of the published graph under the given name', async () => {
    const run = await createDemoRun(library, 'cloud', sample(), '周五的审查')
    expect(run).toMatchObject({ projectId: 'cloud', name: '周五的审查', status: 'pending' })
    expect(useDemoRuns.getState().runs[run.id]?.status).toBe('pending')
    expect(Object.values(run.nodeStates).every((s) => s.status === 'idle')).toBe(true)
  })

  it('plays a started run to the end, with the Agent conversation and the Output result', async () => {
    const run = await createDemoRun(library, 'cloud', sample(), '')
    await startDemoRun(run.id, '审查 PR 的改动', {})
    await waitFor(() => expect(useDemoRuns.getState().runs[run.id]?.status).toBe('succeeded'))

    const finished = useDemoRuns.getState().runs[run.id]!
    const nodes = finished.definitionSnapshot.nodes
    const start = nodes.find((n) => n.data.kind === 'start')!
    expect(start.data.input).toBe('审查 PR 的改动')
    const agents = nodes.filter((n) => n.data.kind === 'agent')
    const conversation = useDemoRuns.getState().conversations[run.id] ?? []
    for (const agent of agents.filter((n) => finished.nodeStates[n.id]?.status === 'succeeded')) {
      expect(conversation.some((item) => item.nodeId === agent.id && item.kind === 'message' && item.role === 'assistant')).toBe(true)
    }
    const output = nodes.find((n) => n.data.kind === 'output')!
    expect(finished.nodeStates[output.id]).toMatchObject({ status: 'succeeded', output: expect.anything() })

    // The condition took one branch; the other one's nodes are inactive, so the path is complete.
    const condition = nodes.find((n) => n.data.kind === 'condition')!
    const branches = finished.definitionSnapshot.edges.filter((e) => e.source === condition.id).map((e) => finished.nodeStates[e.target]?.status)
    expect(branches.sort()).toEqual(['inactive', 'succeeded'])
    expect(Object.values(finished.nodeStates).every((state) => state.status === 'succeeded' || state.status === 'inactive')).toBe(true)

    // Theater lists each node's artifacts and brings the newest into view.
    const artifacts = useDemoRuns.getState().artifacts[run.id] ?? []
    expect(artifacts.length).toBeGreaterThan(0)
    expect(useDemoRuns.getState().revealedArtifactIds[run.id]).toBe(artifacts.at(-1)?.id)
  })

  it('saves the kickoff input edited before the run starts', async () => {
    const run = await createDemoRun(library, 'cloud', sample(), '')
    await updateDemoRunInput(run.id, '只看 v2 目录', { repository: 'ora-space/desktop' })

    const start = useDemoRuns.getState().runs[run.id]!.definitionSnapshot.nodes.find((n) => n.data.kind === 'start')!
    expect(start.data.input).toBe('只看 v2 目录')
    expect(useDemoRuns.getState().inputs[run.id]).toEqual({ repository: 'ora-space/desktop' })
  })

  it('stops a running run and starts a fresh pending one on rerun, keeping the old one', async () => {
    demoRunClock.nodeStepMs = 60_000
    resetDemoRuns()
    const run = await createDemoRun(library, 'cloud', sample(), '')
    await startDemoRun(run.id, '', {})
    await waitFor(() => expect(useDemoRuns.getState().runs[run.id]?.status).toBe('running'))
    await cancelDemoRun(run.id)
    expect(useDemoRuns.getState().runs[run.id]?.status).toBe('cancelled')

    const again = await rerunDemoRun(run.id)
    expect(again).toMatchObject({ name: run.name, status: 'pending' })
    expect(again.id).not.toBe(run.id)
    expect(useDemoRuns.getState().runs[run.id]?.status).toBe('cancelled')
  })

  it('deletes a run with its conversation', async () => {
    const run = await createDemoRun(library, 'cloud', sample(), '')
    await deleteDemoRun(run.id)
    expect(useDemoRuns.getState().runs[run.id]).toBeUndefined()
    expect(useDemoRuns.getState().conversations[run.id]).toBeUndefined()
    expect(useDemoRuns.getState().artifacts[run.id]).toBeUndefined()
  })
})
