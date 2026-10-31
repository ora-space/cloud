import { createContext, useContext } from "react";
import { isAxiosError } from "axios";
import {
  activateWorkflow,
  analyzeWorkflow,
  createWorkflow,
  deleteWorkflow,
  deleteWorkflowVersion,
  getWorkflow,
  getWorkflowDraft,
  getWorkflowVersion,
  listWorkflowVersions,
  listWorkflows,
  publishWorkflow,
  rollbackWorkflow,
  updateWorkflow,
  updateWorkflowDraft,
  type Workflow as WireWorkflow,
  type WorkflowSnapshot as WireSnapshot,
  type WorkflowSummary as WireSummary,
  type WorkflowVersion as WireVersion,
} from "../api/generated";
import type {
  ActivateWorkflowRequest,
  ActivateWorkflowResponse,
  AnalyzeWorkflowRequest,
  AnalyzeWorkflowResponse,
  CreateWorkflowRequest,
  CreateWorkflowResponse,
  DeleteSnapshotRequest,
  DeleteSnapshotResponse,
  DeleteWorkflowRequest,
  DeleteWorkflowResponse,
  GetDraftRequest,
  GetDraftResponse,
  GetVersionRequest,
  GetVersionResponse,
  GetWorkflowRequest,
  GetWorkflowResponse,
  ListVersionsRequest,
  ListVersionsResponse,
  ListWorkflowsResponse,
  PublishWorkflowRequest,
  PublishWorkflowResponse,
  RollbackWorkflowRequest,
  RollbackWorkflowResponse,
  UpdateDraftRequest,
  UpdateDraftResponse,
  UpdateWorkflowRequest,
  UpdateWorkflowResponse,
  Workflow,
  WorkflowSnapshot,
  WorkflowSummary,
  WorkflowVersion,
} from "../contracts";
import { ContractError } from "../contracts";
import {
  forgetPendingDraft,
  pendingDrafts,
  rememberPendingDraft,
  type PendingDraft,
} from "./pending-drafts";
import {
  demoAgentModels,
  demoPlugins,
  demoRoles,
  demoSkills,
} from "./demo-catalog";

type Options = { signal?: AbortSignal };
type Operation<Request, Response> = (
  request: Request,
  options?: Options,
) => Promise<Response>;

/** Desktop's workflow definition operations the editor and the run menu call. */
export interface WorkflowContracts {
  activate: Operation<ActivateWorkflowRequest, ActivateWorkflowResponse>;
  analyze: Operation<AnalyzeWorkflowRequest, AnalyzeWorkflowResponse>;
  create: Operation<CreateWorkflowRequest, CreateWorkflowResponse>;
  delete: Operation<DeleteWorkflowRequest, DeleteWorkflowResponse>;
  deleteSnapshot: Operation<DeleteSnapshotRequest, DeleteSnapshotResponse>;
  get: Operation<GetWorkflowRequest, GetWorkflowResponse>;
  getDraft: Operation<GetDraftRequest, GetDraftResponse>;
  getVersion: Operation<GetVersionRequest, GetVersionResponse>;
  list: Operation<Record<string, never>, ListWorkflowsResponse>;
  listVersions: Operation<ListVersionsRequest, ListVersionsResponse>;
  publish: Operation<PublishWorkflowRequest, PublishWorkflowResponse>;
  rollback: Operation<RollbackWorkflowRequest, RollbackWorkflowResponse>;
  update: Operation<UpdateWorkflowRequest, UpdateWorkflowResponse>;
  updateDraft: Operation<UpdateDraftRequest, UpdateDraftResponse>;
}

/**
 * Desktop's workflow operations over v2-server's workflow routes in one tenant. The editor was
 * written against desktop's generated DTOs, so instants become `bigint` milliseconds here, and a
 * refused request becomes the `ContractError` the editor localizes through `errors.<code>`.
 */
