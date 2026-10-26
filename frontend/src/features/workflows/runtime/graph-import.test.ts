import { describe, expect, it } from 'vitest'
import {
  WorkflowImportError,
  isWorkflowAgentConfig,
  parseImportedWorkflowDocument,
} from '@/features/workflows/runtime/graph-import'
import { validateWorkflowDefinition } from '@/features/workflows/runtime/definition'
import type { WorkflowAgentConfig } from '@/features/workflows/runtime/types'

const AGENT_CONFIG: WorkflowAgentConfig = {
  schemaVersion: 3,
  executor: { agentCli: 'official/ora-space.codeagentcli', modelId: 'gpt-5' },
  roleId: 'Architect',
  skills: [],
  mcps: [],
  prompt: '',
}

const START_NODE = {
  id: 'start',
  type: 'workflow',
  position: { x: 0, y: 0 },
  deletable: false,
  data: { kind: 'start', title: '开始', description: '定义工作流输入' },
}

const AGENT_NODE = {
  id: 'agent-1',
  type: 'workflow',
  position: { x: 240, y: 0 },
  data: { kind: 'agent', title: 'Agent', description: '', agentConfig: AGENT_CONFIG },
}

const EDGE = { id: 'e1', type: 'workflow', source: 'start', target: 'agent-1' }

const ANNOTATION = {
  id: 'note-1',
  type: 'annotation',
  position: { x: 12, y: 34 },
  data: { text: 'Review this branch', theme: 'yellow' },
}

/** Builds an exported document around the parts one test varies. */
function documentWith(parts: Record<string, unknown>): Record<string, unknown> {
  return {
    id: 'workflow-1',
    name: 'Review flow',
    description: 'Reviews a change',
    updatedAt: '2026-09-20T12:00:00+08:00',
    viewport: { x: 0, y: 0, zoom: 1 },
    nodes: [START_NODE, AGENT_NODE],
    edges: [EDGE],
    ...parts,
  }
}

/** Runs an import that is expected to be refused, returning the issues it reported. */
function importIssues(value: unknown): readonly string[] {
  try {
    parseImportedWorkflowDocument(value)
  } catch (error) {
    if (error instanceof WorkflowImportError) {
      return error.issues
    }
    throw error
  }
  throw new Error('expected the import to be refused')
}

