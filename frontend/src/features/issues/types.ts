/**
 * Cloud Issue domain types, mirroring the backend contract in `api/openapi.json`.
 *
 * These are the *real* shapes returned by `/api/v1/tenants/:tid/issues*` and are
 * deliberately separate from the MSW mock types in `@/mocks/data/types` (which the
 * non-issue preview features still use). The two must not drift into each other.
 */

/** Board priority levels; the backend validates against exactly these five. */
export type IssuePriority = 'urgent' | 'high' | 'medium' | 'low' | 'none'

/** Polymorphic assignee owner. `user` is resolved; `agent`/`team` are opaque this wave. */
export type AssigneeType = 'user' | 'agent' | 'team'

/** Tenant label attached to issues. */
export interface Label {
  id: string
  tenantId: string
  name: string
  color: string
  version: number
  createdAt: string
  updatedAt: string
}

/** A status column in the tenant's catalog (7 system columns seeded lazily + custom ones). */
export interface IssueStatusColumn {
  id: string
  tenantId: string
  key: string
  name: string
  description: string
  category: 'unstarted' | 'started' | 'done' | 'closed'
  color: string
  icon: string
  isSystem: boolean
  position: number
  version: number
  createdAt: string
  updatedAt: string
}

/** A board issue, as `SELECT i.*` plus the attached `labels` array. */
export interface Issue {
  id: string
  tenantId: string
  creatorUserId: string
  assigneeType: AssigneeType
  assigneeId: string | null
  assigneeUserId: string | null
  parentIssueId: string | null
  projectRef: string | null
  title: string
  description: string
  status: string
  priority: IssuePriority
  position: number
  number: number
  properties: Record<string, unknown>
  version: number
  createdAt: string
  updatedAt: string
  labels: Label[]
}

/** A threaded comment on an issue. `authorType` is polymorphic; only `user` is resolved. */
export interface IssueComment {
  id: string
  tenantId: string
  issueId: string
  authorType: 'user' | 'agent' | 'team' | 'system'
  authorId: string
  authorUserId: string | null
  parentId: string | null
  body: string
  seq: number
  version: number
  createdAt: string
  updatedAt: string
}

/** A queued issue run. It is persisted as `queued`; nothing dispatches it this wave. */
export interface IssueRun {
  id: string
  tenantId: string
  issueId: string
  executorType: 'agent' | 'team' | 'workflow'
  executorId: string
  input: Record<string, unknown>
  status: string
  /** Agent runs: the settled delivery, `{revisionId, deliveryState}`, once the run released its Workspace. */
  result?: RunResult | null
  /** Agent runs: the registered Revision as metadata only; null until one is registered. */
  revision?: RunRevision | null
  /** Agent runs: the Revision this run resumed, fixed when its session started; null for a fresh run. */
  resumeRevisionId?: string | null
  createdAt: string
  updatedAt: string
}

/** How an agent run's delivery settled (Cloud Revision D5, IssueRun D4). */
export interface RunResult {
  revisionId?: string | null
  deliveryState?: 'saved' | 'unchanged' | 'skipped' | 'failed'
}

/** Public metadata of a registered Revision; object keys and URLs are never exposed. */
export interface RunRevision {
  id: string
  baseCommit: string
  finalCommit: string
  changed: boolean
  bundleSize: number | null
  historySize: number
  /** The Revision the run resumed; with `changed: false` the run reused that Revision's bundle. */
  priorRevisionId?: string | null
  createdAt: string
}

/** Reference-not-copy pointer to an external resource attached to an issue. */
export interface ContextRef {
  id: string
  tenantId: string
  issueId: string
  refType:
    | 'parent_issue'
    | 'run'
    | 'timeline_message'
    | 'pull_request'
    | 'project'
    | 'workspace'
    | 'acceptance_criteria'
  refId: string
  createdAt: string
}

/** A tenant member, used to resolve user assignee display names. */
export interface TenantMember {
  id: string
  userId: string
  role: 'admin' | 'member'
  status: 'active' | 'disabled'
  version: number
  displayName: string
}

