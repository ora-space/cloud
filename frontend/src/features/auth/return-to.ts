const MAX_RETURN_TO_LENGTH = 2048

function containsControlCharacter(value: string): boolean {
  for (const character of value) {
    if (character.charCodeAt(0) < 0x20 || character === '\u007f') return true
  }
  return false
}

/** Returns a safe same-origin path, falling back to `/` for every ambiguous URL form. */
export function safeReturnTo(candidate: string | null): string {
  if (
    candidate === null ||
    candidate.length === 0 ||
    candidate.length > MAX_RETURN_TO_LENGTH ||
    !candidate.startsWith('/') ||
    candidate.startsWith('//') ||
    candidate.startsWith('/\\') ||
    candidate.includes('\\') ||
    containsControlCharacter(candidate)
  ) {
    return '/'
  }
  return candidate
}

/** Builds the login route while preserving only a validated in-app destination. */
export function loginPath(returnTo: string): string {
  const query = new URLSearchParams({ returnTo: safeReturnTo(returnTo) })
  return `/login?${query.toString()}`
}
