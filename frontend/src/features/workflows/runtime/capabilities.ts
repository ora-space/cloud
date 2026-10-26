import { DEMO_AGENT_REF } from '@/features/workflows/runtime/agent-identity'
import type { WorkflowAgentConfig } from '@/features/workflows/runtime/types'

/** One selectable value offered by a workflow inspector control. */
export interface WorkflowChoice {
  value: string
  /** Menu text, for a choice whose name is a proper noun or an identifier. */
  label?: string
  /** Translation key of the menu text, for a choice that has to be translated. */
  labelKey?: string
}

/**
 * Resolves the menu text of one capability choice for the active locale.
 *
 * A choice with neither spelling falls back to its value, so a catalog entry can never render as
 * an empty row.
 */
export function workflowChoiceLabel(
  choice: WorkflowChoice,
  translate: (key: string) => string,
): string {
  if (choice.labelKey !== undefined) {
    return translate(choice.labelKey)
  }
  return choice.label ?? choice.value
}

/**
 * Availability is descriptive: an author can save a binding before the plugin behind it is
 * configured, and the reason is shown beside the choice rather than blocking the selection.
 */
export interface WorkflowMcpChoice extends WorkflowChoice {
  unavailableReason?: 'configurationIncomplete' | 'configurationUnavailable' | 'invalidDeclaration'
}

/** One agent CLI and model pair an Agent node may execute with. */
export interface WorkflowAgentModel {
  agentCli: string
  modelId: string
  label: string
}

/** Everything the workflow inspectors offer as a selectable value. */
export interface WorkflowCapabilities {
  /** Agent CLI and model pairs, newest first; the first entry seeds a new Agent node. */
  agentModels: WorkflowAgentModel[]
  roles: WorkflowChoice[]
  skills: WorkflowChoice[]
  /** MCP catalog choices for Agent node attachments (optional per node). */
  mcps: WorkflowMcpChoice[]
  tools: WorkflowChoice[]
  /** Comparison operators offered by Condition nodes. */
  conditionOperators: WorkflowChoice[]
  /** Operations offered per tool value, mirroring Dify's tool-derived actions. */
  toolOperations: Record<string, WorkflowChoice[]>
  /** Execution contract a new Agent node starts from. */
  defaultAgentConfig: WorkflowAgentConfig
  /** Tool a new Tool node starts from. */
  defaultTool: string
}

/** Agent CLI and model an Agent node starts with when nothing else is known. */
const DEFAULT_AGENT_MODEL: WorkflowAgentModel = {
  agentCli: DEMO_AGENT_REF.codeagentcli,
  modelId: 'gpt-5',
  label: 'CodeAgentCLI · GPT-5',
}

/** Role a new Agent node starts with. */
const DEFAULT_AGENT_ROLE = 'Architect'

/** Tool a new Tool node starts with. */
const DEFAULT_WORKFLOW_TOOL = 'Terminal'

/** Model catalog used until the workspace reports the models its agents actually expose. */
export const DEMO_AGENT_MODELS: WorkflowAgentModel[] = [
  DEFAULT_AGENT_MODEL,
  { agentCli: DEMO_AGENT_REF.opencode, modelId: 'opencode/sonnet', label: 'OpenCode · Sonnet' },
  {
    agentCli: DEMO_AGENT_REF.opencode,
    modelId: 'deepseek/deepseek-v4-flash',
    label: 'OpenCode · deepseek/deepseek-v4-flash',
  },
  {
    agentCli: DEMO_AGENT_REF.opencode,
    modelId: 'deepseek/deepseek-v4-pro',
    label: 'OpenCode · deepseek/deepseek-v4-pro',
  },
  { agentCli: DEMO_AGENT_REF.nga, modelId: 'nga/default', label: 'NGA · Default' },
]

/** Role catalog, matching the roles the workflow engine recognises. */
export const DEMO_AGENT_ROLES: WorkflowChoice[] = [
  { value: DEFAULT_AGENT_ROLE, labelKey: 'workflows.role.architect' },
  { value: 'Planner', labelKey: 'workflows.role.planner' },
  { value: 'Researcher', labelKey: 'workflows.role.researcher' },
  { value: 'Implementer', labelKey: 'workflows.role.implementer' },
  { value: 'Reviewer', labelKey: 'workflows.role.reviewer' },
  { value: 'Tester', labelKey: 'workflows.role.tester' },
  { value: 'Debugger', labelKey: 'workflows.role.debugger' },
  { value: 'Documentation Agent', labelKey: 'workflows.role.documentation-agent' },
]

