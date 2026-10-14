import { describe, expect, it } from 'vitest'
import { deletionRefusalMessage } from '@/features/projects/deletion'

describe('deletionRefusalMessage', () => {
  it('names what the member can do for every reason a delete may answer', () => {
    // The codes Cloud can refuse a project deletion with. A code that loses its
    // sentence would fall back to the bare code, which is the regression this
    // case exists to catch: a member who cannot read the refusal can only retry
    // it blindly.
    const codes = [
      'operation_in_progress',
      'runtime_control_held',
      'resource_in_use',
      'space_role_required',
      'version_conflict',
      'not_found',
      'runtime_control_reconciliation_required',
    ]

    for (const code of codes) {
      const message = deletionRefusalMessage(code)
      expect(message).not.toBe(`删除失败：${code}`)
      expect(message.length).toBeGreaterThan(8)
      // The code must not be the sentence: it is rendered beside it.
      expect(message).not.toBe(code)
    }
  })

  it('tells a duplicate deletion apart from a refused one', () => {
    // These two are the pair a member is most likely to confuse: one means the
    // deletion is already happening, the other means it did not start. They
    // must not read alike.
    expect(deletionRefusalMessage('operation_in_progress')).not.toBe(
      deletionRefusalMessage('runtime_control_held'),
    )
  })

  it('falls back to the bare code for a reason this build was not taught', () => {
    expect(deletionRefusalMessage('some_new_refusal')).toBe('删除失败：some_new_refusal')
  })
})
