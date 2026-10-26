import { describe, expect, it } from 'vitest'
import { createDefaultWorkflowCapabilities } from '@/features/workflows/runtime/capabilities'
import { workflowNodeTypeDefinition } from '@/features/workflows/runtime/node-catalog'
import { DEFAULT_ITERATION_MAX_ITERATIONS } from '@/features/workflows/runtime/iteration-defaults'
import {
  WORKFLOW_LOOP_NODE_HEIGHT,
  WORKFLOW_LOOP_NODE_WIDTH,
} from '@/features/workflows/runtime/node-geometry'
import {
  AUTHORABLE_NODE_KINDS,
  createWorkflowLoopGroup,
  createWorkflowNode,
  nextWorkflowNodeSequence,
  workflowNodeId,
} from '@/features/workflows/runtime/node-factory'
import type { WorkflowNodeKind } from '@/features/workflows/runtime/types'

/** Stands in for the active locale's translator, so a key is visible in the assertion. */
const translate = (key: string): string => `<${key}>`

/** The origin a node is placed at unless a test cares about the position. */
const ORIGIN = { x: 40, y: 60 }

/** Creates a node of `kind` at ordinal 1. */
function create(kind: WorkflowNodeKind, sequence = 1) {
  return createWorkflowNode({ kind, sequence, position: ORIGIN, translate })
}

describe('workflowNodeId', () => {
  it('names a node by its kind and ordinal', () => {
    expect(workflowNodeId('agent', 3)).toBe('agent-3')
  })
})

describe('nextWorkflowNodeSequence', () => {
  it('starts a kind at 1 when the graph has none of it', () => {
    expect(nextWorkflowNodeSequence('agent', [])).toBe(1)
  })

  it('continues past the highest ordinal in use', () => {
    expect(nextWorkflowNodeSequence('agent', ['agent-1', 'agent-2'])).toBe(3)
  })

  it('reuses the ordinal of a deleted node, so ids stay short', () => {
    expect(nextWorkflowNodeSequence('agent', ['agent-2'])).toBe(1)
  })

  it('counts only ids of the kind being created', () => {
    expect(nextWorkflowNodeSequence('agent', ['start-1', 'output-1'])).toBe(1)
  })
})

describe('AUTHORABLE_NODE_KINDS', () => {
  it('matches the palette: the six kinds the editor can create', () => {
    expect(AUTHORABLE_NODE_KINDS).toEqual([
      'start',
      'agent',
      'condition',
      'aggregator',
      'loop',
      'iteration',
      'output',
    ])
  })
})

describe('createWorkflowNode', () => {
  it('stores the palette label and description resolved in the active locale', () => {
    const definition = workflowNodeTypeDefinition('agent')
    const node = create('agent', 2)

    expect(node.data.title).toBe(`${translate(definition.labelKey)} 2`)
    expect(node.data.description).toBe(translate(definition.descriptionKey))
  })

  it('pins every node to the one element type the canvas registers', () => {
    expect(create('agent').type).toBe('workflow')
  })

  it('copies the seed position instead of aliasing the point it was given', () => {
    const position = { x: 10, y: 20 }
    const node = createWorkflowNode({ kind: 'agent', sequence: 1, position, translate })

    position.x = 999

    expect(node.position).toEqual({ x: 10, y: 20 })
  })

  it('makes the Start node undeletable, because a graph without one cannot run', () => {
    expect(create('start').deletable).toBe(false)
  })

  it('leaves every other kind deletable', () => {
    expect(create('agent').deletable).toBeUndefined()
  })

  it('seeds a Start node with an empty kickoff prompt', () => {
    expect(create('start').data).toMatchObject({ kind: 'start', input: '' })
  })

  it('seeds an Agent node from the catalog default execution contract', () => {
    const defaults = createDefaultWorkflowCapabilities().defaultAgentConfig

    expect(create('agent').data.agentConfig).toEqual(defaults)
  })

  it('clones the agent contract, so editing one node cannot reach the catalog', () => {
    const node = create('agent')
    const config = node.data.agentConfig

    if (config === undefined) {
      throw new Error('an Agent node must carry an execution contract')
    }
    config.prompt = 'edited'

    expect(create('agent').data.agentConfig?.prompt).toBe('')
  })

  it('takes the agent contract a caller supplies, such as the workspace model list', () => {
    const supplied = { ...createDefaultWorkflowCapabilities().defaultAgentConfig, prompt: 'hi' }
    const node = createWorkflowNode({
      kind: 'agent',
      sequence: 1,
      position: ORIGIN,
      translate,
      agentConfig: supplied,
    })

    expect(node.data.agentConfig?.prompt).toBe('hi')
    expect(node.data.agentConfig).not.toBe(supplied)
  })

  it('seeds an Output node with nothing beyond its identity', () => {
    expect(create('output').data).toEqual({
      kind: 'output',
      title: `${translate(workflowNodeTypeDefinition('output').labelKey)} 1`,
      description: translate(workflowNodeTypeDefinition('output').descriptionKey),
    })
  })

  it('seeds a Condition node with a named rule and one empty IF branch', () => {
    expect(create('condition').data).toMatchObject({
      kind: 'condition',
      condition: translate('workflows.node.condition.label'),
      cases: [{ id: 'case-1', logic: 'and', conditions: [] }],
    })
  })

  it('seeds an Iteration node with empty selectors and the safety ceiling', () => {
    expect(create('iteration').data).toMatchObject({
      kind: 'iteration',
      iterationConfig: {
        iteratorSelector: [],
        collectSelector: [],
        errorStrategy: 'fail',
        maxIterations: DEFAULT_ITERATION_MAX_ITERATIONS,
      },
    })
  })

  it('seeds a bare Loop node with no body, which the group factory replaces', () => {
    expect(create('loop').data.loopConfig).toBeUndefined()
  })

  it('refuses a kind the editor does not offer, such as a Tool node', () => {
    expect(() => create('tool')).toThrow('tool')
    expect(() => create('human')).toThrow('human')
  })
})