/** A tenant the signed-in user belongs to (from `/api/v1/me/tenants`). */
export interface Tenant {
  id: string
  name: string
  status: string
  role: string
}

/**
 * Collaboration target modes, the frozen §4 wire contract. The mode is derived
 * server-side from the target type: user -> mention, agent/team -> task, workflow -> form.
 */
export type InteractionMode = 'mention' | 'task' | 'form'

/** Descriptor attached to every collaboration target; drives the picker UI. */
export interface InteractionDescriptor {
  mode: InteractionMode
  requiresTask: boolean
  /** Form Mode only: opaque token used to load the FormDescriptor. Never parsed by the frontend. */
  formRef?: string | null
}

/** Comment/Collaboration target kinds recognized by the interaction spine. */
export type CollaborationTargetType = 'user' | 'agent' | 'team' | 'workflow'

/** Read-only projection from `GET /collaboration/targets` (humans + directory fixtures). */
export interface CollaborationTargetSummary {
  type: CollaborationTargetType
  id: string
  displayName: string
  description: string
  interactionDescriptor: InteractionDescriptor
}

/** One persisted `@` interaction (the spine), linked to its comment and optional run. */
export interface IssueInteraction {
  id: string
  tenantId: string
  issueId: string
  commentId: string
  targetType: CollaborationTargetType
  targetId: string
  mode: InteractionMode
  task: string
  runId: string | null
  /** Confirmed form values (Form Mode). Empty until the interaction is confirmed. */
  input: Record<string, unknown>
  createdAt: string
}

/**
 * Field types the Issues frontend can render. The set is closed: an unknown type is a descriptor bug
 * and is surfaced as an error state, never guessed at (§38.4).
 */
export type FormFieldType = 'text' | 'textarea' | 'number' | 'boolean' | 'select' | 'multi_select'

/** One allowed value for a select / multi_select field. */
export interface FormOption {
  value: string
  label: string
}

/** One rendered control, declared entirely by the provider. */
export interface FormField {
  key: string
  label: string
  type: FormFieldType
  required: boolean
  description?: string | null
  placeholder?: string | null
  defaultValue?: unknown
  options?: FormOption[] | null
}

/**
 * The Issues-facing rendering descriptor for a Workflow interaction. It is NOT the Workflow schema —
 * the frontend renders exactly what this declares and never branches on a workflow id (§38.2, §38.27).
 */
export interface FormDescriptor {
  formRef: string
  title?: string | null
  description?: string | null
  fields: FormField[]
}

/** A form-value map keyed by field key. */
export type FormValues = Record<string, unknown>

/** A suggested context ref. Applying one only affects the confirm payload, never persistent refs. */
export interface ContextRefRef {
  refType: ContextRef['refType']
  refId: string
}

/** AI Assist output: a field-level patch plus suggested refs. Suggest only — never applied server-side. */
export interface AssistSuggestion {
  suggestedValues: FormValues
  suggestedContextRefs: ContextRefRef[]
  explanations?: Record<string, string> | null
}

export type TimelineEntryKind = 'comment' | 'activity'

/**
 * A merged Timeline entry (comment + activity by shared per-issue seq). Every entry
 * carries the author ActorRef; only comments have a body/parent, only activities have
 * an action/details. Nullable either/or fields satisfy the closed OpenAPI contract.
 */
export interface TimelineEntry {
  kind: TimelineEntryKind
  id: string
  seq: number
  createdAt: string
  authorType: 'user' | 'agent' | 'team' | 'system'
  authorId: string | null
  authorUserId: string | null
  body: string | null
  parentId: string | null
  action: string | null
  details: Record<string, unknown> | null
}

/** A `@` target attached to a comment being composed. Task mode targets carry a task. */
export interface CommentTargetInput {
  type: CollaborationTargetType
  id: string
  task?: string
}

/** Paginated list envelope the public API returns for `items` endpoints. */
export interface Page<T> {
  items: T[]
  nextCursor: string
}
