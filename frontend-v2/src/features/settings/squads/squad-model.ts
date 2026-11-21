import type { SquadDetail, SquadMember, SquadSummary } from '@/lib/squads-api/generated'

/** Members still in the squad, in the order the server returns them. */
export const activeMembers = (squad: SquadDetail): SquadMember[] => squad.members.filter(m => m.endedAt === null)

/** Ended memberships, kept by the server as history and shown read-only. */
export const endedMembers = (squad: SquadDetail): SquadMember[] => squad.members.filter(m => m.endedAt !== null)

/**
 * The leader's display name. The summary only carries an id, so a name is only known when the
 * leader is a live member of the squad; otherwise the caller shows that the leader is not present.
 */
export function leaderName(squad: SquadDetail): string | null {
  if (!squad.leaderAgentId) return null
  return activeMembers(squad).find(m => m.spaceAgentId === squad.leaderAgentId)?.displayName ?? null
}

/** Whether a live agent member currently leads the squad. */
export const isLeader = (squad: SquadDetail, member: SquadMember): boolean =>
  member.type === 'agent' && member.spaceAgentId !== null && member.spaceAgentId === squad.leaderAgentId

/** The current user's live human membership, if they are still in the squad. */
export function ownMembership(squad: SquadDetail, userId: string): SquadMember | undefined {
  return activeMembers(squad).find(m => m.type === 'human' && m.userId === userId)
}

/** Workspace roles that may edit any squad; they mirror the server's administrator check. */
export const isWorkspaceAdministrator = (role: string | undefined): boolean => role === 'owner'

/**
 * Whether the signed-in user may edit this squad: workspace administrators may edit any squad,
 * everyone else must be a live human maintainer. The server remains the authority and answers 403
 * otherwise, so the two checks must stay in step.
 */
export const canMaintain = (squad: SquadDetail, userId: string, workspaceRole?: string): boolean =>
  isWorkspaceAdministrator(workspaceRole) || activeMembers(squad).some(m => m.userId === userId && m.maintainer)

/** Live totals across the listed squads; archived squads are not listed. */
export function squadTotals(squads: SquadSummary[]) {
  return squads.reduce(
    (total, squad) => ({
      humans: total.humans + squad.humanCount,
      agents: total.agents + squad.agentCount,
      maintainers: total.maintainers + squad.maintainerCount,
    }),
    { humans: 0, agents: 0, maintainers: 0 },
  )
}

/** Local date and time of a server instant; the demo runs in one timezone. */
export const formatTime = (iso: string): string =>
  new Intl.DateTimeFormat('zh-CN', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(iso))

/** A usable squad name: trimmed already, non-empty, at most 200 characters. */
export function validateSquad(name: string, description: string, charter: string): Record<string, string> {
  const errors: Record<string, string> = {}
  const trimmed = name.trim()
  if (!trimmed) errors.name = '请填写小队名称。'
  else if ([...trimmed].length > 200) errors.name = '名称最多 200 个字符。'
  if ([...description].length > 2000) errors.description = '简介最多 2000 个字符。'
  if ([...charter].length > 8000) errors.charter = '协作说明最多 8000 个字符。'
  return errors
}