/** Skill catalog; a skill is named by its published id, so no translation is involved. */
export const DEMO_AGENT_SKILLS: WorkflowChoice[] = ['cdase:sfmea_review', 'code-defect-scan'].map(
  (value) => ({ value, label: value }),
)

/** MCP catalog; each entry is named by its server, so no translation is involved. */
export const DEMO_AGENT_MCPS: WorkflowMcpChoice[] = [
  { value: 'filesystem', label: 'Filesystem' },
  { value: 'github', label: 'GitHub' },
  { value: 'browser', label: 'Browser' },
  { value: 'postgres', label: 'Postgres' },
  { value: 'notion', label: 'Notion' },
]

/** Tool catalog offered by Tool nodes. */
export const DEMO_WORKFLOW_TOOLS: WorkflowChoice[] = [
  { value: DEFAULT_WORKFLOW_TOOL, label: 'Terminal' },
  { value: 'File system', label: 'File system' },
  { value: 'GitHub', label: 'GitHub' },
]

/** Operations offered per tool value. */
export const DEMO_TOOL_OPERATIONS: Record<string, WorkflowChoice[]> = {
  Terminal: [{ value: 'run_command', labelKey: 'workflows.operation.run-command' }],
  'File system': [
    { value: 'read_file', labelKey: 'workflows.operation.read-file' },
    { value: 'write_file', labelKey: 'workflows.operation.write-file' },
  ],
  GitHub: [
    { value: 'create_pr', labelKey: 'workflows.operation.create-pr' },
    { value: 'merge_pr', labelKey: 'workflows.operation.merge-pr' },
  ],
}

/** Comparison operators offered by Condition nodes. */
export const DEMO_CONDITION_OPERATORS: WorkflowChoice[] = [
  { value: 'equals', labelKey: 'workflows.operator.equals' },
  { value: 'not_equals', labelKey: 'workflows.operator.not-equals' },
  { value: 'contains', labelKey: 'workflows.operator.contains' },
  { value: 'not_contains', labelKey: 'workflows.operator.not-contains' },
  { value: 'starts_with', labelKey: 'workflows.operator.starts-with' },
  { value: 'ends_with', labelKey: 'workflows.operator.ends-with' },
  { value: 'greater_than', labelKey: 'workflows.operator.greater-than' },
  { value: 'greater_than_or_equal', labelKey: 'workflows.operator.greater-than-or-equal' },
  { value: 'less_than', labelKey: 'workflows.operator.less-than' },
  { value: 'less_than_or_equal', labelKey: 'workflows.operator.less-than-or-equal' },
  { value: 'empty', labelKey: 'workflows.operator.empty' },
  { value: 'not_empty', labelKey: 'workflows.operator.not-empty' },
  { value: 'exists', labelKey: 'workflows.operator.exists' },
  { value: 'not_exists', labelKey: 'workflows.operator.not-exists' },
]

/**
 * Builds the capability catalog a workflow editor runs on.
 *
 * Every catalog here is a default that the workspace may later replace with what its own agents,
 * plugins, and tools actually expose; the shape is what the inspectors depend on, not the list.
 */
export function createDefaultWorkflowCapabilities(
  agentModels: WorkflowAgentModel[] = DEMO_AGENT_MODELS,
): WorkflowCapabilities {
  const defaultAgentModel = agentModels[0] ?? DEFAULT_AGENT_MODEL
  return {
    agentModels,
    roles: DEMO_AGENT_ROLES,
    skills: DEMO_AGENT_SKILLS,
    mcps: DEMO_AGENT_MCPS,
    tools: DEMO_WORKFLOW_TOOLS,
    conditionOperators: DEMO_CONDITION_OPERATORS,
    toolOperations: DEMO_TOOL_OPERATIONS,
    defaultAgentConfig: {
      schemaVersion: 3,
      executor: {
        agentCli: defaultAgentModel.agentCli,
        modelId: defaultAgentModel.modelId,
      },
      roleId: DEFAULT_AGENT_ROLE,
      skills: [],
      mcps: [],
      prompt: '',
      interactive: false,
    },
    defaultTool: DEFAULT_WORKFLOW_TOOL,
  }
}
