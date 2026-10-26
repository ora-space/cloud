import { describe, expect, it } from 'vitest'
import {
  DEFAULT_WORKFLOW_GLOBAL_VARIABLES,
  decorateWorkflowVariableCatalog,
  deriveWorkflowVariableCatalog,
  normalizeWorkflowGlobalVariables,
} from '@/features/workflows/runtime/variable-catalog'
import type {
  WorkflowCatalogEdge,
  WorkflowCatalogNode,
  WorkflowVariableCatalogEntry,
} from '@/features/workflows/runtime/variable-catalog'
import type {
  WorkflowGlobalVariable,
  WorkflowNodeData,
  WorkflowNodeKind,
} from '@/features/workflows/runtime/types'

/** Builds one graph node carrying only the fields the catalog reads. */
function node(
  id: string,
  kind: WorkflowNodeKind,
  data: Partial<WorkflowNodeData> = {},
): WorkflowCatalogNode {
  return { id, data: { kind, title: id, description: '', ...data } }
}

/** Builds one edge between two node ids. */
function edge(source: string, target: string): WorkflowCatalogEdge {
  return { source, target }
}

/** Builds one agent node carrying the structured-output contract under test. */
function structuredAgent(id: string, schema: Record<string, unknown>): WorkflowCatalogNode {
  return node(id, 'agent', {
    agentConfig: {
      schemaVersion: 3,
      executor: { agentCli: 'cli', modelId: 'm' },
      roleId: 'Reviewer',
      skills: [],
      mcps: [],
      prompt: '',
      outputContract: { type: 'structured', schema },
    },
  })
}

/** The selectors a derivation produced, in order. */
function selectors(entries: readonly WorkflowVariableCatalogEntry[]): string[] {
  return entries.map((entry) => entry.selector.join('.'))
}

/** Resolves one entry by its dotted selector, or fails naming what was derived. */
function entryFor(
  entries: readonly WorkflowVariableCatalogEntry[],
  selector: string,
): WorkflowVariableCatalogEntry {
  const found = entries.find((entry) => entry.selector.join('.') === selector)
  if (found === undefined) {
    throw new Error(`selector ${selector} missing from ${JSON.stringify(selectors(entries))}`)
  }
  return found
}

/** Two globals, one of which the graph also happens to declare as a variable name. */
const GLOBALS: WorkflowGlobalVariable[] = [{ name: 'sys.tenant', valueType: 'string' }]

describe('normalizeWorkflowGlobalVariables', () => {
  it('restores the required system globals without dropping user declarations', () => {
    const normalized = normalizeWorkflowGlobalVariables([
      { name: 'team.token', valueType: 'secret' },
    ])
    expect(normalized.map((variable) => variable.name)).toEqual([
      'team.token',
      'sys.workflow_id',
      'sys.timestamp',
    ])
  })

  it('lets the required global replace a user declaration of the same name', () => {
    // A required global's type is a runtime guarantee, not an authorable choice: declaring
    // `sys.timestamp` again must not weaken it, and must not create a duplicate row either.
    const normalized = normalizeWorkflowGlobalVariables([
      { name: 'sys.timestamp', valueType: 'string', value: 'fixed' },
      { name: 'team.token', valueType: 'secret' },
    ])
    expect(normalized).toEqual([
      { name: 'sys.timestamp', valueType: 'number' },
      { name: 'team.token', valueType: 'secret' },
      { name: 'sys.workflow_id', valueType: 'string' },
    ])
  })

  it('treats a missing declaration list as empty', () => {
    expect(normalizeWorkflowGlobalVariables(undefined)).toEqual(DEFAULT_WORKFLOW_GLOBAL_VARIABLES)
  })
})

