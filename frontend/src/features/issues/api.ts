import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { AXIOS_INSTANCE } from '@/lib/api-client'
import type {
  AssistSuggestion,
  CollaborationTargetSummary,
  CommentTargetInput,
  ContextRef,
  ContextRefRef,
  FormDescriptor,
  FormValues,
  Issue,
  IssueComment,
  IssueInteraction,
  IssuePriority,
  IssueRun,
  IssueStatusColumn,
  Label,
  Page,
  TenantMember,
  TimelineEntry,
} from './types'

/**
 * Cloud Issue data layer. Every hook talks to the real `/api/v1/tenants/:tid`
 * backend through the shared `AXIOS_INSTANCE` (same-origin, so the edge server's
 * session cookie carries auth). Nothing here touches the MSW mocks.
 */

/** Field names the PUT endpoint accepts; sent verbatim, with `version` for concurrency. */
export interface UpdateIssueInput {
  title?: string
  description?: string
  status?: string
  priority?: IssuePriority
  assigneeType?: 'user' | 'agent' | 'team'
  assigneeId?: string
  parentIssueId?: string
  projectRef?: string
  properties?: Record<string, unknown>
}

/** Fields the create endpoint accepts. */
export interface CreateIssueInput {
  title: string
  projectRef?: string
  description?: string
  status?: string
  priority?: IssuePriority
  assigneeType?: 'user' | 'agent' | 'team'
  assigneeId?: string
  parentIssueId?: string
}

/** Anchor-based move the board drag maps onto. */
export interface MoveIssueInput {
  status?: string
  beforeId?: string
  afterId?: string
}

/** Fresh idempotency key for every POST/DELETE; the backend dedupes replays by it. */
function idempotencyKey(): string {
  return crypto.randomUUID()
}

/** Unwraps a `Page<T>` list envelope into its `items` array. */
async function listPage<T>(url: string, params?: Record<string, string>): Promise<T[]> {
  const { data } = await AXIOS_INSTANCE.get<Page<T>>(url, { params })
  return data.items
}

/** Unwraps a `{ resource }` creation envelope. */
async function created<T>(url: string, body: unknown): Promise<T> {
  const { data } = await AXIOS_INSTANCE.post<{ resource: T }>(url, body, {
    headers: { 'Idempotency-Key': idempotencyKey() },
  })
  return data.resource
}

export function issuesKey(tid: string) {
  return ['issues', tid] as const
}

/** Lists the tenant board (optionally filtered by a free-text `q`). */
export function useIssues(tid: string, q?: string) {
  return useQuery({
    queryKey: [...issuesKey(tid), q ?? ''],
    queryFn: () => listPage<Issue>(`/api/v1/tenants/${tid}/issues`, q ? { q } : undefined),
    enabled: !!tid,
  })
}

export function useIssue(tid: string, id: string | undefined) {
  return useQuery({
    queryKey: ['issue', tid, id],
    queryFn: async () => {
      const { data } = await AXIOS_INSTANCE.get<Issue>(`/api/v1/tenants/${tid}/issues/${id}`)
      return data
    },
    enabled: !!tid && !!id,
  })
}

/** Loads the tenant's status catalog (columns the board renders, in position order). */
export function useIssueStatuses(tid: string) {
  return useQuery({
    queryKey: ['issue-statuses', tid],
    queryFn: () => listPage<IssueStatusColumn>(`/api/v1/tenants/${tid}/issue-statuses`),
    enabled: !!tid,
  })
}

/** Loads tenant members for resolving user assignee display names. */
export function useMembers(tid: string) {
  return useQuery({
    queryKey: ['members', tid],
    queryFn: () => listPage<TenantMember>(`/api/v1/tenants/${tid}/members`),
    enabled: !!tid,
  })
}

export function useCreateIssue(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: CreateIssueInput) => created<Issue>(`/api/v1/tenants/${tid}/issues`, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: issuesKey(tid) })
    },
  })
}

export function useUpdateIssue(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      id,
      version,
      patch,
    }: {
      id: string
      version: number
      patch: UpdateIssueInput
    }) => {
      const { data } = await AXIOS_INSTANCE.put<Issue>(`/api/v1/tenants/${tid}/issues/${id}`, {
        ...patch,
        version,
      })
      return data
    },
    onSuccess: (data) => {
      queryClient.setQueryData(['issue', tid, data.id], data)
      void queryClient.invalidateQueries({ queryKey: issuesKey(tid) })
    },
  })
}