export function createHttpWorkflowContracts(tenantId: string): WorkflowContracts {
  const send =
    <Request, Response>(operation: (request: Request, signal?: AbortSignal) => Promise<Response>) =>
    async (request: Request, options?: Options) => {
      try {
        return await operation(request, options?.signal);
      } catch (error) {
        throw contractError(error);
      }
    };
  const tid = tenantId;
  return {
    activate: send(async ({ workflowId, snapshotId }: ActivateWorkflowRequest, signal) => ({
      snapshot: snapshot(
        (await activateWorkflow(tid, workflowId, { snapshotId }, undefined, signal)).snapshot,
      ),
    })),
    analyze: send(({ graph }: AnalyzeWorkflowRequest, signal) =>
      analyzeWorkflow(tid, { graph }, undefined, signal),
    ),
    create: send(async ({ name, graph }: CreateWorkflowRequest, signal) => {
      const created = await createWorkflow(tid, { name, graph }, undefined, signal);
      return { workflow: workflow(created.workflow), draft: snapshot(created.draft) };
    }),
    delete: send(({ workflowId }: DeleteWorkflowRequest, signal) =>
      deleteWorkflow(tid, workflowId, undefined, signal),
    ),
    deleteSnapshot: send(({ workflowId, version }: DeleteSnapshotRequest, signal) =>
      deleteWorkflowVersion(tid, workflowId, pathSegment(version), undefined, signal),
    ),
    get: send(async ({ workflowId }: GetWorkflowRequest, signal) => {
      const detail = await getWorkflow(tid, workflowId, undefined, signal);
      return {
        workflow: workflow(detail.workflow),
        draft: snapshot(detail.draft),
        published: detail.published ? snapshot(detail.published) : null,
      };
    }),
    getDraft: send(async ({ workflowId }: GetDraftRequest, signal) => ({
      snapshot: snapshot((await getWorkflowDraft(tid, workflowId, undefined, signal)).snapshot),
    })),
    getVersion: send(async ({ workflowId, version }: GetVersionRequest, signal) => ({
      snapshot: snapshot(
        (await getWorkflowVersion(tid, workflowId, pathSegment(version), undefined, signal))
          .snapshot,
      ),
    })),
    list: send(async (_request: Record<string, never>, signal) => ({
      workflows: (await listWorkflows(tid, undefined, signal)).workflows.map(summary),
    })),
    listVersions: send(async ({ workflowId }: ListVersionsRequest, signal) => ({
      versions: (await listWorkflowVersions(tid, workflowId, undefined, signal)).versions.map(
        version,
      ),
    })),
    publish: send(async ({ workflowId, version }: PublishWorkflowRequest, signal) => ({
      snapshot: snapshot(
        (await publishWorkflow(tid, workflowId, { version }, undefined, signal)).snapshot,
      ),
    })),
    rollback: send(async ({ workflowId, snapshotId }: RollbackWorkflowRequest, signal) => ({
      snapshot: snapshot(
        (await rollbackWorkflow(tid, workflowId, { snapshotId }, undefined, signal)).snapshot,
      ),
    })),
    update: send(async ({ workflowId, name }: UpdateWorkflowRequest, signal) => ({
      workflow: workflow((await updateWorkflow(tid, workflowId, { name }, undefined, signal)).workflow),
    })),
    updateDraft: send(async ({ workflowId, graph }: UpdateDraftRequest, signal) => ({
      snapshot: snapshot(
        (await updateWorkflowDraft(tid, workflowId, { graph }, undefined, signal)).snapshot,
      ),
    })),
  };
}

/**
 * Axios's fetch adapter with `keepalive`: the one request the browser carries on after the page
 * unloads. The default XHR adapter cannot do that, so leaving the page asks for this one.
 */
export const keepaliveRequest = {
  adapter: "fetch",
  fetchOptions: { keepalive: true },
} as const;

/** The drafts of pages that went away before their last edits were saved, in one tenant. */
export interface DraftKeeper {
  /**
   * Keeps the draft a page or route is leaving with: remembered in the browser at once, so the
   * next visit writes it first (`replayPendingDrafts`), and sent with `keepalive`, so it usually
   * reaches the server even as the page unloads. Must stay synchronous.
   */
  keep(draft: PendingDraft): void;
  /** Forgets the workflow's draft once the server holds exactly `graph`. */
  saved(workflowId: string, graph: string): void;
  /** Drafts a replay found someone else's edits behind, which the user has to settle. */
  conflicts(): PendingDraft[];
  /** Drops the workflow's draft for good: the user settled it. */
  discard(workflowId: string): void;
}

