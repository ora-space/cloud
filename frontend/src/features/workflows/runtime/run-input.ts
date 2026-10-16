import type { WorkflowGraphEnvelope } from '@/features/workflows/runtime/graph-codec'
import { resolveWorkflowInputFieldType } from '@/features/workflows/runtime/start-input'
import type {
  WorkflowGlobalVariable,
  WorkflowInputFieldType,
  WorkflowInputVariable,
  WorkflowLaunchField,
  WorkflowNodeData,
} from '@/features/workflows/runtime/types'
import { WORKFLOW_LAUNCH_FIELD_DEFAULTS } from '@/features/workflows/runtime/types'

/** The control the manual run form renders for one answer. */
export type WorkflowRunControl = 'text' | 'textarea' | 'number' | 'boolean' | 'select'

/**
 * One question the manual run form asks, and the key its answer travels under.
 *
 * The key is the run's own vocabulary: the answers are submitted as the run's `input` object, keyed
 * exactly the way the frozen graph names them, because that is the object the start gate validates.
 */
export interface WorkflowRunField {
  key: string
  /** What the member reads above the control. */
  label: string
  /** Whether a run may start without an answer. */
  required: boolean
  control: WorkflowRunControl
  /** The choices a `select` offers, in declaration order; empty for every other control. */
  options: string[]
  /** The answer the form opens with, when the frozen graph declares one. */
  defaultValue?: unknown
}

/** The answers a member has given, keyed by field. */
export type WorkflowRunValues = Record<string, unknown>

/** The key the platform reads a run's kickoff prompt from. */
const PROMPT_FIELD_KEY = 'prompt'

/**
 * The bounds the server puts on one projected form, mirrored so the two projections agree.
 *
 * The server owns these numbers — `maxFormFields`, `maxFormOptions`, `maxFieldValueLength` and the
 * 200-byte bound it puts on a field key, a field label and a select choice alike in
 * `internal/core/form_descriptor.go` and `workflow_collaboration.go` — and it validates the submitted
 * input against the same document this form is projected from. A bound only one side applies is
 * either a question the run cannot answer or a refusal of an answer the form accepted, so the form
 * reads the server's limits rather than leaving them to drift. They count UTF-8 bytes, which is what
 * Go's `len` counts, so a Chinese label is bounded at a third of the characters an ASCII one is.
 */
const MAX_FORM_FIELDS = 100
const MAX_FORM_OPTIONS = 100
const MAX_FIELD_LABEL = 200
const MAX_FIELD_VALUE = 4000

/** The grammar a field key may use, mirroring the server's `validOpaqueToken`. */
const OPAQUE_TOKEN = /^[A-Za-z0-9._:@-]+$/

/**
 * The control each declared Start field type degrades to in the run form.
 *
 * This mirrors the server's own projection onto the `@` form, including its degradations, because
 * the two have to agree: the server validates the submitted input against the same document, and a
 * field only one side knows about is either a question with no answer or an answer with no question.
 * A file reference and a JSON document both arrive as the text that carries them, since neither
 * surface has an upload or structured-value control.
 */
const CONTROL_BY_FIELD_TYPE: Record<WorkflowInputFieldType, WorkflowRunControl> = {
  'text-input': 'text',
  paragraph: 'textarea',
  select: 'select',
  number: 'number',
  checkbox: 'boolean',
  file: 'text',
  'file-list': 'textarea',
  json: 'textarea',
}

/** The control a global variable's value type degrades to, matching the Start variables' own. */
const CONTROL_BY_VALUE_TYPE: Record<string, WorkflowRunControl> = {
  number: 'number',
  integer: 'number',
  boolean: 'boolean',
  string: 'text',
  secret: 'text',
}

