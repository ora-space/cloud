import { replaceEqualDeep, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  Error as ApiError,
  GetApiV1TenantsTidWorkflowsWfidRuns200,
  Workflow as CloudWorkflow,
  WorkflowRun,
  WorkflowRunNodeStates,
  WorkflowRunStatus,
  WorkflowSnapshot,
} from '@/api/generated.schemas'
import {
  deleteApiV1TenantsTidWorkflowsWfid,
  getApiV1TenantsTidWorkflows,
  getApiV1TenantsTidWorkflowsWfid,
  getApiV1TenantsTidWorkflowsWfidRuns,
  getApiV1TenantsTidWorkflowsWfidRunsRid,
  getApiV1TenantsTidWorkflowsWfidSnapshots,
  postApiV1TenantsTidWorkflows,
  postApiV1TenantsTidWorkflowsWfidPublish,
  postApiV1TenantsTidWorkflowsWfidRuns,
  putApiV1TenantsTidWorkflowsWfid,
  putApiV1TenantsTidWorkflowsWfidSnapshotsSnapshotIdRestore,
} from '@/api/workflows/workflows'
import { asJsonRecord, isJsonRecord } from '@/features/workflows/runtime/json-record'
import { isTerminalRunStatus } from '@/features/workflows/runtime/types'
import { mutationHeaders, useIdempotencyKeys } from '@/features/spaces/api'
import type { ErrorType } from '@/lib/api-client'

/**
 * Query key of a tenant's workflow list.
 *
 * The key is the generated client's own URL, matching what orval's hooks use, so an
 * invalidation written here also refreshes a list rendered through the generated hook.
 *
 * @param tenantId - Tenant the list is scoped to, or `undefined` while it resolves.
 * @returns The query key.
 */
export function workflowListKey(tenantId: string | undefined) {
  return [`/api/v1/tenants/${tenantId}/workflows`]
}

/** Query key of one workflow, kept separate so a detail page can invalidate alone. */
function workflowDetailKey(tenantId: string | undefined, workflowId: string | undefined) {
  return [`/api/v1/tenants/${tenantId}/workflows/${workflowId}`]
}

/** Query key of a workflow's published-snapshot history. */
function workflowSnapshotsKey(tenantId: string | undefined, workflowId: string | undefined) {
  return [`/api/v1/tenants/${tenantId}/workflows/${workflowId}/snapshots`]
}

/** Query key of a workflow's run history. */
function workflowRunsKey(tenantId: string | undefined, workflowId: string | undefined) {
  return [`/api/v1/tenants/${tenantId}/workflows/${workflowId}/runs`]
}

/** Query key of one workflow run, separate so the detail page can invalidate alone. */
function workflowRunKey(
  tenantId: string | undefined,
  workflowId: string | undefined,
  runId: string | undefined,
) {
  return [`/api/v1/tenants/${tenantId}/workflows/${workflowId}/runs/${runId}`]
}

/**
 * The tenant's workflows, newest page first.
 *
 * The list is pending rather than empty until the tenant resolves, so the page shows a
 * skeleton instead of flashing its empty state.
 *
 * @param tenantId - Tenant to list workflows for.
 * @returns The list query; `data.items` is the page of workflows.
 */
