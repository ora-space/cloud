import { describe, expect, it } from 'vitest'
import type { SquadDetail, SquadMember, SquadSummary } from '@/lib/squads-api/generated'
import { activeMembers, canMaintain, endedMembers, isLeader, leaderName, ownMembership, squadTotals, validateSquad } from './squad-model'

const member = (overrides: Partial<SquadMember>): SquadMember => ({
  id: 'member', type: 'human', userId: 'user', spaceAgentId: null, displayName: '成员', role: '', maintainer: false,
  joinedAt: '2026-10-01T00:00:00Z', endedAt: null, ...overrides,
})

const squad = (overrides: Partial<SquadDetail> = {}): SquadDetail => ({
  id: 'squad', name: '产品小队', description: '', charter: '', leaderAgentId: null, archivedAt: null, version: 1,
  createdBy: 'user', createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z',
  humanCount: 2, agentCount: 1, maintainerCount: 1, members: [], ...overrides,
})

describe('squad members', () => {
  it('splits live members from the ended history', () => {
    const detail = squad({ members: [member({ id: 'live' }), member({ id: 'left', endedAt: '2026-10-02T00:00:00Z' })] })
    expect(activeMembers(detail).map(m => m.id)).toEqual(['live'])
    expect(endedMembers(detail).map(m => m.id)).toEqual(['left'])
  })

  it('names the leader only while that agent is a live member', () => {
    const agent = member({ id: 'agent', type: 'agent', userId: null, spaceAgentId: 'agent-1', displayName: '研究员' })
    expect(leaderName(squad({ leaderAgentId: 'agent-1', members: [agent] }))).toBe('研究员')
    expect(leaderName(squad({ leaderAgentId: 'agent-1', members: [{ ...agent, endedAt: '2026-10-02T00:00:00Z' }] }))).toBeNull()
    expect(leaderName(squad({ leaderAgentId: 'gone' }))).toBeNull()
    expect(leaderName(squad())).toBeNull()
  })

  it('tells the leader agent apart from other members', () => {
    const agent = member({ id: 'agent', type: 'agent', userId: null, spaceAgentId: 'agent-1', displayName: '研究员' })
    const other = member({ id: 'other', type: 'agent', userId: null, spaceAgentId: 'agent-2' })
    const detail = squad({ leaderAgentId: 'agent-1', members: [agent, other] })
    expect(isLeader(detail, agent)).toBe(true)
    expect(isLeader(detail, other)).toBe(false)
  })

  it('finds the current user live membership for self-exit', () => {
    const detail = squad({ members: [member({ id: 'me', userId: 'me' }), member({ id: 'left', userId: 'left', endedAt: '2026-10-02T00:00:00Z' })] })
    expect(ownMembership(detail, 'me')?.id).toBe('me')
    expect(ownMembership(detail, 'left')).toBeUndefined()
  })

  it('recognizes a live human maintainer or a workspace administrator as editor', () => {
    const detail = squad({ members: [
      member({ id: 'me', userId: 'me', maintainer: true }),
      member({ id: 'left', userId: 'left', maintainer: true, endedAt: '2026-10-02T00:00:00Z' }),
      member({ id: 'agent', type: 'agent', userId: null, spaceAgentId: 'agent-1', maintainer: false }),
    ] })
    expect(canMaintain(detail, 'me')).toBe(true)
    expect(canMaintain(detail, 'left')).toBe(false)
    expect(canMaintain(squad({ members: [{ ...member({ userId: 'me', maintainer: true }), endedAt: '2026-10-02T00:00:00Z' }] }), 'me')).toBe(false)
    // Workspace administrators may edit any squad, matching the server's check.
    expect(canMaintain(detail, 'stranger', 'owner')).toBe(true)
    expect(canMaintain(detail, 'stranger', 'admin')).toBe(false)
    expect(canMaintain(detail, 'stranger', 'member')).toBe(false)
  })
})

describe('squad totals', () => {
  it('adds live counts across the listed squads', () => {
    const summaries: SquadSummary[] = [squad(), squad({ id: 'other', humanCount: 1, agentCount: 2, maintainerCount: 0 })]
    expect(squadTotals(summaries)).toEqual({ humans: 3, agents: 3, maintainers: 1 })
    expect(squadTotals([])).toEqual({ humans: 0, agents: 0, maintainers: 0 })
  })
})

describe('squad validation', () => {
  it('requires a non-blank name and bounds the long fields', () => {
    expect(validateSquad('   ', '', '')).toEqual({ name: '请填写小队名称。' })
    expect(validateSquad('x'.repeat(201), '', '')).toEqual({ name: '名称最多 200 个字符。' })
    expect(validateSquad('产品小队', 'd'.repeat(2001), '')).toEqual({ description: '简介最多 2000 个字符。' })
    expect(validateSquad('产品小队', '', 'c'.repeat(8001))).toEqual({ charter: '协作说明最多 8000 个字符。' })
    expect(validateSquad('产品小队', '', '')).toEqual({})
  })
})