/**
 * Projects a frozen graph onto the questions the manual run form asks.
 *
 * The fields come from the document the run will execute, never from the live draft: a member who
 * picked a version published three weeks ago answers that version's variables, and an edit saved
 * since cannot change what the form asks, what it prefills, or what it insists on. The projection
 * mirrors the server's `workflowInputFields` — the kickoff prompt first, then the Start node's
 * declared variables, then the dotted global variables that stand in for run-time parameters — and
 * the server's bounds on it, so the two sides never disagree about which fields exist.
 *
 * @param graph - The selected version's parsed graph envelope.
 * @returns The fields in the order the form shows them. A graph that declares nothing yields an
 *   empty list rather than a partial one.
 */
export function workflowRunFields(graph: WorkflowGraphEnvelope): WorkflowRunField[] {
  const start = startNodeData(graph)
  const declared = declaredFields(start)
  const globals = globalFields(graph, declared)
  // The server stops adding its own fields once the author's fill the descriptor, because appending
  // past the bound would make the descriptor invalid and turn every start into a 500. The kickoff
  // prompt is the one platform field this form adds, so it is the one that gives way.
  const full = declared.length + globals.length === MAX_FORM_FIELDS
  const prompt = full ? [] : promptFields(graph, declared, start)
  return [...prompt, ...declared, ...globals]
}

/**
 * The answers the form opens with, taken from the version's own declared defaults.
 *
 * A field with no usable default is left out rather than seeded with an empty string, so the form
 * can tell "the member has not answered" from "the member answered with nothing" — which is what
 * makes the required-field gate below mean what it says.
 *
 * @param fields - The fields {@link workflowRunFields} projected.
 * @returns The initial answers, keyed by field.
 */
export function initialWorkflowRunValues(fields: readonly WorkflowRunField[]): WorkflowRunValues {
  const values: WorkflowRunValues = {}
  for (const field of fields) {
    if (field.defaultValue !== undefined && field.defaultValue !== null) {
      values[field.key] = field.defaultValue
    }
  }
  return values
}

/**
 * The required fields the member has not answered yet.
 *
 * This is a courtesy, not the gate: the server re-checks the same questions against the same frozen
 * document and answers `required_field_missing`. Asking here spares a round trip, and a disagreement
 * between the two resolves in the server's favour, since it is the one that decides.
 *
 * @param fields - The fields {@link workflowRunFields} projected.
 * @param values - The answers so far.
 * @returns The unanswered required fields, in form order.
 */
export function missingWorkflowRunFields(
  fields: readonly WorkflowRunField[],
  values: WorkflowRunValues,
): WorkflowRunField[] {
  return fields.filter((field) => field.required && isUnanswered(values[field.key]))
}

/**
 * Builds the run's `input` object from the member's answers.
 *
 * Unanswered fields are left out entirely rather than sent as `null` or `''`: the gate treats a
 * missing key and a null one the same, and an absent key keeps the submitted object a statement of
 * what the member actually chose. Every other answer is passed through as typed, because the
 * controls produce the values the gate accepts and second-guessing them here would only hide a bug.
 *
 * @param fields - The fields {@link workflowRunFields} projected.
 * @param values - The member's answers.
 * @returns The `input` object for the run creation request.
 */
export function workflowRunInput(
  fields: readonly WorkflowRunField[],
  values: WorkflowRunValues,
): Record<string, unknown> {
  const input: Record<string, unknown> = {}
  for (const field of fields) {
    const value = values[field.key]
    if (isUnanswered(value)) continue
    input[field.key] = value
  }
  return input
}

/** True for an answer the form should treat as not given. */
function isUnanswered(value: unknown): boolean {
  if (value === undefined || value === null) return true
  return typeof value === 'string' && value.trim() === ''
}

/** The Start node's data, or undefined for a graph that has no Start node. */
function startNodeData(graph: WorkflowGraphEnvelope): WorkflowNodeData | undefined {
  return graph.nodes.find((node) => node.data.kind === 'start')?.data
}

