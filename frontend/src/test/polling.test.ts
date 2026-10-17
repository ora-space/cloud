import { describe, expect, it } from 'vitest'
import { settleUntil } from '@/test/polling'

describe('settleUntil', () => {
  it('retries the assertion until it holds', async () => {
    let calls = 0

    await settleUntil(() => {
      calls += 1
      expect(calls).toBeGreaterThanOrEqual(3)
    })

    // The first two attempts failed and were retried, not swallowed.
    expect(calls).toBe(3)
  })

  it('rethrows the last failure once the attempts run out', async () => {
    let calls = 0
    const never = (): void => {
      calls += 1
      expect(calls).toBeGreaterThan(99)
    }

    // The last failure is the one the caller sees, not a timeout of its own.
    await expect(settleUntil(never, 2)).rejects.toThrow('expected 3 to be greater than 99')
    // Three calls: the initial attempt plus the two retries.
    expect(calls).toBe(3)
  })
})
