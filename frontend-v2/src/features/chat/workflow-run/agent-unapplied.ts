import { normalizeWorkflowAgentConfig } from '@/lib/workflows/mock'
import type { WorkflowAgentConfig, WorkflowAgentUnappliedCapability } from '@/lib/workflows/runtime'
import { resolveAgentRetryDisplay } from './agent-config-display'

/** How the run inspector names each Agent setting v2 does not apply, in display order. */
export const unappliedSettingNames: Readonly<Record<WorkflowAgentUnappliedCapability, string>> = {
  role: '角色',
  skill: 'Skill',
  mcp: 'MCP',
  retry: '自动重试',
  interactive: '交互模式',
}
const settingOrder = Object.keys(unappliedSettingNames) as WorkflowAgentUnappliedCapability[]

/**
 * The Agent settings v2 keeps in the graph but runs without: those the attempt recorded, plus those
 * the node's configuration turns on, so a node that has not run yet, or failed, says so as well. A
 * retry counts whenever desktop's policy would rerun the node, its implicit default included, since
 * v2 never retries.
 */
export function unappliedAgentSettings(
  config: WorkflowAgentConfig,
  recorded: readonly WorkflowAgentUnappliedCapability[] = [],
): WorkflowAgentUnappliedCapability[] {
  const normalized = normalizeWorkflowAgentConfig(config)
  const found = new Set(recorded)
  if (normalized.roleId.trim() !== '') found.add('role')
  if (normalized.skills.some((skill) => skill.enabled)) found.add('skill')
  if (normalized.mcps.some((mcp) => mcp.enabled)) found.add('mcp')
  if (normalized.interactive === true) found.add('interactive')
  if (resolveAgentRetryDisplay(normalized).kind === 'enabled') found.add('retry')
  return settingOrder.filter((setting) => found.has(setting))
}