describe('deriveWorkflowVariableCatalog', () => {
  it('offers globals first, then each upstream node in graph order', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        node('start-1', 'start', { inputVariables: [{ name: 'repository', valueType: 'string' }] }),
        node('agent-1', 'agent'),
      ],
      [edge('start-1', 'agent-1')],
      'agent-1',
      GLOBALS,
    )
    expect(selectors(entries)).toEqual(['sys.tenant', 'start-1.input', 'start-1.repository'])
  })

  it('drops a global whose name has no root, so it cannot render as an empty row', () => {
    const entries = deriveWorkflowVariableCatalog([], [], undefined, [
      { name: 'singleton', valueType: 'string' },
    ])
    expect(selectors(entries)).toEqual([])
  })

  it('ignores an undeclared Start variable name but keeps the ones that are declared', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        node('start-1', 'start', {
          inputVariables: [
            { name: '  ', valueType: 'string' },
            { name: ' branch ', valueType: 'string' },
          ],
        }),
      ],
      [],
      undefined,
      [],
    )
    expect(selectors(entries)).toEqual(['start-1.input', 'start-1.branch'])
  })

  it('hides a downstream node from its upstream consumer', () => {
    const entries = deriveWorkflowVariableCatalog(
      [node('start-1', 'start'), node('agent-1', 'agent'), node('output-1', 'output')],
      [edge('start-1', 'agent-1'), edge('agent-1', 'output-1')],
      'agent-1',
      [],
    )
    expect(selectors(entries)).toEqual(['start-1.input'])
  })

  it('reaches through a Condition, which is value-transparent', () => {
    const entries = deriveWorkflowVariableCatalog(
      [node('start-1', 'start'), node('condition-1', 'condition'), node('agent-1', 'agent')],
      [edge('start-1', 'condition-1'), edge('condition-1', 'agent-1')],
      'agent-1',
      [],
    )
    // The Start is visible through the condition, and the condition itself contributes no value:
    // its branch decision is scheduler state, not business data.
    expect(selectors(entries)).toEqual(['start-1.input'])
  })

  it('terminates on a cycle the author drew but has not fixed yet', () => {
    const entries = deriveWorkflowVariableCatalog(
      [node('agent-1', 'agent'), node('agent-2', 'agent')],
      [edge('agent-1', 'agent-2'), edge('agent-2', 'agent-1')],
      'agent-1',
      [],
    )
    expect(selectors(entries)).toEqual(['agent-2.output'])
  })

  it('ignores an edge whose source is not a node in the graph', () => {
    const entries = deriveWorkflowVariableCatalog(
      [node('agent-1', 'agent')],
      [edge('missing-1', 'agent-1')],
      'agent-1',
      [],
    )
    expect(selectors(entries)).toEqual([])
  })

  it('exposes an agent structured-output contract as an object plus its leaves', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        structuredAgent('agent-1', {
          type: 'object',
          properties: {
            verdict: { type: 'string' },
            findings: { type: 'array', items: { type: 'string' } },
            meta: { type: 'object', properties: { score: { type: 'number' } } },
          },
        }),
      ],
      [],
      undefined,
      [],
    )
    expect(selectors(entries)).toEqual([
      'agent-1.output',
      'agent-1.structured_output',
      'agent-1.structured_output.verdict',
      'agent-1.structured_output.findings',
      'agent-1.structured_output.meta',
      'agent-1.structured_output.meta.score',
    ])
    expect(entryFor(entries, 'agent-1.structured_output.findings').valueType).toBe('array[string]')
    expect(entryFor(entries, 'agent-1.structured_output.meta.score').valueType).toBe('number')
  })

  it('skips schema fragments the supported subset does not describe', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        structuredAgent('agent-1', {
          type: 'object',
          properties: {
            unnamed: {},
            listed: { type: 'array' },
            listedAny: { type: 'array', items: { type: 'any' } },
            listedOdd: { type: 'array', items: { type: 'null' } },
            unknown: { type: 'nonsense' },
          },
        }),
      ],
      [],
      undefined,
      [],
    )
    expect(entryFor(entries, 'agent-1.structured_output.unnamed').valueType).toBe('any')
    expect(entryFor(entries, 'agent-1.structured_output.listed').valueType).toBe('array')
    expect(entryFor(entries, 'agent-1.structured_output.listedAny').valueType).toBe('array[any]')
    expect(entryFor(entries, 'agent-1.structured_output.listedOdd').valueType).toBe('array')
    expect(entryFor(entries, 'agent-1.structured_output.unknown').valueType).toBe('any')
  })

  it('offers no structured entries for an agent without the contract', () => {
    const entries = deriveWorkflowVariableCatalog([node('agent-1', 'agent')], [], undefined, [])
    expect(selectors(entries)).toEqual(['agent-1.output'])
  })
})

