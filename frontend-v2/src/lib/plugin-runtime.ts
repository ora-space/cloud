import type { SpacePlugin } from '@/api/generated.schemas'

type PreinstalledAgentFields = Pick<
  SpacePlugin,
  'kind' | 'requestedByUserId'
>

type RunnableAgentFields = Pick<
  SpacePlugin,
  'kind' | 'observedState' | 'requestedByUserId'
>

/** System-seeded Agent plugins are managed by the demo runtime, not by the marketplace. */
export function isPreinstalledAgent(plugin: PreinstalledAgentFields): boolean {
  return plugin.kind === 'agent' && plugin.requestedByUserId == null
}

/** Only the preinstalled and observed Agents can be selected for real execution in v2. */
export function isRunnableAgent(plugin: RunnableAgentFields): boolean {
  return isPreinstalledAgent(plugin) && plugin.observedState === 'installed'
}
