import type { TranslationKey } from '@/i18n/i18n-instance'

/**
 * Maps the workflow API's fault codes onto the messages the interface shows.
 *
 * The backend answers a rejected mutation with a `code` (never a sentence), so
 * this table is the only place that decides what a member reads. A code with no
 * entry falls back to a generic message rather than rendering the raw code:
 * an unlisted fault is a gap in this table, not something to show a member.
 *
 * `version_conflict` and `version_required` are separate entries because they
 * ask for different actions — reload and retry versus a bug in the caller —
 * even though both end in the same advice today.
 */
const FAULT_MESSAGES: Record<string, TranslationKey> = {
  workflow_conflict: 'workflows.errors.nameConflict',
  not_found: 'workflows.errors.notFound',
  version_conflict: 'workflows.errors.stale',
  version_required: 'workflows.errors.stale',
  invalid_input: 'workflows.errors.nameBlank',
  workflow_no_published_snapshot: 'workflows.run.noPublishedSnapshot',
  required_field_missing: 'workflows.errors.requiredField',
  invalid_field_value: 'workflows.errors.invalidField',
  workflow_executor_unconfigured: 'workflows.errors.executorUnconfigured',
  workflow_graph_invalid: 'workflows.errors.graphInvalid',
}

/**
 * Resolves the translation key for a fault code returned by the workflow API.
 *
 * @param code - Fault code from the response body, if the request produced one.
 * @returns The key to translate; never undefined, so callers render unconditionally.
 */
export function workflowErrorKey(code: string | undefined): TranslationKey {
  if (code === undefined) return 'workflows.errors.unknown'
  return FAULT_MESSAGES[code] ?? 'workflows.errors.unknown'
}
