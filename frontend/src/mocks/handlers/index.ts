import { agentHandlers } from './agents'
import { billingHandlers } from './billing'
import { chatHandlers } from './chat'
import { inboxHandlers } from './inbox'
import { issueHandlers } from './issues'
import { projectHandlers } from './projects'
import { runtimeHandlers } from './runtimes'
import { skillHandlers } from './skills'
import { squadHandlers } from './squads'
import { workspaceHandlers } from './workspaces'

// The plugin endpoints are cloud-backed only: they have no mock-api twin, and
// registering their real /api/v1 paths here would hijack the live backend in
// the browser. Tests install pluginCloudHandlers explicitly per scenario.
export const handlers = [
  ...workspaceHandlers,
  ...issueHandlers,
  ...projectHandlers,
  ...squadHandlers,
  ...agentHandlers,
  ...chatHandlers,
  ...inboxHandlers,
  ...skillHandlers,
  ...runtimeHandlers,
  ...billingHandlers,
]