export function useWorkflows(tenantId: string | undefined) {
  return useQuery({
    queryKey: workflowListKey(tenantId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidWorkflows(tenantId ?? '', undefined, undefined, signal),
    enabled: !!tenantId,
  })
}

/**
 * One workflow by id, including its graph document.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @param workflowId - Workflow to read.
 * @returns The detail query, disabled until both ids are present.
 */
export function useWorkflow(tenantId: string | undefined, workflowId: string | undefined) {
  return useQuery({
    queryKey: workflowDetailKey(tenantId, workflowId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidWorkflowsWfid(tenantId ?? '', workflowId ?? '', undefined, signal),
    enabled: !!tenantId && !!workflowId,
  })
}

/** Input for creating a workflow; the graph is optional and defaults to an empty canvas. */
export interface CreateWorkflowInput {
  name: string
  description?: string
  /** Authored graph document to store; omit for a fresh empty canvas. */
  graph?: Record<string, unknown>
}

/**
 * Creates a workflow in the current tenant.
 *
 * The name is unique among live workflows, so a taken name is rejected with `workflow_conflict`
 * rather than merged. An `import` writes the whole imported graph document in the same call.
 * Creation is synchronous: the response is the stored workflow.
 *
 * @returns The create mutation; `data` is the created workflow.
 */
export function useCreateWorkflow(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<{ resource: CloudWorkflow }, ErrorType<ApiError>, CreateWorkflowInput>({
    mutationFn: (input: CreateWorkflowInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return postApiV1TenantsTidWorkflows(
        tenantId,
        {
          name: input.name,
          ...(input.description ? { description: input.description } : {}),
          ...(input.graph === undefined ? {} : { graph: input.graph }),
        },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: (created) => {
      queryClient.setQueryData(workflowDetailKey(tenantId, created.resource.id), created.resource)
      void queryClient.invalidateQueries({ queryKey: workflowListKey(tenantId) })
    },
  })
}

/** Input for renaming a workflow; `version` guards against a concurrent edit. */
export interface RenameWorkflowInput {
  id: string
  name: string
  version: number
}

/**
 * Renames a workflow.
 *
 * `version` is the optimistic guard the API requires: a stale value is rejected with 409, which
 * the caller surfaces as "changed elsewhere" rather than silently overwriting.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The rename mutation.
 */
export function useRenameWorkflow(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  return useMutation<CloudWorkflow, ErrorType<ApiError>, RenameWorkflowInput>({
    mutationFn: (input: RenameWorkflowInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return putApiV1TenantsTidWorkflowsWfid(tenantId, input.id, {
        name: input.name,
        version: input.version,
      })
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(workflowDetailKey(tenantId, updated.id), updated)
      void queryClient.invalidateQueries({ queryKey: workflowListKey(tenantId) })
    },
  })
}

/** Input for saving a workflow's graph; `version` guards against a concurrent edit. */
export interface UpdateWorkflowGraphInput {
  id: string
  /** Name the save carries, because the endpoint requires one on every write. */
  name: string
  graph: Record<string, unknown>
  version: number
}

/**
 * Saves a workflow's graph document.
 *
 * The whole document is replaced on every write: the backend stores it as an opaque jsonb
 * value, so there is no field-level merge to attempt and no partial update to send. `version`
 * is the same optimistic guard the rename uses — a concurrent save is rejected with 409 rather
 * than silently overwriting the other editor's work.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The save mutation; `data` is the workflow at its new version.
 */
export function useUpdateWorkflowGraph(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  return useMutation<CloudWorkflow, ErrorType<ApiError>, UpdateWorkflowGraphInput>({
    mutationFn: (input: UpdateWorkflowGraphInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return putApiV1TenantsTidWorkflowsWfid(tenantId, input.id, {
        name: input.name,
        graph: input.graph,
        version: input.version,
      })
    },
    onSuccess: (updated) => {
      queryClient.setQueryData(workflowDetailKey(tenantId, updated.id), updated)
      void queryClient.invalidateQueries({ queryKey: workflowListKey(tenantId) })
    },
  })
}

/** Input for archiving a workflow; `version` guards against a concurrent edit. */
export interface DeleteWorkflowInput {
  id: string
  version: number
}

/**
 * Archives a workflow (soft delete). A `DELETE` must carry an idempotency key, so a retry of the
 * same request replays the stored response instead of archiving twice.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The delete mutation.
 */
export function useDeleteWorkflow(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<CloudWorkflow, ErrorType<ApiError>, DeleteWorkflowInput>({
    mutationFn: (input: DeleteWorkflowInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return deleteApiV1TenantsTidWorkflowsWfid(
        tenantId,
        input.id,
        { version: input.version },
        { headers: mutationHeaders(keyFor(input)) },
      )
    },
    onSuccess: (archived) => {
      queryClient.removeQueries({ queryKey: workflowDetailKey(tenantId, archived.id) })
      void queryClient.invalidateQueries({ queryKey: workflowListKey(tenantId) })
    },
  })
}

/**
 * Published snapshots of a workflow, newest version first.
 *
 * The backend pages the history by row UUID, so the query fetches the full list
 * (snapshot counts are small) and sorts by version client-side; version numbers
 * are assigned at publish time and never reused.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @param workflowId - Workflow whose published history to read.
 * @returns The history, newest version first, or `undefined` while resolving.
 */
export function useWorkflowSnapshots(tenantId: string | undefined, workflowId: string | undefined) {
  return useQuery({
    queryKey: workflowSnapshotsKey(tenantId, workflowId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidWorkflowsWfidSnapshots(
        tenantId ?? '',
        workflowId ?? '',
        { limit: 100 },
        undefined,
        signal,
      ),
    enabled: !!tenantId && !!workflowId,
    select: (page) => [...page.items].toSorted((a, b) => b.version - a.version),
  })
}

/** Input for publishing a workflow's live graph as the next snapshot. */
export interface PublishWorkflowInput {
  workflowId: string
  /** Optional name for the released version; defaults to the workflow name. */
  name?: string
}

/**
 * Freezes the workflow's live graph as the next immutable snapshot.
 *
 * Publishing never edits the workflow itself: no version is required, and the
 * workflow's own document version is unchanged. A `POST` must carry an
 * idempotency key, so a retried publish returns the already-stored snapshot
 * instead of minting a duplicate version.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The publish mutation; `data` is the created snapshot.
 */
export function usePublishWorkflow(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<WorkflowSnapshot, ErrorType<ApiError>, PublishWorkflowInput>({
    mutationFn: (input: PublishWorkflowInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      const body = input.name === undefined || input.name === '' ? {} : { name: input.name }
      return postApiV1TenantsTidWorkflowsWfidPublish(tenantId, input.workflowId, body, {
        headers: mutationHeaders(keyFor(input)),
      }).then((created) => created.resource)
    },
    onSuccess: (_snapshot, input) => {
      void queryClient.invalidateQueries({
        queryKey: workflowSnapshotsKey(tenantId, input.workflowId),
      })
    },
  })
}

/** Input for rolling the live graph back to a published snapshot. */
export interface RestoreWorkflowSnapshotInput {
  workflowId: string
  snapshotId: string
  /** The workflow's current document version; a stale value is a 409. */
  version: number
}

/**
 * Poll interval while a run can still change.
 *
 * A workflow run publishes no space event, so nothing pushes its progress to an
 * open page; a bounded poll is the honest stand-in until the backend owns one.
 * The cadence matches the issue-run list's settling poll, which covers the same
 * "nothing is published when a run settles" gap.
 */
export const WORKFLOW_RUN_POLL_MS = 5000

/**
 * How often a run read polls, given the outcome of the last attempt.
 *
 * A settled run stops the poll: nothing can change it again, so re-reading it
 * would only spend requests. A status this build does not recognise keeps
 * polling on purpose — freezing on a value the viewer cannot classify would
 * silently stop updating a run the server is still advancing. A failed read
 * keeps polling too: the failure says nothing about whether the run settled,
 * only that this attempt did not see it, so the next attempt must still happen.
 *
 * @param status - Status of the last successful read, or `undefined` when no read has succeeded yet.
 * @param failed - Whether the last attempt failed instead of returning a run.
 * @returns The interval in milliseconds, or `false` when there is nothing to keep up with.
 */
export function runPollInterval(
  status: WorkflowRunStatus | undefined,
  failed: boolean,
): number | false {
  if (status === undefined) return failed ? WORKFLOW_RUN_POLL_MS : false
  return isTerminalRunStatus(status) ? false : WORKFLOW_RUN_POLL_MS
}

/**
 * How often the run history polls: while any listed run can still change.
 *
 * A failed read keeps polling for the same reason the single-run read does:
 * the failure says nothing about whether the listed runs settled, so the next
 * attempt must still happen.
 *
 * @param items - Runs of the last successful page, or `undefined` when no read has succeeded yet.
 * @param failed - Whether the last attempt failed instead of returning a page.
 * @returns The interval in milliseconds, or `false` when every listed run is settled.
 */
export function runsPollInterval(
  items: WorkflowRun[] | undefined,
  failed: boolean,
): number | false {
  if (items === undefined) return failed ? WORKFLOW_RUN_POLL_MS : false
  return items.some((run) => !isTerminalRunStatus(run.status)) ? WORKFLOW_RUN_POLL_MS : false
}

/**
 * Merges a run read into the copy already held, so a late or stale response
 * cannot undo a newer one.
 *
 * A run row is the evidence for what happened, and a read is only a snapshot of
 * it: a poll can overlap a write, or be answered by a replica that has not caught
 * up, and then arrive after a newer read already landed. Two rules make that
 * harmless — a settled run is never reopened by an unsettled read, and between
 * reads of the same kind the newer `updatedAt` wins. Node states are then merged
 * node by node on top of the winning record, so a node the page is already
 * showing is never blanked by a read that raced a partial write.
 *
 * @param previous - The run already cached, if any.
 * @param next - The run the latest read returned.
 * @returns The run to keep: one of the two inputs, or a copy of the winner
 *   carrying the merged node states.
 */
export function mergeRunRead(previous: WorkflowRun | undefined, next: WorkflowRun): WorkflowRun {
  if (previous === undefined) return next
  const keepNext = prefersNextRead(previous, next)
  const winner = keepNext ? next : previous
  const other = keepNext ? previous : next
  const nodeStates = mergeNodeStates(winner.nodeStates, other.nodeStates)
  return nodeStates === winner.nodeStates ? winner : { ...winner, nodeStates }
}

/** Whether `next` is the read the cache should keep. */
function prefersNextRead(previous: WorkflowRun, next: WorkflowRun): boolean {
  const settled = isTerminalRunStatus(next.status)
  if (settled !== isTerminalRunStatus(previous.status)) return settled
  return runUpdatedAt(next) >= runUpdatedAt(previous)
}

/** The run's `updatedAt` as an epoch, so ordering does not lean on the server's string format. */
function runUpdatedAt(run: WorkflowRun): number {
  const parsed = Date.parse(run.updatedAt)
  return Number.isNaN(parsed) ? Number.NEGATIVE_INFINITY : parsed
}

/**
 * Merges two node-state maps into the winning record's, node by node.
 *
 * The winner keeps its own entry for every node it knows, because its node
 * states were written with the row the comparison chose. Entries only the other
 * read carried are kept: node ids come from the run's frozen snapshot, so a node
 * never disappears, and a read that raced a node's write must not blank what the
 * page is already showing. The winner's map is returned unchanged when the other
 * read had nothing extra, so an idle poll does not churn the cache.
 */
function mergeNodeStates(
  winner: WorkflowRunNodeStates,
  other: WorkflowRunNodeStates,
): WorkflowRunNodeStates {
  const extra = Object.keys(other).filter((nodeId) => !(nodeId in winner))
  if (extra.length === 0) return winner
  const merged: WorkflowRunNodeStates = { ...winner }
  for (const nodeId of extra) merged[nodeId] = other[nodeId]
  return merged
}

/**
 * Merges a run-history page into the one already held, so one run cannot read
 * differently in the list and on its detail page.
 *
 * Membership follows the incoming page: the server decides which runs exist, and
 * the page is capped, so keeping rows the server no longer returns would grow
 * the list without bound. What each surviving run says about itself is merged
 * with the same rule the detail page uses, which is what keeps the two surfaces
 * agreeing.
 *
 * @param previous - The page already cached, if any.
 * @param next - The page the latest read returned.
 * @returns The page to keep.
 */
export function mergeRunPage(
  previous: GetApiV1TenantsTidWorkflowsWfidRuns200 | undefined,
  next: GetApiV1TenantsTidWorkflowsWfidRuns200,
): GetApiV1TenantsTidWorkflowsWfidRuns200 {
  if (previous === undefined) return next
  const held = new Map(previous.items.map((run) => [run.id, run]))
  return { ...next, items: next.items.map((run) => mergeRunRead(held.get(run.id), run)) }
}

/**
 * True when a value is a run row the merge can read.
 *
 * TanStack types `structuralSharing` over `unknown`, so its arguments need
 * narrowing before the merge can take them. What it passes is what this module's
 * own `queryFn` returned — never wire data parsed somewhere else — so the check
 * only has to cover the fields the merge reads, not the whole row.
 */
function isMergeableRun(value: unknown): value is WorkflowRun {
  const record = asJsonRecord(value)
  if (record === undefined) return false
  return (
    typeof record['id'] === 'string' &&
    typeof record['status'] === 'string' &&
    typeof record['updatedAt'] === 'string' &&
    isJsonRecord(record['nodeStates'])
  )
}

/** True when a value is a run-history page whose rows the merge can read. */
function isMergeableRunPage(value: unknown): value is GetApiV1TenantsTidWorkflowsWfidRuns200 {
  const record = asJsonRecord(value)
  if (record === undefined) return false
  const { items, nextCursor } = record
  if (!Array.isArray(items) || typeof nextCursor !== 'string') return false
  const rows: unknown[] = items
  return rows.every(isMergeableRun)
}

/**
 * The `structuralSharing` adapter for one run read.
 *
 * TanStack calls this as it writes the read into the cache, which is the only
 * point where the comparison sees the cache's current value; merging after the
 * fact could interleave with a concurrent read's write. A value this module did
 * not produce is passed straight through, so an unexpected cache entry replaces
 * itself rather than being dropped.
 *
 * A custom function replaces TanStack's default structural sharing rather than
 * adding to it, so the merge is followed by `replaceEqualDeep` — the default —
 * to keep the identity of an unchanged run. Without it every poll would hand the
 * page a new object, and the frozen graph would be re-laid out every few seconds
 * for a run that has not moved.
 */
function shareRunRead(previous: unknown, next: unknown): unknown {
  if (!isMergeableRun(next)) return next
  const held = isMergeableRun(previous) ? previous : undefined
  return replaceEqualDeep(held, mergeRunRead(held, next))
}

/** The `structuralSharing` adapter for the run history, over {@link mergeRunPage}'s rule. */
function shareRunPage(previous: unknown, next: unknown): unknown {
  if (!isMergeableRunPage(next)) return next
  const held = isMergeableRunPage(previous) ? previous : undefined
  return replaceEqualDeep(held, mergeRunPage(held, next))
}

/**
 * A workflow's run history, newest first.
 *
 * The backend pages runs by row UUID, so the query fetches up to 100 rows and
 * sorts by `createdAt` client-side. It keeps re-reading while a listed run is
 * still moving, so the popover shows a run settling without being reopened.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @param workflowId - Workflow whose runs to read.
 * @returns The runs, newest `createdAt` first, or `undefined` while resolving.
 */
export function useWorkflowRuns(tenantId: string | undefined, workflowId: string | undefined) {
  return useQuery({
    queryKey: workflowRunsKey(tenantId, workflowId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidWorkflowsWfidRuns(
        tenantId ?? '',
        workflowId ?? '',
        { limit: 100 },
        undefined,
        signal,
      ),
    enabled: !!tenantId && !!workflowId,
    select: (page) => [...page.items].toSorted((a, b) => b.createdAt.localeCompare(a.createdAt)),
    refetchInterval: (query) =>
      runsPollInterval(query.state.data?.items, query.state.status === 'error'),
    structuralSharing: shareRunPage,
  })
}

/**
 * One workflow run by id, including its frozen snapshot graph and node states.
 *
 * The read keeps refreshing while the run is still moving, so an open detail
 * page follows a run from `pending` to its terminal status without a reload, and
 * stops on its own once the run settles.
 *
 * @param tenantId - Tenant the run belongs to.
 * @param workflowId - Workflow the run belongs to.
 * @param runId - Run to read.
 * @returns The detail query, disabled until all three ids are present.
 */
export function useWorkflowRun(
  tenantId: string | undefined,
  workflowId: string | undefined,
  runId: string | undefined,
) {
  return useQuery({
    queryKey: workflowRunKey(tenantId, workflowId, runId),
    queryFn: ({ signal }) =>
      getApiV1TenantsTidWorkflowsWfidRunsRid(
        tenantId ?? '',
        workflowId ?? '',
        runId ?? '',
        undefined,
        signal,
      ),
    enabled: !!tenantId && !!workflowId && !!runId,
    refetchInterval: (query) =>
      runPollInterval(query.state.data?.status, query.state.status === 'error'),
    structuralSharing: shareRunRead,
  })
}

/** Input for running a workflow once against a published snapshot. */
export interface CreateWorkflowRunInput {
  workflowId: string
  /**
   * The Project the run's Workspace provisions in. The contract requires it — a run that names
   * none is refused with 409 `workflow_run_project_required` — so the mutation refuses a caller
   * that has none instead of sending a body the contract does not describe.
   */
  projectId: string
  /** Optional display name; defaults to the workflow's name. */
  name?: string
  /** The exact snapshot to execute, or omit to pin the latest published one. */
  snapshotId?: string
  /** The run's input object: form values keyed by variable name, plus the reserved `input` kickoff text. */
  input?: Record<string, unknown>
}

/**
 * Creates a workflow run.
 *
 * The run is pinned to a published snapshot: without one the request is rejected
 * with `workflow_no_published_snapshot`, exactly like a desktop run. On the dev
 * wiring a simulated executor fills the trace synchronously; in production the
 * row stays `pending` until a real engine picks it up. A `POST` must carry an
 * idempotency key, so a retried run replays the stored response instead of
 * minting a second execution.
 *
 * The run's Project is required by the contract — a run that names none is refused with 409
 * `workflow_run_project_required` — and the run form cannot submit before one is chosen, so the
 * refusal here is the backstop for a caller that builds its own request.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The create mutation; `data` is the stored run.
 */
export function useCreateWorkflowRun(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  const keyFor = useIdempotencyKeys()
  return useMutation<WorkflowRun, ErrorType<ApiError>, CreateWorkflowRunInput>({
    mutationFn: (input: CreateWorkflowRunInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      // Refused here rather than sent: a run with no frozen Project is an identity-incomplete
      // aggregate that can never provision the Workspace its agent nodes need, so the service
      // answers this same code (409) and there is no value the client could put in the field
      // instead — the caller either names a Project or names none.
      if (input.projectId === '') {
        throw new Error('workflow_run_project_required')
      }
      return postApiV1TenantsTidWorkflowsWfidRuns(
        tenantId,
        input.workflowId,
        {
          projectId: input.projectId,
          ...(input.name === undefined || input.name === '' ? {} : { name: input.name }),
          ...(input.snapshotId === undefined || input.snapshotId === ''
            ? {}
            : { snapshotId: input.snapshotId }),
          ...(input.input === undefined ? {} : { input: input.input }),
        },
        { headers: mutationHeaders(keyFor(input)) },
      ).then((created) => created.resource)
    },
    onSuccess: (_run, input) => {
      void queryClient.invalidateQueries({
        queryKey: workflowRunsKey(tenantId, input.workflowId),
      })
    },
  })
}

/**
 * Replaces the workflow's live graph with a published snapshot's, advancing the
 * document version like any editing write.
 *
 * The restore carries the same optimistic guard an edit does, so rolling back a
 * version that another editor changed concurrently reports a 409 instead of
 * overwriting their work.
 *
 * @param tenantId - Tenant the workflow belongs to.
 * @returns The restore mutation; `data` is the workflow at the restored graph.
 */
export function useRestoreWorkflowSnapshot(tenantId: string | undefined) {
  const queryClient = useQueryClient()
  return useMutation<CloudWorkflow, ErrorType<ApiError>, RestoreWorkflowSnapshotInput>({
    mutationFn: (input: RestoreWorkflowSnapshotInput) => {
      if (!tenantId) throw new Error('cloud tenant not resolved')
      return putApiV1TenantsTidWorkflowsWfidSnapshotsSnapshotIdRestore(
        tenantId,
        input.workflowId,
        input.snapshotId,
        { version: input.version },
      )
    },
    onSuccess: (restored) => {
      queryClient.setQueryData(workflowDetailKey(tenantId, restored.id), restored)
    },
  })
}