describe('imported workflow documents', () => {
  it('turns a valid document into a definition and its editor notes', () => {
    const imported = parseImportedWorkflowDocument(documentWith({ annotations: [ANNOTATION] }))

    expect(imported.definition.id).toBe('workflow-1')
    expect(imported.definition.name).toBe('Review flow')
    expect(imported.definition.nodes.map((node) => node.id)).toEqual(['start', 'agent-1'])
    expect(imported.definition.nodes.every((node) => node.type === 'workflow')).toBe(true)
    expect(imported.definition.edges).toEqual([
      { id: 'e1', source: 'start', target: 'agent-1', type: 'workflow' },
    ])
    expect(imported.annotations).toEqual([ANNOTATION])
  })

  it('recovers the launch-field declaration an export carried', () => {
    const launchFields = [
      { key: 'snapshotId', enabled: false },
      { key: 'prompt', enabled: true, required: true },
    ]
    const imported = parseImportedWorkflowDocument(documentWith({ launchFields }))

    // It rides beside the definition rather than inside it: the executable document is shared with
    // the desktop runtime, which has no `@` form to declare fields for.
    expect(imported.launchFields).toEqual(launchFields)
    expect(imported.definition).not.toHaveProperty('launchFields')
  })

  it('refuses a declaration entry that does not name a platform launch field', () => {
    expect(
      importIssues(documentWith({ launchFields: [{ key: 'deploy_target' }, { key: 'branch' }] })),
    ).toEqual(['launch field 0 does not name a platform launch field'])
    expect(importIssues(documentWith({ launchFields: {} }))).toEqual([
      'launchFields must be an array',
    ])
  })

  it('defaults the metadata an export may leave out', () => {
    const imported = parseImportedWorkflowDocument({
      id: 'workflow-1',
      name: 'Review flow',
      nodes: [START_NODE],
      edges: [],
    })

    expect(imported.definition.description).toBe('')
    expect(imported.definition.viewport).toEqual({ x: 0, y: 0, zoom: 1 })
    expect(Number.isNaN(Date.parse(imported.definition.updatedAt))).toBe(false)
  })

  it('refuses anything that is not an object', () => {
    expect(importIssues('not a workflow')).toEqual(['document must be an object'])
    expect(importIssues([])).toEqual(['document must be an object'])
  })

  it('requires the identity a workflow cannot be created without', () => {
    expect(importIssues(documentWith({ id: '  ' }))).toEqual(['document must carry an id'])
    expect(importIssues(documentWith({ name: undefined }))).toEqual(['document must carry a name'])
  })

  it('reports every problem at once rather than stopping at the first', () => {
    expect(importIssues(documentWith({ id: '', name: '', nodes: 'nope' }))).toEqual([
      'document must carry an id',
      'document must carry a name',
      'nodes must be an array',
    ])
  })

  it('refuses a node that is not a workflow node the editor could render', () => {
    expect(importIssues(documentWith({ nodes: [{ ...START_NODE, type: 'default' }] }))).toEqual([
      'node 0 must be a workflow node',
    ])
    expect(importIssues(documentWith({ nodes: [{ id: 'start' }] }))).toEqual([
      'node 0 must be a workflow node',
    ])
  })

  it('refuses a node without identity or geometry', () => {
    expect(importIssues(documentWith({ nodes: [{ ...START_NODE, id: ' ' }] }))).toEqual([
      'node 0 has an empty id',
    ])
    expect(
      importIssues(documentWith({ nodes: [{ ...START_NODE, position: { x: Number.NaN, y: 0 } }] })),
    ).toEqual(['node 0 has a non-finite position'])
  })

  it('refuses a node kind the execution contract does not understand', () => {
    expect(
      importIssues(
        documentWith({
          nodes: [{ ...START_NODE, data: { kind: 'prompt', title: 'x', description: '' } }],
        }),
      ),
    ).toEqual(['node 0 is missing a supported kind, a title, or an agent config'])
  })

  it('refuses a start node that could be deleted, since a run has to begin somewhere', () => {
    expect(importIssues(documentWith({ nodes: [{ ...START_NODE, deletable: true }] }))).toEqual([
      'node 0 must be a non-deletable start node',
    ])
  })

  it('refuses a node without the text the canvas draws on it', () => {
    expect(
      importIssues(
        documentWith({ nodes: [{ ...START_NODE, data: { kind: 'start', title: '开始' } }] }),
      ),
    ).toEqual(['node 0 is missing a supported kind, a title, or an agent config'])
  })

  it('refuses an agent node whose execution contract is incomplete', () => {
    const incomplete = {
      ...AGENT_NODE,
      data: { ...AGENT_NODE.data, agentConfig: { schemaVersion: 3 } },
    }

    expect(importIssues(documentWith({ nodes: [START_NODE, incomplete] }))).toEqual([
      'node 1 is missing a supported kind, a title, or an agent config',
    ])
  })

  it('refuses edges that do not join two named nodes', () => {
    expect(importIssues(documentWith({ edges: [{ ...EDGE, type: 'default' }] }))).toEqual([
      'edge 0 must be a workflow edge',
    ])
    expect(importIssues(documentWith({ edges: [{ ...EDGE, target: '' }] }))).toEqual([
      'edge 0 must name an id, a source, and a target',
    ])
    expect(importIssues(documentWith({ edges: [{ ...EDGE, sourceHandle: 7 }] }))).toEqual([
      'edge 0 has a handle that is neither absent nor a string',
    ])
    expect(importIssues(documentWith({ edges: 'nope' }))).toEqual(['edges must be an array'])
  })

  it('accepts a handle React Flow reports as null', () => {
    const imported = parseImportedWorkflowDocument(
      documentWith({ edges: [{ ...EDGE, sourceHandle: null, targetHandle: null }] }),
    )

    expect(imported.definition.edges.at(0)?.sourceHandle).toBeUndefined()
  })

  it('refuses a viewport the canvas could not restore', () => {
    expect(importIssues(documentWith({ viewport: { x: 0, y: 0, zoom: 0 } }))).toEqual([
      'viewport must contain finite coordinates and a positive zoom',
    ])
    expect(importIssues(documentWith({ viewport: 'origin' }))).toEqual([
      'viewport must contain finite coordinates and a positive zoom',
    ])
  })

  it('refuses an editor note it could not draw', () => {
    expect(
      importIssues(
        documentWith({ annotations: [{ ...ANNOTATION, data: { text: 'x', theme: 'neon' } }] }),
      ),
    ).toEqual(['annotation 0 is not a valid editor note'])
    expect(importIssues(documentWith({ annotations: {} }))).toEqual([
      'annotations must be an array',
    ])
  })

  it('carries workflow variables through an export/import round-trip', () => {
    const variable = { name: 'site.url', valueType: 'string', value: 'https://example.com' }
    const imported = parseImportedWorkflowDocument(documentWith({ globalVariables: [variable] }))

    expect(imported.definition.globalVariables).toEqual([variable])
  })

  it('reports a workflow variable it could not read', () => {
    expect(
      importIssues(documentWith({ globalVariables: [{ name: 'no-dot', valueType: 'string' }] })),
    ).toEqual(['global variable 0 is not a valid workflow variable'])
    expect(importIssues(documentWith({ globalVariables: {} }))).toEqual([
      'globalVariables must be an array',
    ])
  })

  it('applies the same identity rules an editor document has to satisfy', () => {
    expect(
      importIssues(documentWith({ nodes: [START_NODE, { ...AGENT_NODE, id: 'start' }] })),
    ).toEqual(['duplicate node id start', 'edge e1 references an unknown node'])
    expect(importIssues(documentWith({ edges: [{ ...EDGE, target: 'ghost' }] }))).toEqual([
      'edge e1 references an unknown node',
    ])
  })

  it('imports a graph the editor may still be wiring, and leaves acyclicity to publish', () => {
    const imported = parseImportedWorkflowDocument(
      documentWith({
        nodes: [START_NODE, AGENT_NODE],
        edges: [EDGE, { id: 'e2', type: 'workflow', source: 'agent-1', target: 'start' }],
      }),
    )

    expect(imported.definition.edges).toHaveLength(2)
    expect(() => validateWorkflowDefinition(imported.definition)).toThrow('graph must be acyclic')
  })
})

