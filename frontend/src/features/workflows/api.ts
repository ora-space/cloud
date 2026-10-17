import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  Error as ApiError,
  Workflow as CloudWorkflow,
  WorkflowRun,
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
 * A workflow's run history, newest first.
 *
 * The backend pages runs by row UUID, so the query fetches up to 100 rows and
 * sorts by `createdAt` client-side.
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
  })
}

/**
 * One workflow run by id, including its frozen snapshot graph and node states.
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
  })
}

/** Input for running a workflow once against a published snapshot. */
export interface CreateWorkflowRunInput {
  workflowId: string
  /**
   * The Project the run's Workspace provisions in. The contract requires it — a run that names
   * none is refused with 409 `workflow_run_project_required` — so the mutation refuses a caller
   * that has none instead of sending a body the contract does not describe. Optional here only
   * because the run form that picks a Project is not built yet.
   */
  projectId?: string
  /** Optional display name; defaults to the workflow's name. */
  name?: string
  /** The exact snapshot to execute, or omit to pin the latest published one. */
  snapshotId?: string
  /** Kickoff input delivered to the start node. */
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
 * `workflow_run_project_required` — and this editor states none yet: the run form that picks a
 * Project is not built, so a run started here is refused until that lands.
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
      if (input.projectId === undefined || input.projectId === '') {
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
