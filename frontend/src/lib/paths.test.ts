import { describe, expect, it } from 'vitest'
import {
  joinUrl,
  joinedLinkPath,
  loginPath,
  safeReturnTo,
  workspacePaths,
  workspaceUrlPrefix,
} from './paths'

describe('safeReturnTo', () => {
  it('keeps a same-origin path including query and fragment', () => {
    expect(safeReturnTo('/w/ora/issues?tab=mine#today')).toBe('/w/ora/issues?tab=mine#today')
  })

  it.each([
    null,
    undefined,
    '',
    'https://evil.example',
    '//evil.example',
    '/\\evil',
    '/path\\evil',
    '/bad\npath',
    '/' + 'a'.repeat(2048),
  ])('falls back to / for an unsafe target %s', (candidate) => {
    expect(safeReturnTo(candidate)).toBe('/')
  })
})

describe('loginPath', () => {
  it('encodes the validated target in the login route', () => {
    expect(loginPath('/w/ora/issues?tab=mine')).toBe(
      '/login?returnTo=%2Fw%2Fora%2Fissues%3Ftab%3Dmine',
    )
    expect(loginPath('//evil.example')).toBe('/login?returnTo=%2F')
  })
})

describe('workspacePaths', () => {
  it('keeps every workspace route under the reserved prefix', () => {
    const p = workspacePaths('acme')
    expect(p.root).toBe('/w/acme')
    expect(p.issueDetail('42')).toBe('/w/acme/issues/42')
    expect(p.workflowDetail('42')).toBe('/w/acme/workflows/42')
    expect(p.spaces).toBe('/w/acme/spaces')
    expect(p.skills).toBe('/w/acme/skills')
    expect(p.plugins).toBe('/w/acme/plugins')
    expect(p.members).toBe('/w/acme/settings/members')
    expect(p.repositories).toBe('/w/acme/repositories')
    expect(p.gitIdentity).toBe('/w/acme/settings/git-identity')
    expect(workspaceUrlPrefix()).toBe('localhost:3000/w/')
  })
})

describe('private join links', () => {
  it('round-trips a same-origin invitation and rejects external or malformed links', () => {
    const token = 'a'.repeat(43)
    const invite = joinUrl('invite', token)
    expect(invite).toBe(`http://localhost:3000/join/invite/${token}`)
    expect(joinedLinkPath(invite)).toBe(`/join/invite/${token}`)
    expect(joinedLinkPath(`/join/apply/${token}`)).toBe(`/join/apply/${token}`)
    expect(joinedLinkPath(`https://evil.invalid/join/invite/${token}`)).toBeUndefined()
    expect(joinedLinkPath('/join/invite/short')).toBeUndefined()
    expect(joinedLinkPath('http://[invalid')).toBeUndefined()
  })
})