describe('imported agent configuration', () => {
  it('accepts a complete execution contract', () => {
    expect(isWorkflowAgentConfig(AGENT_CONFIG)).toBe(true)
    expect(
      isWorkflowAgentConfig({
        ...AGENT_CONFIG,
        skills: [{ skillId: 'code-defect-scan', enabled: true }],
        mcps: [{ mcpId: 'github', enabled: false }],
      }),
    ).toBe(true)
  })

  it('refuses a contract that is missing a part the engine needs', () => {
    expect(isWorkflowAgentConfig(undefined)).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, schemaVersion: 2 })).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, executor: undefined })).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, roleId: ' ' })).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, prompt: undefined })).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, skills: undefined })).toBe(false)
    expect(
      isWorkflowAgentConfig({
        ...AGENT_CONFIG,
        executor: { agentCli: '', modelId: 'gpt-5' },
      }),
    ).toBe(false)
  })

  it('refuses a binding that is unnamed, untyped, or repeated', () => {
    expect(
      isWorkflowAgentConfig({ ...AGENT_CONFIG, skills: [{ skillId: '', enabled: true }] }),
    ).toBe(false)
    expect(
      isWorkflowAgentConfig({ ...AGENT_CONFIG, skills: [{ skillId: 'a', enabled: 'yes' }] }),
    ).toBe(false)
    expect(isWorkflowAgentConfig({ ...AGENT_CONFIG, skills: ['a'] })).toBe(false)
    expect(
      isWorkflowAgentConfig({
        ...AGENT_CONFIG,
        mcps: [
          { mcpId: 'github', enabled: true },
          { mcpId: 'github', enabled: false },
        ],
      }),
    ).toBe(false)
  })
})

it('preserves imported region ownership and authored dimensions', () => {
  const imported = parseImportedWorkflowDocument(
    documentWith({
      nodes: [
        START_NODE,
        { ...AGENT_NODE, parentId: 'region', initialWidth: 300, initialHeight: 200 },
      ],
    }),
  )
  expect(imported.definition.nodes[1]).toMatchObject({
    parentId: 'region',
    initialWidth: 300,
    initialHeight: 200,
  })
})