export function useMoveIssue(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({
      id,
      version,
      move,
    }: {
      id: string
      version: number
      move: MoveIssueInput
    }) => {
      const { data } = await AXIOS_INSTANCE.post<Issue>(
        `/api/v1/tenants/${tid}/issues/${id}/move`,
        { ...move, version },
        { headers: { 'Idempotency-Key': idempotencyKey() } },
      )
      return data
    },
    onSuccess: (data) => {
      queryClient.setQueryData(['issue', tid, data.id], data)
      void queryClient.invalidateQueries({ queryKey: issuesKey(tid) })
    },
  })
}

export function useDeleteIssue(tid: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async ({ id, version }: { id: string; version: number }) => {
      await AXIOS_INSTANCE.delete(`/api/v1/tenants/${tid}/issues/${id}`, {
        data: { version },
        headers: { 'Idempotency-Key': idempotencyKey() },
      })
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: issuesKey(tid) })
    },
  })
}

export function useComments(tid: string, issueId: string) {
  return useQuery({
    queryKey: ['issue-comments', tid, issueId],
    queryFn: () => listPage<IssueComment>(`/api/v1/tenants/${tid}/issues/${issueId}/comments`),
    enabled: !!tid && !!issueId,
  })
}

export function useCreateComment(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: { body: string; parentId?: string; targets?: CommentTargetInput[] }) =>
      created<IssueComment>(`/api/v1/tenants/${tid}/issues/${issueId}/comments`, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['issue-comments', tid, issueId] })
      void queryClient.invalidateQueries({ queryKey: ['issue-timeline', tid, issueId] })
      void queryClient.invalidateQueries({ queryKey: ['issue-interactions', tid, issueId] })
      void queryClient.invalidateQueries({ queryKey: ['issue-runs', tid, issueId] })
    },
  })
}

/** Polls an issue's runs while one is unsettled; run status and delivery publish no space event. */
export const RUNS_SETTLING_POLL_MS = 5000

/**
 * The issue's runs. While an agent run is still queued, dispatched or running the list refreshes
 * itself: a session that ended keeps saving its Revision and releasing its Workspace, and Cloud
 * publishes no event when the run finally settles.
 */
export function useRuns(tid: string, issueId: string) {
  return useQuery({
    queryKey: ['issue-runs', tid, issueId],
    queryFn: () => listPage<IssueRun>(`/api/v1/tenants/${tid}/issues/${issueId}/runs`),
    enabled: !!tid && !!issueId,
    refetchInterval: (query) =>
      (query.state.data ?? []).some(
        (run) =>
          run.executorType === 'agent' && ['queued', 'dispatched', 'running'].includes(run.status),
      )
        ? RUNS_SETTLING_POLL_MS
        : false,
  })
}

export function useCreateRun(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: {
      executorType: 'agent' | 'team' | 'workflow'
      executorId: string
      input?: Record<string, unknown>
    }) => created<IssueRun>(`/api/v1/tenants/${tid}/issues/${issueId}/runs`, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['issue-runs', tid, issueId] })
    },
  })
}

/** Lists collaboration targets for the @ picker (members + directory fixtures), optional `q`. */
export function useCollaborationTargets(tid: string, q?: string) {
  return useQuery({
    queryKey: ['collaboration-targets', tid, q ?? ''],
    queryFn: () =>
      listPage<CollaborationTargetSummary>(
        `/api/v1/tenants/${tid}/collaboration/targets`,
        q ? { q } : undefined,
      ),
    enabled: !!tid,
  })
}

/** The issue Timeline: comments and activities merged into one seq-ordered stream. */
export function useTimeline(tid: string, issueId: string) {
  return useQuery({
    queryKey: ['issue-timeline', tid, issueId],
    queryFn: () => listPage<TimelineEntry>(`/api/v1/tenants/${tid}/issues/${issueId}/timeline`),
    enabled: !!tid && !!issueId,
  })
}

/** The persisted @ interaction spine for an issue (one row per selected target). */
export function useInteractions(tid: string, issueId: string) {
  return useQuery({
    queryKey: ['issue-interactions', tid, issueId],
    queryFn: () =>
      listPage<IssueInteraction>(`/api/v1/tenants/${tid}/issues/${issueId}/interactions`),
    enabled: !!tid && !!issueId,
  })
}

