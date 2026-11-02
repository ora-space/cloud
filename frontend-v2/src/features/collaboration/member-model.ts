import type { MemberListItem, SpacePlugin } from '@/api/generated.schemas'
import type { SquadDetail } from './api/generated'
import { activeMembers } from './squad-model'
import { isRunnableAgent } from '@/lib/plugin-runtime'

/**
 * Workspace members who may still join a squad: active accounts with no live membership in it.
 * A member who left keeps their history but can be added again, which is why ended rows are not
 * considered. Agents are not part of the workspace roster and never appear here.
 */
export function candidatesFor(squad: SquadDetail, roster: readonly MemberListItem[]): MemberListItem[] {
  const joined = new Set(activeMembers(squad).flatMap(member => (member.userId === null ? [] : [member.userId])))
  return roster.filter(member => member.status === 'active' && !joined.has(member.userId))
}

/**
 * Runnable agent plugins a squad may still add: only the preinstalled runtime Agents qualify,
 * and each Space Agent holds at most one live membership.
 */
export function agentCandidatesFor(squad: SquadDetail, plugins: readonly SpacePlugin[]): SpacePlugin[] {
  const joined = new Set(activeMembers(squad).flatMap(member => (member.spaceAgentId === null ? [] : [member.spaceAgentId])))
  return plugins.filter(plugin => isRunnableAgent(plugin) && !joined.has(plugin.id))
}

/** The install identifier people recognize an agent plugin by, such as official/ora-space.opencode. */
export const pluginLabel = (plugin: SpacePlugin): string => `${plugin.sourceNamespace}/${plugin.identifier}`

/** Filters agent plugins by identifier; an empty query leaves the list unchanged. */
export function matchingPlugins(plugins: readonly SpacePlugin[], query: string): SpacePlugin[] {
  const needle = query.trim().toLowerCase()
  return needle ? plugins.filter(plugin => pluginLabel(plugin).toLowerCase().includes(needle)) : [...plugins]
}

/** Filters candidates by display name; an empty query leaves the list unchanged. */
export function matchingMembers(roster: readonly MemberListItem[], query: string): MemberListItem[] {
  const needle = query.trim().toLowerCase()
  return needle ? roster.filter(member => member.displayName.toLowerCase().includes(needle)) : [...roster]
}
