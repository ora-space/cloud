import { act } from '@testing-library/react'

/** How long {@link settleUntil} waits between attempts, in milliseconds. */
const RETRY_MS = 5

/**
 * Waits for an assertion to hold while a query polls on a faked `setInterval`.
 *
 * Faking `setInterval` is how a polling test advances its query, but Testing
 * Library's `waitFor` polls on `setInterval` too, so it cannot run alongside the
 * fake clock. This helper retries the assertion on a real `setTimeout` instead,
 * flushing React between attempts.
 *
 * @param check - Assertion that throws while its condition does not hold yet.
 * @param attempts - Retries left before the last failure is rethrown.
 * @throws The last failure `check` produced, once the attempts run out.
 */
export async function settleUntil(check: () => void, attempts = 50): Promise<void> {
  try {
    check()
    return
  } catch (error) {
    if (attempts <= 0) {
      throw error
    }
  }
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, RETRY_MS))
  })
  await settleUntil(check, attempts - 1)
}