describe('deriveWorkflowVariableCatalog iteration scope', () => {
  /** A region whose member is an agent, plus an outer consumer. */
  function region(memberData: Partial<WorkflowNodeData> = {}): WorkflowCatalogNode[] {
    return [
      node('start-1', 'start', { inputVariables: [{ name: 'files', valueType: 'array[file]' }] }),
      node('iteration-1', 'iteration', {
        iterationConfig: {
          iteratorSelector: ['start-1', 'files'],
          collectSelector: ['agent-1', 'output'],
          errorStrategy: 'fail',
          maxIterations: 10,
        },
      }),
      { ...node('agent-1', 'agent', memberData), parentId: 'iteration-1' },
    ]
  }

  it('gives a member the round bindings typed from the iterator source', () => {
    const entries = deriveWorkflowVariableCatalog(
      region(),
      [edge('start-1', 'iteration-1'), edge('iteration-1', 'agent-1')],
      'agent-1',
      [],
    )
    expect(entryFor(entries, 'iteration-1.item').valueType).toBe('file')
    expect(entryFor(entries, 'iteration-1.index').valueType).toBe('number')
  })

  it('falls back to any when the iterator source declares no type', () => {
    const nodes = region()
    const entries = deriveWorkflowVariableCatalog(nodes, [], 'agent-1', [])
    // The region's own config is intact here; only the source selector is unresolvable.
    expect(entryFor(entries, 'iteration-1.item').valueType).toBe('file')

    const untyped = deriveWorkflowVariableCatalog(
      [node('iteration-1', 'iteration'), { ...node('agent-1', 'agent'), parentId: 'iteration-1' }],
      [],
      'agent-1',
      [],
    )
    expect(entryFor(untyped, 'iteration-1.item').valueType).toBe('any')
  })

  it('gives an outer consumer the three exposed results, typed from the collect target', () => {
    const entries = deriveWorkflowVariableCatalog(
      region(),
      [
        edge('start-1', 'iteration-1'),
        edge('iteration-1', 'agent-1'),
        edge('iteration-1', 'output-1'),
      ],
      'output-1',
      [],
    )
    expect(entryFor(entries, 'iteration-1.output').valueType).toBe('array[string]')
    expect(entryFor(entries, 'iteration-1.entries').valueType).toBe('array[object]')
    expect(entryFor(entries, 'iteration-1.failed_count').valueType).toBe('number')
    // A member's products stay region-visible, so the outer consumer never sees the agent.
    expect(selectors(entries)).not.toContain('agent-1.output')
  })

  it('types the collect target from a Start variable or a structured contract', () => {
    const structured = deriveWorkflowVariableCatalog(
      [
        node('start-1', 'start', { inputVariables: [{ name: 'files', valueType: 'array[file]' }] }),
        node('iteration-1', 'iteration', {
          iterationConfig: {
            iteratorSelector: ['start-1', 'files'],
            collectSelector: ['agent-1', 'structured_output'],
            errorStrategy: 'fail',
            maxIterations: 10,
          },
        }),
        { ...node('agent-1', 'agent'), parentId: 'iteration-1' },
        node('output-1', 'output'),
      ],
      [edge('iteration-1', 'output-1')],
      'output-1',
      [],
    )
    expect(entryFor(structured, 'iteration-1.output').valueType).toBe('array[object]')
  })

  it('lets a dangling collect target degrade to any, ignoring a variable name it looks known', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        node('iteration-1', 'iteration', {
          iterationConfig: {
            iteratorSelector: ['start-1', 'files'],
            collectSelector: ['ghost-1', 'output'],
            errorStrategy: 'fail',
            maxIterations: 10,
          },
        }),
        node('output-1', 'output'),
      ],
      [edge('iteration-1', 'output-1')],
      'output-1',
      [],
    )
    // `output` alone would normally type as string, but the node it points at is gone, so the
    // element type is unknowable and the exposed result falls back to `any`.
    expect(entryFor(entries, 'iteration-1.output').valueType).toBe('array[any]')
  })

  it('exposes nothing for a region with no configuration', () => {
    const entries = deriveWorkflowVariableCatalog(
      [node('iteration-1', 'iteration'), node('output-1', 'output')],
      [edge('iteration-1', 'output-1')],
      'output-1',
      [],
    )
    expect(selectors(entries)).toEqual([])
  })

  it('hides one region member from a member of a different region', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        node('iteration-1', 'iteration'),
        { ...node('agent-1', 'agent'), parentId: 'iteration-1' },
        node('iteration-2', 'iteration'),
        { ...node('agent-2', 'agent'), parentId: 'iteration-2' },
      ],
      [edge('agent-1', 'agent-2')],
      'agent-2',
      [],
    )
    expect(selectors(entries)).toEqual(['iteration-2.item', 'iteration-2.index'])
  })

  it('resolves a global iterator selector against the declaration, not a node', () => {
    const entries = deriveWorkflowVariableCatalog(
      [
        node('iteration-1', 'iteration', {
          iterationConfig: {
            iteratorSelector: ['sys', 'batch'],
            collectSelector: [],
            errorStrategy: 'fail',
            maxIterations: 10,
          },
        }),
        { ...node('agent-1', 'agent'), parentId: 'iteration-1' },
      ],
      [],
      'agent-1',
      [{ name: 'sys.batch', valueType: 'array[object]' }],
    )
    expect(entryFor(entries, 'iteration-1.item').valueType).toBe('object')
  })
})