/**
 * Loads the Issues-facing FormDescriptor for a `formRef` (§38.17). The descriptor is a rendering
 * projection — it is fetched per selection, never embedded in the picker payload.
 *
 * `issueId` is the issue the form is being configured for. It is what makes the descriptor carry the
 * platform fields (repository, prompt) and what prefills them from the issue's project repository and
 * the workflow's Start prompt. It is optional, and omitting it is not a degraded mode: the descriptor
 * is then exactly the one this route served before the platform fields existed.
 *
 * It is part of the query key because it changes the response: a descriptor cached for one issue must
 * never seed another issue's form with the wrong defaults.
 */
export function useFormDescriptor(tid: string, formRef: string | undefined, issueId?: string) {
  return useQuery({
    queryKey: ['collaboration-form', tid, formRef ?? '', issueId ?? ''],
    queryFn: async () => {
      const { data } = await AXIOS_INSTANCE.get<FormDescriptor>(
        `/api/v1/tenants/${tid}/collaboration/forms/${encodeURIComponent(formRef ?? '')}`,
        issueId ? { params: { issueId } } : undefined,
      )
      return data
    },
    enabled: !!tid && !!formRef,
  })
}

/**
 * Asks for AI suggestions for a Workflow form that has **not** been confirmed yet. The form is a draft
 * until the user confirms it, so this route is deliberately stateless: no interaction needs to exist,
 * and the call writes nothing (no comment, no run, no context ref).
 */
export function useAssistWorkflow(tid: string, issueId: string) {
  return useMutation({
    mutationFn: async (input: { targetId: string; values: FormValues }) => {
      const { data } = await AXIOS_INSTANCE.post<AssistSuggestion>(
        `/api/v1/tenants/${tid}/issues/${issueId}/collaboration/assist`,
        input,
        { headers: { 'Idempotency-Key': idempotencyKey() } },
      )
      return data
    },
  })
}

/**
 * Confirms a Workflow interaction: the single boundary that turns configuration into an execution
 * intent. It validates the values, persists the interaction's input and creates the initial run.
 */
export function useConfirmWorkflow(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (input: {
      interactionId: string
      values: FormValues
      contextRefs?: ContextRefRef[]
    }) => {
      const { interactionId, ...body } = input
      return created<IssueRun>(
        `/api/v1/tenants/${tid}/issues/${issueId}/interactions/${interactionId}/confirm`,
        body,
      )
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['issue-interactions', tid, issueId] })
      void queryClient.invalidateQueries({ queryKey: ['issue-runs', tid, issueId] })
      void queryClient.invalidateQueries({ queryKey: ['issue-timeline', tid, issueId] })
    },
  })
}

/**
 * Records the comment that carries a workflow target and returns the interaction it created. It runs at
 * confirm time, never at selection time: selecting a workflow must leave no trace in the Timeline until
 * the user actually confirms the configuration.
 */
export async function openWorkflowInteraction(
  tid: string,
  issueId: string,
  targetId: string,
  body: string,
): Promise<IssueInteraction> {
  await AXIOS_INSTANCE.post(
    `/api/v1/tenants/${tid}/issues/${issueId}/comments`,
    { body, targets: [{ type: 'workflow', id: targetId }] },
    { headers: { 'Idempotency-Key': idempotencyKey() } },
  )
  const { data } = await AXIOS_INSTANCE.get<Page<IssueInteraction>>(
    `/api/v1/tenants/${tid}/issues/${issueId}/interactions`,
  )
  const mine = data.items.findLast(
    (item) => item.mode === 'form' && item.runId === null && item.targetId === targetId,
  )
  if (!mine) throw new Error('workflow interaction was not created')
  return mine
}

export function useContextRefs(tid: string, issueId: string) {
  return useQuery({
    queryKey: ['issue-context-refs', tid, issueId],
    queryFn: () => listPage<ContextRef>(`/api/v1/tenants/${tid}/issues/${issueId}/context-refs`),
    enabled: !!tid && !!issueId,
  })
}

export function useCreateContextRef(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (input: { refType: ContextRef['refType']; refId: string }) =>
      created<ContextRef>(`/api/v1/tenants/${tid}/issues/${issueId}/context-refs`, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['issue-context-refs', tid, issueId] })
    },
  })
}

export function useDeleteContextRef(tid: string, issueId: string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (id: string) =>
      AXIOS_INSTANCE.delete(`/api/v1/tenants/${tid}/issues/${issueId}/context-refs/${id}`, {
        headers: { 'Idempotency-Key': idempotencyKey() },
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['issue-context-refs', tid, issueId] })
    },
  })
}

/** Re-export the label type used by issue cards; keeps components on one import surface. */
export type { Label }