/** Maps every declared Start variable onto a field, dropping the ones the form cannot name. */
function declaredFields(start: WorkflowNodeData | undefined): WorkflowRunField[] {
  const fields: WorkflowRunField[] = []
  for (const variable of start?.inputVariables ?? []) {
    if (fields.length === MAX_FORM_FIELDS) break
    const field = startVariableField(variable, start?.input ?? '')
    if (field !== undefined) fields.push(field)
  }
  return fields
}

/**
 * The platform's kickoff-prompt field, when it is the platform's to offer.
 *
 * The prompt is not a declared variable but the platform's own field, so its default is the Start
 * node's initial prompt: an author who wrote one gets it prefilled and can edit it for this run.
 * The field is dropped when the workflow's declaration turned it off, and — like every other
 * platform field — when the author declared that key in their own Start node, because a key the
 * author declared is not the platform's to withdraw.
 */
function promptFields(
  graph: WorkflowGraphEnvelope,
  declared: readonly WorkflowRunField[],
  start: WorkflowNodeData | undefined,
): WorkflowRunField[] {
  if (declared.some((field) => field.key === PROMPT_FIELD_KEY)) return []
  if (!launchFieldAsks(graph.launchFields, PROMPT_FIELD_KEY)) return []
  const field: WorkflowRunField = {
    key: PROMPT_FIELD_KEY,
    label: PROMPT_FIELD_KEY,
    required: launchFieldRequired(graph.launchFields, PROMPT_FIELD_KEY),
    control: 'textarea',
    options: [],
  }
  const initial = start?.input?.trim() ?? ''
  if (initial !== '') field.defaultValue = initial
  return [field]
}

/**
 * The dotted global variables, as parameters one run may override.
 *
 * A global name always contains a dot — the editor's own guard, mirrored here — which is what keeps
 * these keys from colliding with a declared variable or a platform field. A global is a
 * workflow-wide constant this invocation may override, never a required answer, so leaving one
 * untouched runs the workflow with exactly the constant it declares.
 *
 * They share the descriptor's field bound with the declared variables rather than getting one of
 * their own, because the server appends them to the same list: the author's own questions come first
 * and the globals fill whatever room is left.
 */
function globalFields(
  graph: WorkflowGraphEnvelope,
  declared: readonly WorkflowRunField[],
): WorkflowRunField[] {
  const fields: WorkflowRunField[] = []
  const seen = new Set(declared.map((field) => field.key))
  for (const variable of graph.globalVariables) {
    if (declared.length + fields.length === MAX_FORM_FIELDS) break
    if (!variable.name.includes('.') || !isFieldKey(variable.name) || seen.has(variable.name)) {
      continue
    }
    seen.add(variable.name)
    fields.push(globalVariableField(variable))
  }
  return fields
}

/** The UTF-8 byte length of a string, the unit every bound above counts in. */
function utf8Length(value: string): number {
  return new TextEncoder().encode(value).length
}

/**
 * Whether a variable name is one the server would project as a field key.
 *
 * A name outside the grammar is dropped by the server rather than rewritten, because the confirmed
 * values reach the workflow keyed by name: a sanitized key would feed the run the wrong variable. The
 * form drops it for the same reason — a field only this side knows about is a question whose answer
 * never arrives.
 */
function isFieldKey(name: string): boolean {
  return utf8Length(name) <= MAX_FIELD_LABEL && OPAQUE_TOKEN.test(name)
}

/**
 * The choices a declared select offers, filtered exactly the way the server filters them.
 *
 * The server drops a choice that is not a string, is empty, is over the same 200-byte bound a field
 * key and a label carry, repeats one already offered, or falls past the descriptor's option bound. A
 * choice the server dropped is not a value the gate accepts, so offering it would be a question whose
 * only answer comes back `invalid_field_value` — and a declared default among the dropped ones would
 * open the form on an answer it refuses. The graph is untrusted JSON and the codec validates its
 * shapes rather than every choice, so the string test the server makes is made here too.
 */
