import { describe, expect, it } from 'vitest'
import type { MemberListItem, SpacePlugin } from '@/api/generated.schemas'
import type { SquadDetail, SquadMember } from './api/generated'
import { agentCandidatesFor, candidatesFor, matchingMembers, matchingPlugins, pluginLabel } from './member-model'

const account = (overrides: Partial<MemberListItem>): MemberListItem => ({
  id: 'user', tenantId: 'tenant', userId: 'user', displayName: '成员', role: 'member', status: 'active', version: 1,
  ...overrides,
})

const member = (overrides: Partial<SquadMember>): SquadMember => ({
  id: 'member', type: 'human', userId: 'user', spaceAgentId: null, displayName: '成员', role: '', maintainer: false,
  joinedAt: '2026-10-01T00:00:00Z', endedAt: null, ...overrides,
})

const squad = (members: SquadMember[]): SquadDetail => ({
  id: 'squad', name: '产品小队', description: '', charter: '', leaderAgentId: null, archivedAt: null, version: 1,
  createdBy: 'user', createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z',
  humanCount: 1, agentCount: 0, maintainerCount: 1, members,
})

const plugin = (overrides: Partial<SpacePlugin>): SpacePlugin => ({
  id: 'plugin', spaceId: 'space', tenantId: 'tenant', sourceNamespace: 'official', identifier: 'ora-space.opencode',
  kind: 'agent', desiredState: 'installed', desiredVersion: '0.6.11', observedState: 'installed', observedVersion: '0.6.11',
  installError: null, version: 1, createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z', ...overrides,
})

describe('member candidates', () => {
  it('offers active accounts that are not already in the squad', () => {
    const detail = squad([
      member({ id: 'live', userId: 'live' }),
      member({ id: 'left', userId: 'left', endedAt: '2026-10-02T00:00:00Z' }),
      member({ id: 'agent', type: 'agent', userId: null, spaceAgentId: 'agent-1' }),
    ])
    const roster = [account({ userId: 'live', displayName: '在队' }), account({ userId: 'left', displayName: '已退出' }), account({ userId: 'new', displayName: '新人' })]
    expect(candidatesFor(detail, roster).map(m => m.userId)).toEqual(['left', 'new'])
  })

  it('never offers a suspended account', () => {
    expect(candidatesFor(squad([]), [account({ userId: 'off', status: 'suspended' })])).toEqual([])
  })
})

describe('member search', () => {
  it('matches display names case-insensitively and keeps the list when empty', () => {
    const roster = [account({ userId: 'a', displayName: 'Alice' }), account({ userId: 'b', displayName: '林晓' })]
    expect(matchingMembers(roster, 'ali').map(m => m.userId)).toEqual(['a'])
    expect(matchingMembers(roster, ' 晓 ').map(m => m.userId)).toEqual(['b'])
    expect(matchingMembers(roster, '')).toHaveLength(2)
  })
})

describe('agent candidates', () => {
  it('offers only runnable preinstalled agents without a live membership', () => {
    const detail = squad([
      member({ id: 'live', type: 'agent', userId: null, spaceAgentId: 'joined' }),
      member({ id: 'left', type: 'agent', userId: null, spaceAgentId: 'rejoinable', endedAt: '2026-10-02T00:00:00Z' }),
    ])
    const plugins = [
      plugin({ id: 'joined' }),
      plugin({ id: 'rejoinable' }),
      plugin({ id: 'fresh' }),
      plugin({ id: 'failed', observedState: 'failed' }),
      plugin({ id: 'pending', observedState: 'pending' }),
      plugin({ id: 'skill', kind: 'skill' }),
      plugin({ id: 'market', requestedByUserId: 'owner' }),
    ]
    expect(agentCandidatesFor(detail, plugins).map(p => p.id)).toEqual(['rejoinable', 'fresh'])
  })

  it('labels an agent by the identifier people install and searches it', () => {
    expect(pluginLabel(plugin({ id: 'a', sourceNamespace: 'official', identifier: 'ora-space.opencode' }))).toBe('official/ora-space.opencode')
    const plugins = [plugin({ id: 'a', identifier: 'ora-space.opencode' }), plugin({ id: 'b', sourceNamespace: 'acme', identifier: 'reviewer' })]
    expect(matchingPlugins(plugins, 'opencode').map(p => p.id)).toEqual(['a'])
    expect(matchingPlugins(plugins, ' ACME/ ').map(p => p.id)).toEqual(['b'])
    expect(matchingPlugins(plugins, '')).toHaveLength(2)
  })
})