/** The browser's pending drafts for `tenantId`, sent through the generated workflow client. */
export function browserDraftKeeper(tenantId: string): DraftKeeper {
  return {
    keep(draft) {
      rememberPendingDraft(tenantId, draft);
      try {
        void updateWorkflowDraft(tenantId, draft.workflowId, { graph: draft.graph }, keepaliveRequest)
          .then(() => {
            // A title still to apply keeps the draft: the next visit renames without rewriting.
            if (draft.name === null) forgetPendingDraft(tenantId, draft.workflowId, draft.graph);
          })
          .catch(() => undefined);
      } catch {
        // A body over the keepalive limit throws; the remembered draft still covers it.
      }
    },
    saved: (workflowId, graph) => forgetPendingDraft(tenantId, workflowId, graph),
    conflicts: () => pendingDrafts(tenantId).filter((draft) => draft.conflict),
    discard: (workflowId) => forgetPendingDraft(tenantId, workflowId),
  };
}

/**
 * The client every copied workflow query and mutation hook calls: the definition operations, and
 * the catalogs the Agent node's pickers read. v2-server reports no Roles, Skills or MCP servers
 * yet, so the catalogs are still the demo entries.
 */
export function createContractsClient(
  workflow: WorkflowContracts,
  /** Keeps the drafts of pages that go away; absent where nothing can outlive the page. */
  drafts?: DraftKeeper,
) {
  const answer =
    <Request, Response>(operation: (request: Request) => Response) =>
    async (request: Request, _options?: Options) =>
      operation(request);
  return {
    workflow,
    drafts,
    agent: {
      list: answer((_request: Record<string, never>) => ({ agents: demoRoles })),
    },
    skill: {
      list: answer((_request: Record<string, never>) => ({ skills: demoSkills })),
    },
    plugin: {
      listInstalled: answer((_request: Record<string, never>) => ({
        plugins: demoPlugins,
      })),
    },
    agentRuntime: {
      listModels: answer((request: { agentRef: string }) => ({
        models: demoAgentModels[request.agentRef] ?? [],
      })),
    },
  };
}

export type ContractsClient = ReturnType<typeof createContractsClient>;

export const ContractsClientContext = createContext<ContractsClient | null>(null);

export function useContractsClient(): ContractsClient {
  const client = useContext(ContractsClientContext);
  if (client === null) {
    throw new Error("useContractsClient must be used inside ContractsClientProvider");
  }
  return client;
}

/** The fault body's code and parameters, or the error unchanged when the server sent none. */
function contractError(error: unknown): unknown {
  if (!isAxiosError(error)) return error;
  const body = error.response?.data as { code?: unknown; params?: unknown } | undefined;
  if (typeof body?.code !== "string") return error;
  const params =
    body.params !== null && typeof body.params === "object"
      ? (body.params as Record<string, unknown>)
      : {};
  return new ContractError(body.code, params);
}

/**
 * The generated client splices path parameters in as they are, and a version name is free text
 * (`release?x`, `50%`, `v1 中文`), so it is encoded here; ids are UUIDs and need nothing.
 */
const pathSegment = (value: string): string => encodeURIComponent(value);

const instant = (ms: number): bigint => BigInt(ms);

function workflow(wire: WireWorkflow): Workflow {
  return { ...wire, createdAt: instant(wire.createdAt), updatedAt: instant(wire.updatedAt) };
}

function summary(wire: WireSummary): WorkflowSummary {
  return { ...wire, createdAt: instant(wire.createdAt), updatedAt: instant(wire.updatedAt) };
}

function snapshot(wire: WireSnapshot): WorkflowSnapshot {
  return {
    ...wire,
    createdAt: instant(wire.createdAt),
    updatedAt: wire.updatedAt === null ? null : instant(wire.updatedAt),
  };
}

function version(wire: WireVersion): WorkflowVersion {
  return { ...wire, createdAt: instant(wire.createdAt) };
}
