import type {
  WorkflowConditionBranch,
  WorkflowConditionCase,
  WorkflowConditionComparison,
  WorkflowConditionRule,
  WorkflowNodeData,
} from '@/features/workflows/runtime/types'

/** Comparison operators a Condition node offers, in menu order. */
export const WORKFLOW_CONDITION_OPERATORS: readonly string[] = [
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
]

/** Operators whose meaning does not require a literal comparison value. */
const VALUELESS_CONDITION_OPERATORS: ReadonlySet<string> = new Set([
  'empty',
  'not_empty',
  'exists',
  'not_exists',
])

/** Spellings an earlier editor wrote, mapped to the operator the engine now reads. */
const LEGACY_CONDITION_OPERATORS: Record<string, string> = {
  is_empty: 'empty',
  is_not_empty: 'not_empty',
}

/** The operator a negated comparison switches to, when one exists. */
const NEGATED_CONDITION_OPERATORS: Record<string, string> = {
  equals: 'not_equals',
  contains: 'not_contains',
  empty: 'not_empty',
}

/** Whether a condition row has enough authored data to be presented as configured. */
export function isWorkflowConditionComparisonComplete(
  comparison: WorkflowConditionComparison,
): boolean {
  if (
    comparison.variableSelector.length < 2 ||
    comparison.variableSelector.some((part) => part.trim() === '') ||
    comparison.operator.trim() === ''
  ) {
    return false
  }
  if (VALUELESS_CONDITION_OPERATORS.has(comparison.operator)) {
    return true
  }
  return (
    comparison.value !== undefined &&
    comparison.value !== null &&
    (typeof comparison.value !== 'string' || comparison.value.trim() !== '')
  )
}

/**
 * Returns the executable cases of a Condition node, migrating any earlier authoring shape.
 *
 * Saved graphs have carried three spellings over time — `cases`, then `conditionCases`, then the
 * `conditionBranches` rule list. Reading all three keeps an old graph editable; a node with none
 * of them still gets one empty branch, so its IF output handle exists before any rule is added.
 */
export function resolveConditionCases(data: WorkflowNodeData): WorkflowConditionCase[] {
  if (data.cases !== undefined) {
    return data.cases
  }
  if (data.conditionCases !== undefined) {
    return data.conditionCases
  }
  const branches = data.conditionBranches
  if (branches !== undefined && branches.length > 0) {
    return branches.map(conditionCaseFromBranch)
  }
  return [{ id: 'case-1', logic: 'and', conditions: [] }]
}

/** Converts one legacy branch into the executable case the engine reads. */
function conditionCaseFromBranch(
  branch: WorkflowConditionBranch,
  index: number,
): WorkflowConditionCase {
  return {
    id: `case-${index + 1}`,
    logic: branch.logic ?? 'and',
    conditions: branch.conditions.map(conditionComparisonFromRule),
  }
}

/** Converts one legacy rule into a fully-qualified comparison. */
function conditionComparisonFromRule(rule: WorkflowConditionRule): WorkflowConditionComparison {
  const comparison: WorkflowConditionComparison = {
    variableSelector: splitVariableSelector(rule.variable),
    operator: migrateConditionOperator(rule.operator, rule.negated),
  }
  if (rule.value !== '') {
    comparison.value = rule.value
  }
  return comparison
}

/** Splits a dotted variable path into the selector parts the engine matches on. */
function splitVariableSelector(variable: string): string[] {
  return variable
    .split('.')
    .map((part) => part.trim())
    .filter((part) => part !== '')
}

/** Normalizes legacy operators before the graph is saved in the canonical `cases` field. */
export function migrateConditionOperator(operator: string, negated: boolean | undefined): string {
  const normalized = LEGACY_CONDITION_OPERATORS[operator] ?? operator
  if (negated !== true) {
    return normalized
  }
  return NEGATED_CONDITION_OPERATORS[normalized] ?? normalized
}
