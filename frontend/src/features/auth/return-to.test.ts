import { describe, expect, it } from 'vitest'
import { loginPath, safeReturnTo } from '@/features/auth/return-to'

describe('safeReturnTo', () => {
  it('keeps a same-origin path including query and fragment', () => {
    expect(safeReturnTo('/ora/issues?tab=mine#today')).toBe('/ora/issues?tab=mine#today')
  })

  it.each([
    null,
    '',
    'https://evil.example',
    '//evil.example',
    '/\\evil',
    '/path\\evil',
    '/bad\npath',
  ])('falls back for an unsafe target %s', (candidate) => expect(safeReturnTo(candidate)).toBe('/'))

  it('encodes the validated target in the login route', () => {
    expect(loginPath('/ora/issues?tab=mine')).toBe('/login?returnTo=%2Fora%2Fissues%3Ftab%3Dmine')
  })
})