describe('createWorkflowLoopGroup', () => {
  it('builds the container, two children, and the internal edge in one call', () => {
    const group = createWorkflowLoopGroup({
      kind: 'loop',
      sequence: 1,
      position: ORIGIN,
      translate,
    })
    expect(group.nodes.map((node) => node.data.kind)).toEqual(['loop', 'start', 'agent'])
    expect(group.edges).toEqual([
      {
        id: 'e-loop-1-start-loop-1-agent',
        source: 'loop-1-start',
        target: 'loop-1-agent',
        type: 'workflow',
      },
    ])
  })

  it('gives each child the authored frame size and the container ownership', () => {
    const group = createWorkflowLoopGroup({
      kind: 'loop',
      sequence: 2,
      position: ORIGIN,
      translate,
    })
    const [loop, childStart, childAgent] = group.nodes
    expect(loop?.initialWidth).toBe(WORKFLOW_LOOP_NODE_WIDTH)
    expect(loop?.initialHeight).toBe(WORKFLOW_LOOP_NODE_HEIGHT)
    expect(childStart?.data.containerId).toBe('loop-2')
    expect(childStart?.parentId).toBe('loop-2')
    expect(childAgent?.data.containerId).toBe('loop-2')
    expect(childAgent?.parentId).toBe('loop-2')
  })

  it('wires the loop so the Agent reads the carried variable on its first probe', () => {
    const group = createWorkflowLoopGroup({
      kind: 'loop',
      sequence: 1,
      position: ORIGIN,
      translate,
    })
    const agent = group.nodes.at(-1)
    expect(agent?.data.agentConfig?.prompt).toBe(
      translate('workflows.loop.promptTemplate').replace('NODE_ID', 'loop-1'),
    )
    expect(group.nodes[0]?.data.loopConfig).toMatchObject({
      maxIterations: 3,
      variables: [{ name: 'value', feedback: ['loop-1-agent', 'output'] }],
      until: {
        logic: 'and',
        conditions: [{ variableSelector: ['loop-1-agent', 'output'], operator: 'not_empty' }],
      },
      outputs: [{ name: 'result', variableSelector: ['loop-1-agent', 'output'] }],
    })
  })

  it('clones the child Agent contract, so editing one group cannot reach another', () => {
    const group = createWorkflowLoopGroup({
      kind: 'loop',
      sequence: 1,
      position: ORIGIN,
      translate,
    })
    const agent = group.nodes.at(-1)?.data.agentConfig

    if (agent === undefined) {
      throw new Error('a Loop group must carry an executable child Agent')
    }
    agent.prompt = 'edited'

    const second = createWorkflowLoopGroup({
      kind: 'loop',
      sequence: 1,
      position: ORIGIN,
      translate,
    })
    expect(second.nodes.at(-1)?.data.agentConfig?.prompt).not.toBe('edited')
  })
})
