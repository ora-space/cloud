import { describe, expect, it } from 'vitest'
import {
  WORKFLOW_NODE_PALETTE,
  supportsWorkflowNodeScope,
  workflowNodeTypeDefinition,
  workflowPaletteNodeTypes,
} from '@/features/workflows/runtime/node-catalog'
import { WORKFLOW_NODE_KINDS } from '@/features/workflows/runtime/types'

describe('workflow node catalog', () => {
  it('describes every kind the execution contract understands', () => {
    for (const kind of WORKFLOW_NODE_KINDS) {
      const definition = workflowNodeTypeDefinition(kind)

      expect(definition.kind).toBe(kind)
      expect(definition.labelKey).toBe(`workflows.node.${kind}.label`)
      expect(definition.descriptionKey).toBe(`workflows.node.${kind}.description`)
      expect(definition.supportedScopes.length).toBeGreaterThan(0)
    }
  })

  it('offers each palette entry once, and only kinds the catalog describes', () => {
    expect(new Set(WORKFLOW_NODE_PALETTE).size).toBe(WORKFLOW_NODE_PALETTE.length)
    expect(WORKFLOW_NODE_PALETTE.every((kind) => WORKFLOW_NODE_KINDS.includes(kind))).toBe(true)
  })

  it('restricts the start node to the workflow scope, since a region cannot restart a run', () => {
    expect(supportsWorkflowNodeScope('start', 'workflow')).toBe(true)
    expect(supportsWorkflowNodeScope('start', 'iteration')).toBe(false)
  })

  it('offers a loop body the nodes a round can run plus its early exit, but never an Output', () => {
    // The publish gate refuses an Output inside a loop body (loop_body_output_unsupported).
    expect(workflowPaletteNodeTypes('loop').map((entry) => entry.kind)).toEqual([
      'agent',
      'condition',
      'aggregator',
      'loopExit',
    ])
  })

  it('allows only the nodes a region can run in the iteration scope', () => {
    expect(workflowPaletteNodeTypes('iteration').map((entry) => entry.kind)).toEqual([
      'agent',
      'condition',
      'aggregator',
    ])
  })

  it('lists the workflow palette in menu order', () => {
    // The palette also carries the loop-only marker, which the workflow scope filters out.
    expect(workflowPaletteNodeTypes('workflow').map((entry) => entry.kind)).toEqual([
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
