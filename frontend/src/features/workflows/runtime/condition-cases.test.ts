import { describe, expect, it } from 'vitest'
import {
  WORKFLOW_CONDITION_OPERATORS,
  isWorkflowConditionComparisonComplete,
  migrateConditionOperator,
  resolveConditionCases,
} from '@/features/workflows/runtime/condition-cases'
import type { WorkflowNodeData } from '@/features/workflows/runtime/types'

/** Builds node data around the one field a case varies. */
function dataWith(parts: Partial<WorkflowNodeData>): WorkflowNodeData {
  return { kind: 'condition', title: 'Condition', description: '', ...parts }
}

describe('condition case resolution', () => {
  it('prefers the canonical cases over every earlier spelling', () => {
    const cases = [{ id: 'case-1', logic: 'and' as const, conditions: [] }]
    const data = dataWith({
      cases,
      conditionCases: [{ id: 'legacy', logic: 'or', conditions: [] }],
      conditionBranches: [{ conditions: [] }],
    })

    expect(resolveConditionCases(data)).toBe(cases)
  })

  it('falls back to the legacy executable cases', () => {
    const legacy = [{ id: 'legacy', logic: 'or' as const, conditions: [] }]

    expect(resolveConditionCases(dataWith({ conditionCases: legacy }))).toBe(legacy)
  })

  it('migrates authored branches into executable cases', () => {
    const cases = resolveConditionCases(
      dataWith({
        conditionBranches: [
          {
            logic: 'or',
            conditions: [{ variable: 'agent-1.output', operator: 'equals', value: 'done' }],
          },
          { conditions: [{ variable: 'start.query', operator: 'is_not_empty', value: '' }] },
        ],
      }),
    )

    expect(cases).toEqual([
      {
        id: 'case-1',
        logic: 'or',
        conditions: [
          { variableSelector: ['agent-1', 'output'], operator: 'equals', value: 'done' },
        ],
      },
      {
        id: 'case-2',
        logic: 'and',
        conditions: [{ variableSelector: ['start', 'query'], operator: 'not_empty' }],
      },
    ])
  })

  it('splits and trims a dotted variable path into selector parts', () => {
    const cases = resolveConditionCases(
      dataWith({
        conditionBranches: [
          {
            conditions: [{ variable: ' agent-1 . output . text ', operator: 'equals', value: 'x' }],
          },
        ],
      }),
    )

    expect(cases.at(0)?.conditions.at(0)?.variableSelector).toEqual(['agent-1', 'output', 'text'])
  })

  it('gives a condition node one empty branch so its IF handle exists before any rule does', () => {
    expect(resolveConditionCases(dataWith({}))).toEqual([
      { id: 'case-1', logic: 'and', conditions: [] },
    ])
    expect(resolveConditionCases(dataWith({ conditionBranches: [] }))).toEqual([
      { id: 'case-1', logic: 'and', conditions: [] },
    ])
  })
})

describe('legacy condition operator migration', () => {
  it('renames the operators an earlier editor wrote', () => {
    expect(migrateConditionOperator('is_empty', undefined)).toBe('empty')
    expect(migrateConditionOperator('is_not_empty', undefined)).toBe('not_empty')
    expect(migrateConditionOperator('equals', undefined)).toBe('equals')
  })

  it('folds a negated rule into its negated operator', () => {
    expect(migrateConditionOperator('equals', true)).toBe('not_equals')
    expect(migrateConditionOperator('contains', true)).toBe('not_contains')
    expect(migrateConditionOperator('is_empty', true)).toBe('not_empty')
  })

  it('leaves an operator without a negated form alone', () => {
    expect(migrateConditionOperator('greater_than', true)).toBe('greater_than')
    expect(migrateConditionOperator('equals', false)).toBe('equals')
  })
})

describe('condition comparison completeness', () => {
  it('accepts a fully-qualified comparison with a value', () => {
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'equals',
        value: 'done',
      }),
    ).toBe(true)
  })

  it('accepts a valueless operator without a value', () => {
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'not_empty',
      }),
    ).toBe(true)
  })

  it('rejects a comparison the engine could not evaluate', () => {
    expect(
      isWorkflowConditionComparisonComplete({ variableSelector: ['output'], operator: 'equals' }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', ' '],
        operator: 'equals',
        value: 'x',
      }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: ' ',
      }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'equals',
      }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'equals',
        value: '  ',
      }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'equals',
        value: null,
      }),
    ).toBe(false)
    expect(
      isWorkflowConditionComparisonComplete({
        variableSelector: ['agent-1', 'output'],
        operator: 'equals',
        value: 0,
      }),
    ).toBe(true)
  })
})

describe('condition operator catalog', () => {
  it('offers the operators the engine evaluates, each once', () => {
    expect(new Set(WORKFLOW_CONDITION_OPERATORS).size).toBe(WORKFLOW_CONDITION_OPERATORS.length)
    expect(WORKFLOW_CONDITION_OPERATORS).toEqual([
      'equals',
      'not_equals',
      'contains',
      'not_contains',
      'starts_with',
      'ends_with',
      'greater_than',
      'greater_than_or_equal',
      'less_than',
      'less_than_or_equal',
      'empty',
      'not_empty',
      'exists',
      'not_exists',
    ])
  })
})
