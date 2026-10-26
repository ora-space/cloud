import { describe, expect, it } from 'vitest'
import {
  publishErrorTranslations,
  workflowAdmissionKeys,
} from '@/features/workflows/publish-error-translations'
describe('publish admission guidance', () => {
  it.each([
    'aggregator_selector_not_upstream',
    'loop_max_iterations_invalid',
    'loop_initial_invalid',
    'iteration_collect_outside_region',
    'output_selector_undeclared',
    'output_bindings_not_a_list',
    'output_selector_unsupported',
  ])('explains %s in both locales', (code) => {
    const key = workflowAdmissionKeys[code]
    expect(key).toBeDefined()
    if (key === undefined) throw new Error('missing admission message')
    expect(publishErrorTranslations['zh-CN'][key]).not.toBe(
      publishErrorTranslations['zh-CN']['workflows.errors.problemCheck'],
    )
    expect(publishErrorTranslations['en-US'][key]).not.toBe(
      publishErrorTranslations['en-US']['workflows.errors.problemCheck'],
    )
  })
})