describe('decorateWorkflowVariableCatalog', () => {
  it('marks a declared global and names every node source', () => {
    const nodes = [
      node('start-1', 'start', { inputVariables: [{ name: 'repo', valueType: 'string' }] }),
    ]
    const decorated = decorateWorkflowVariableCatalog(
      deriveWorkflowVariableCatalog(nodes, [], undefined, GLOBALS),
      nodes,
      GLOBALS,
    )
    const global = decorated.find((entry) => entry.selector.join('.') === 'sys.tenant')
    expect(global?.scope).toBe('global')
    const produced = decorated.find((entry) => entry.selector.join('.') === 'start-1.input')
    expect(produced?.scope).toBe('node')
    expect(produced?.sourceNodeTitle).toBe('start-1')
    expect(produced?.sourceNodeKind).toBe('start')
  })

  it('leaves a source it cannot find unnamed rather than inventing a title', () => {
    const decorated = decorateWorkflowVariableCatalog(
      [
        {
          selector: ['ghost-1', 'output'],
          sourceNodeId: 'ghost-1',
          variableName: 'output',
          valueType: 'string',
        },
      ],
      [],
      [],
    )
    expect(decorated[0]?.sourceNodeTitle).toBeUndefined()
    expect(decorated[0]?.sourceNodeKind).toBeUndefined()
  })
})

it('exposes loop named outputs outside and carried plus outer ancestors inside', () => {
  const loop = node('l', 'loop', {
    loopConfig: {
      maxIterations: 2,
      variables: [
        {
          name: 'draft',
          valueType: 'string',
          initial: { kind: 'constant', value: '' },
          feedback: ['w', 'output'],
        },
      ],
      until: { logic: 'and', conditions: [] },
      outputs: [{ name: 'result', variableSelector: ['w', 'output'] }],
    },
  })
  const worker = { ...node('w', 'agent'), parentId: 'l' }
  const nodes = [
    node('s', 'start'),
    loop,
    worker,
    node('exit', 'loopExit', { containerId: 'l' }),
    node('o', 'output'),
  ]
  const edges = [edge('s', 'l'), edge('l', 'o')]
  expect(selectors(deriveWorkflowVariableCatalog(nodes, edges, 'o'))).toContain('l.result')
  expect(selectors(deriveWorkflowVariableCatalog(nodes, edges, 'o'))).not.toContain('l.output')
  expect(selectors(deriveWorkflowVariableCatalog(nodes, edges, 'o'))).not.toContain('l.draft')
  expect(selectors(deriveWorkflowVariableCatalog(nodes, edges, 'w'))).toEqual(
    expect.arrayContaining(['s.input', 'l.draft']),
  )
  expect(selectors(deriveWorkflowVariableCatalog(nodes, edges))).not.toContain('exit.output')
})