function selectOptions(variable: WorkflowInputVariable): string[] {
  const options: string[] = []
  const seen = new Set<string>()
  for (const option of variable.options ?? []) {
    if (typeof option !== 'string' || option === '') continue
    if (utf8Length(option) > MAX_FIELD_LABEL || seen.has(option)) continue
    if (options.length === MAX_FORM_OPTIONS) continue
    seen.add(option)
    options.push(option)
  }
  return options
}

/**
 * Accepts a declared value as a default only when the control can render it.
 *
 * A default the control cannot show would seed the form with an answer the member never gave and
 * the gate would reject, which is worse than opening the field empty. This mirrors the server's own
 * check on the defaults it hands the `@` form, including its bound on one text value: a paragraph
 * over it is dropped by the server, so it is not a default this form may open on.
 */
function acceptsValue(
  control: WorkflowRunControl,
  options: readonly string[],
  value: unknown,
): boolean {
  switch (control) {
    case 'number':
      return typeof value === 'number' && Number.isFinite(value)
    case 'boolean':
      return typeof value === 'boolean'
    case 'select':
      return typeof value === 'string' && options.includes(value)
    default:
      return typeof value === 'string' && utf8Length(value) <= MAX_FIELD_VALUE
  }
}

/** Maps one declared Start variable onto a form field, or drops it when it cannot be asked about. */
function startVariableField(
  variable: WorkflowInputVariable,
  initialPrompt: string,
): WorkflowRunField | undefined {
  const key = variable.name
  if (!isFieldKey(key)) return undefined
  // The server decides whether the variable is a field at all from its own label — the declared
  // display name, or the key when the author left one empty — and drops the whole field when that
  // label is over the bound rather than clipping it. The check therefore reads the label untrimmed;
  // only what the form shows is trimmed.
  const declaredLabel = variable.displayName ?? ''
  if (utf8Length(declaredLabel || key) > MAX_FIELD_LABEL) return undefined
  const fieldType = resolveWorkflowInputFieldType(variable)
  const options = fieldType === 'select' ? selectOptions(variable) : []
  // A select with nothing to choose from cannot be answered at all, so it becomes a text field —
  // the same degradation the server applies, which keeps the form usable instead of unanswerable.
  const control =
    fieldType === 'select' && options.length === 0 ? 'text' : CONTROL_BY_FIELD_TYPE[fieldType]
  const field: WorkflowRunField = {
    key,
    label: declaredLabel.trim() || key,
    required: variable.required === true,
    control,
    options,
  }
  if (acceptsValue(control, options, variable.value)) {
    field.defaultValue = variable.value
  } else if (key === PROMPT_FIELD_KEY && initialPrompt.trim() !== '') {
    // The author declared the platform's own prompt key. The platform does not offer its field
    // then, but it still hands its default to theirs — and only when they left the value empty.
    const initial = initialPrompt.trim()
    if (acceptsValue(control, options, initial)) field.defaultValue = initial
  }
  return field
}

/** Maps one dotted global variable onto a form field. */
function globalVariableField(variable: WorkflowGlobalVariable): WorkflowRunField {
  const control = CONTROL_BY_VALUE_TYPE[variable.valueType] ?? 'textarea'
  const field: WorkflowRunField = {
    key: variable.name,
    label: variable.name,
    required: false,
    control,
    options: [],
  }
  if (acceptsValue(control, [], variable.value)) field.defaultValue = variable.value
  return field
}

/** Resolves whether the workflow's declaration still asks for one platform launch field. */
function launchFieldAsks(fields: readonly WorkflowLaunchField[], key: string): boolean {
  return fields.find((field) => field.key === key)?.enabled ?? true
}

/** Resolves whether the declaration insists on an answer, keeping the platform's own default. */
function launchFieldRequired(fields: readonly WorkflowLaunchField[], key: string): boolean {
  const declared = fields.find((field) => field.key === key)
  if (declared?.required !== undefined) return declared.required
  return WORKFLOW_LAUNCH_FIELD_DEFAULTS.find((entry) => entry.key === key)?.required ?? false
}
