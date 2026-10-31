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
  GetWorkflowSnapshotRequest,
  GetWorkflowSnapshotResponse,
  ListVersionsRequest,
  ListVersionsResponse,
  ListWorkflowsResponse,
  PublishWorkflowRequest,
  PublishWorkflowResponse,
  RollbackWorkflowRequest,
  RollbackWorkflowResponse,
  UnrecognizedWorkflowNode,
  UpdateDraftRequest,
  UpdateDraftResponse,
  UpdateWorkflowRequest,
  UpdateWorkflowResponse,
  Workflow,
  WorkflowSnapshot,
} from "../contracts";
import { ContractError } from "../contracts";

/**
 * Desktop's workflow definition backend (`crates/application/src/workflow`) kept in one browser
 * entry, as the workflow page used it before v2-server stored workflows. The page now only reads
 * that entry to offer importing what a browser still holds (`legacy-library.ts`); tests also use it
 * as the fake server. Each operation keeps the Rust handler's rules — trimmed unique names, one
 * mutable `draft` snapshot, immutable published snapshots, the published pointer, soft deletion and
 * its refusals. Timestamps are milliseconds as `bigint`, like the generated DTOs.
 */

/** Desktop's initial graph for a workflow created without one. */
const DEFAULT_GRAPH =
  '{"nodes":[{"id":"start","type":"workflow","deletable":false,"position":{"x":120,"y":260},"data":{"kind":"start","title":"开始","description":"接收工作流输入"}}],"edges":[]}';
const DRAFT_VERSION = "draft";
const MAX_VERSION_BYTES = 128;
const MAX_AUTOMATIC_VERSION_COLLISION_RETRIES = 8;
const LOCAL_NAMESPACE = "local";
const STORAGE_KEY = "ora.v2.workflows";

/** Node kinds desktop's engine registers (`NodeType`); any other kind is unrecognized. */
const RECOGNIZED_KINDS = new Set([
  "start",
  "agent",
  "prompt",
  "condition",
  "aggregator",
  "tool",
  "output",
  "loop",
  "loopExit",
  "iteration",
]);

interface StoredWorkflow extends Workflow {
  isDeleted: boolean;
}

interface StoredSnapshot extends WorkflowSnapshot {
  isDeleted: boolean;
}

interface StoredState {
  workflows: StoredWorkflow[];
  snapshots: StoredSnapshot[];
}

export interface WorkflowBackendStorage {
  load(): string | null;
  save(serialized: string): void;
}

/** localStorage, tolerating a browser that refuses storage: edits then last for the page only. */
export const browserWorkflowStorage: WorkflowBackendStorage = {
  load() {
    try {
      return window.localStorage.getItem(STORAGE_KEY);
    } catch {
      return null;
    }
  },
  save(serialized) {
    try {
      window.localStorage.setItem(STORAGE_KEY, serialized);
    } catch {
      // Storage is best effort; the in-memory state stays authoritative for this page.
    }
  },
};

export interface LocalWorkflowBackendOptions {
  storage?: WorkflowBackendStorage;
  now?: () => number;
  newId?: () => string;
}

export class LocalWorkflowBackend {
  private readonly storage: WorkflowBackendStorage;
  private readonly now: () => number;
  private readonly newId: () => string;
  private state: StoredState;

  constructor(options: LocalWorkflowBackendOptions = {}) {
    this.storage = options.storage ?? browserWorkflowStorage;
    this.now = options.now ?? Date.now;
    this.newId = options.newId ?? (() => crypto.randomUUID());
    this.state = decodeState(this.storage.load());
  }

  /**
   * Newest created first, as desktop's library query orders them. Workflows created in the same
   * millisecond keep their creation order (later first), where desktop falls back to the id.
   */
  list(): ListWorkflowsResponse {
    const workflows = this.state.workflows
      .map((workflow, index) => ({ workflow, index }))
      .filter(({ workflow }) => !workflow.isDeleted)
      .sort((a, b) =>
        a.workflow.createdAt === b.workflow.createdAt
          ? b.index - a.index
          : Number(b.workflow.createdAt - a.workflow.createdAt),
      )
      .map(({ workflow }) => workflow)
      .map((workflow) => ({
        id: workflow.id,
        namespace: workflow.namespace,
        name: workflow.name,
        publishedVersion:
          workflow.publishedSnapshotId === null
            ? null
            : (this.findSnapshot(workflow.publishedSnapshotId)?.version ??
              null),
        createdAt: workflow.createdAt,
        updatedAt: workflow.updatedAt,
      }));
    return { workflows };
  }

  create(request: CreateWorkflowRequest): CreateWorkflowResponse {
    const now = BigInt(this.now());
    const name = this.acceptName(request.name, null);
    const workflow: StoredWorkflow = {
      id: this.newId(),
      namespace: LOCAL_NAMESPACE,
      name,
      publishedSnapshotId: null,
      createdAt: now,
      updatedAt: now,
      isDeleted: false,
    };
    const draft: StoredSnapshot = {
      id: this.newId(),
      workflowId: workflow.id,
      version: DRAFT_VERSION,
      graph: request.graph ?? DEFAULT_GRAPH,
      createdAt: now,
      updatedAt: now,
      isDeleted: false,
    };
    this.state.workflows.push(workflow);
    this.state.snapshots.push(draft);
    this.persist();
    return { workflow: publicWorkflow(workflow), draft: publicSnapshot(draft) };
  }

  get(request: GetWorkflowRequest): GetWorkflowResponse {
    const workflow = this.requireWorkflow(request.workflowId);
    const published =
      workflow.publishedSnapshotId === null
        ? undefined
        : this.findSnapshot(workflow.publishedSnapshotId);
    return {
      workflow: publicWorkflow(workflow),
      draft: publicSnapshot(this.requireDraft(workflow.id)),
      published: published === undefined ? null : publicSnapshot(published),
    };
  }

  getDraft(request: GetDraftRequest): GetDraftResponse {
    this.requireWorkflow(request.workflowId);
    return { snapshot: publicSnapshot(this.requireDraft(request.workflowId)) };
  }

  getVersion(request: GetVersionRequest): GetVersionResponse {
    this.requireWorkflow(request.workflowId);
    return {
      snapshot: publicSnapshot(
        this.requireVersion(request.workflowId, request.version),
      ),
    };
  }

  getSnapshot(request: GetWorkflowSnapshotRequest): GetWorkflowSnapshotResponse {
    const snapshot = this.findSnapshot(request.snapshotId);
    if (snapshot === undefined) {
      throw new ContractError("workflow_snapshot_not_found", {
        snapshotId: request.snapshotId,
      });
    }
    return { snapshot: publicSnapshot(snapshot) };
  }

  /** Published versions only, newest first; the draft never appears in history. */
  listVersions(request: ListVersionsRequest): ListVersionsResponse {
    this.requireWorkflow(request.workflowId);
    const versions = this.state.snapshots
      .filter(
        (snapshot) =>
          snapshot.workflowId === request.workflowId &&
          snapshot.version !== DRAFT_VERSION &&
          !snapshot.isDeleted,
      )
      .sort((a, b) => Number(b.createdAt - a.createdAt))
      .map((snapshot) => ({
        id: snapshot.id,
        version: snapshot.version,
        createdAt: snapshot.createdAt,
      }));
    return { versions };
  }

  update(request: UpdateWorkflowRequest): UpdateWorkflowResponse {
    const workflow = this.requireWorkflow(request.workflowId);
    workflow.name = this.acceptName(request.name, workflow.id);
    workflow.updatedAt = BigInt(this.now());
    this.persist();
    return { workflow: publicWorkflow(workflow) };
  }

  /** Draft saves stay permissive so an unfinished canvas can be kept. */
  updateDraft(request: UpdateDraftRequest): UpdateDraftResponse {
    const workflow = this.requireWorkflow(request.workflowId);
    const draft = this.requireDraft(workflow.id);
    const now = BigInt(this.now());
    draft.graph = request.graph;
    draft.updatedAt = now;
    workflow.updatedAt = now;
    this.persist();
    return { snapshot: publicSnapshot(draft) };
  }

  /**
   * Freezes the draft as a new version and makes it the published one. Like desktop, publish is
   * where the document must project onto an executable graph; spare nodes do not block it.
   */
  publish(request: PublishWorkflowRequest): PublishWorkflowResponse {
    const now = this.now();
    let version: string;
    const automatic = request.version === null;
    if (request.version === null) {
      version = `v${now}`;
    } else if (request.version === DRAFT_VERSION) {
      throw new ContractError("workflow_version_reserved");
    } else if (!isValidUserVersion(request.version)) {
      throw new ContractError("workflow_version_invalid");
    } else {
      version = request.version;
    }
    const workflow = this.requireWorkflow(request.workflowId);
    const draft = this.requireDraft(workflow.id);
    projectAuthoringDocument(draft.graph, { requireExecutable: true });
    for (let retry = 0; this.findVersion(workflow.id, version); retry += 1) {
      if (!automatic || retry >= MAX_AUTOMATIC_VERSION_COLLISION_RETRIES) {
        throw new ContractError("workflow_version_already_exists", {
          workflowId: workflow.id,
          version,
        });
      }
      version = `v${now}-${retry + 1}`;
    }
    const snapshot: StoredSnapshot = {
      id: this.newId(),
      workflowId: workflow.id,
      version,
      graph: draft.graph,
      createdAt: BigInt(now),
      updatedAt: null,
      isDeleted: false,
    };
    this.state.snapshots.push(snapshot);
    workflow.publishedSnapshotId = snapshot.id;
    this.persist();
    return { snapshot: publicSnapshot(snapshot) };
  }

  /** Copies a published version back into the draft without moving the published pointer. */
  rollback(request: RollbackWorkflowRequest): RollbackWorkflowResponse {
    const draft = this.copyIntoDraft(
      request.workflowId,
      request.snapshotId,
      "workflow_cannot_rollback_to_draft",
    );
    this.persist();
    return { snapshot: publicSnapshot(draft) };
  }

  /** Points the published version at another snapshot and syncs its graph into the draft. */
  activate(request: ActivateWorkflowRequest): ActivateWorkflowResponse {
    const draft = this.copyIntoDraft(
      request.workflowId,
      request.snapshotId,
      "workflow_cannot_activate_draft",
    );
    this.requireWorkflow(request.workflowId).publishedSnapshotId =
      request.snapshotId;
    this.persist();
    return { snapshot: publicSnapshot(draft) };
  }

  /** Soft-deletes the workflow together with every snapshot it owns. */
  delete(request: DeleteWorkflowRequest): DeleteWorkflowResponse {
    const workflow = this.requireWorkflow(request.workflowId);
    workflow.isDeleted = true;
    for (const snapshot of this.state.snapshots) {
      if (snapshot.workflowId === workflow.id) snapshot.isDeleted = true;
    }
    this.persist();
    return { workflowId: workflow.id };
  }

  deleteSnapshot(request: DeleteSnapshotRequest): DeleteSnapshotResponse {
    const workflow = this.requireWorkflow(request.workflowId);
    if (request.version === DRAFT_VERSION) {
      throw new ContractError("workflow_cannot_delete_draft");
    }
    const snapshot = this.requireVersion(workflow.id, request.version);
    if (snapshot.id === workflow.publishedSnapshotId) {
      throw new ContractError("workflow_cannot_delete_active_version");
    }
    snapshot.isDeleted = true;
    this.persist();
    return { snapshotId: snapshot.id, version: snapshot.version };
  }

  analyze(request: AnalyzeWorkflowRequest): AnalyzeWorkflowResponse {
    const projection = projectAuthoringDocument(request.graph, {
      requireExecutable: false,
    });
    const unrecognized = new Set(
      projection.unrecognizedNodes.map((node) => node.nodeId),
    );
    return {
      unusedNodeIds: projection.unusedNodeIds.filter(
        (nodeId) => !unrecognized.has(nodeId),
      ),
      unrecognizedNodes: projection.unrecognizedNodes,
    };
  }

  private copyIntoDraft(
    workflowId: string,
    snapshotId: string,
    draftRefusal: string,
  ): StoredSnapshot {
    const workflow = this.requireWorkflow(workflowId);
    const source = this.findSnapshot(snapshotId);
    if (source === undefined || source.workflowId !== workflow.id) {
      throw new ContractError("workflow_snapshot_not_found", {
        workflowId,
        snapshotId,
      });
    }
    if (source.version === DRAFT_VERSION) {
      throw new ContractError(draftRefusal);
    }
    const draft = this.requireDraft(workflow.id);
    const now = BigInt(this.now());
    draft.graph = source.graph;
    draft.updatedAt = now;
    workflow.updatedAt = now;
    return draft;
  }

  /** Trims the name and refuses a blank one or one another live workflow already uses. */
  private acceptName(raw: string, ownId: string | null): string {
    const name = raw.trim();
    if (name === "") {
      throw new ContractError("workflow_name_blank");
    }
    const other = this.state.workflows.find(
      (workflow) =>
        !workflow.isDeleted && workflow.name === name && workflow.id !== ownId,
    );
    if (other !== undefined) {
      throw new ContractError("workflow_name_conflict", { name });
    }
    return name;
  }

  private requireWorkflow(workflowId: string): StoredWorkflow {
    const workflow = this.state.workflows.find(
      (candidate) => candidate.id === workflowId && !candidate.isDeleted,
    );
    if (workflow === undefined) {
      throw new ContractError("workflow_not_found", { workflowId });
    }
    return workflow;
  }

  private requireDraft(workflowId: string): StoredSnapshot {
    return this.requireVersion(workflowId, DRAFT_VERSION);
  }

  private requireVersion(workflowId: string, version: string): StoredSnapshot {
    const snapshot = this.findVersion(workflowId, version);
    if (snapshot === undefined) {
      throw new ContractError("workflow_snapshot_not_found", {
        workflowId,
        version,
      });
    }
    return snapshot;
  }

  private findVersion(
    workflowId: string,
    version: string,
  ): StoredSnapshot | undefined {
    return this.state.snapshots.find(
      (snapshot) =>
        snapshot.workflowId === workflowId &&
        snapshot.version === version &&
        !snapshot.isDeleted,
    );
  }

  private findSnapshot(snapshotId: string): StoredSnapshot | undefined {
    return this.state.snapshots.find(
      (snapshot) => snapshot.id === snapshotId && !snapshot.isDeleted,
    );
  }

  private persist(): void {
    this.storage.save(encodeState(this.state));
  }
}

/** Desktop's `is_valid_user_version`: one addressable URL path segment of at most 128 bytes. */
export function isValidUserVersion(version: string): boolean {
  return (
    version.trim() !== "" &&
    new TextEncoder().encode(version).length <= MAX_VERSION_BYTES &&
    version !== "." &&
    version !== ".." &&
    // eslint-disable-next-line no-control-regex
    !/[\u0000-\u001f\u007f-\u009f/\\]/.test(version)
  );
}

interface AuthoringProjection {
  unusedNodeIds: string[];
  unrecognizedNodes: UnrecognizedWorkflowNode[];
}

interface ProjectedNode {
  id: string;
  kind: string;
  owner: string | null;
}

/**
 * Ports desktop's `execution_document::project`: validate node identity and container ownership,
 * then walk edges from the root Start (entering each active Loop through its own Start). Nodes the
 * walk never reaches are spare. Publishing additionally refuses an entry-reachable node whose kind
 * the engine does not know; desktop's per-node configuration checks stay with the engine.
 */
export function projectAuthoringDocument(
  source: string,
  options: { requireExecutable: boolean },
): AuthoringProjection {
  let document: unknown;
  try {
    document = JSON.parse(source);
  } catch {
    throw graphParseError("invalid JSON");
  }
  if (!isRecord(document)) throw graphParseError("invalid JSON");
  const rawNodes = document.nodes;
  const rawEdges = document.edges;
  if (!Array.isArray(rawNodes)) throw graphParseError("missing nodes");
  if (!Array.isArray(rawEdges)) throw graphParseError("missing edges");

  const nodes = new Map<string, ProjectedNode>();
  const containerOwned = new Set<string>();
  const starts = new Map<string | null, string>();
  const unrecognizedNodes: UnrecognizedWorkflowNode[] = [];
  for (const raw of rawNodes) {
    const id = requiredString(isRecord(raw) ? raw.id : undefined, "missing id");
    const data = isRecord(raw) && isRecord(raw.data) ? raw.data : {};
    const kind = requiredString(data.kind, `node ${id} has no node type`);
    if (!RECOGNIZED_KINDS.has(kind)) {
      unrecognizedNodes.push({ nodeId: id, kind });
    }
    const container =
      "containerId" in data
        ? requiredString(
            data.containerId,
            "containerId must be a non-empty string",
          )
        : null;
    const parentValue = isRecord(raw) ? raw.parentId : undefined;
    const parent =
      parentValue === undefined || parentValue === null
        ? null
        : requiredString(parentValue, "parentId must be a non-empty string");
    if (container !== null && parent !== null && container !== parent) {
      throw graphParseError("parentId disagrees with data.containerId");
    }
    if (nodes.has(id)) throw graphParseError(`duplicate node id ${id}`);
    const owner = container ?? parent;
    nodes.set(id, { id, kind, owner });
    if (container !== null) containerOwned.add(id);
    if (kind === "start") {
      if (starts.has(owner)) throw graphParseError("multiple start nodes");
      starts.set(owner, id);
    }
  }
  for (const node of nodes.values()) {
    if (node.owner === null) continue;
    const parent = nodes.get(node.owner);
    if (parent === undefined) {
      throw graphParseError(`unknown container owner ${node.owner}`);
    }
    const expected = containerOwned.has(node.id) ? "loop" : "iteration";
    if (
      parent.kind !== expected ||
      parent.owner !== null ||
      node.kind === "loop" ||
      node.kind === "iteration"
    ) {
      throw graphParseError(
        `node ${node.id}: invalid or nested container ownership`,
      );
    }
  }
  if (
    [...nodes.values()].some((node) => node.kind === "loop") &&
    document.schemaVersion !== 2
  ) {
    throw graphParseError("container graphs require schemaVersion 2");
  }
  const outgoing = new Map<string, string[]>();
  for (const edge of rawEdges) {
    const record = isRecord(edge) ? edge : {};
    const sourceId = requiredString(record.source, "edge has no source");
    const targetId = requiredString(record.target, "edge has no target");
    const from = nodes.get(sourceId);
    const to = nodes.get(targetId);
    if (from === undefined) throw graphParseError(`dangling edge ${sourceId}`);
    if (to === undefined) throw graphParseError(`dangling edge ${targetId}`);
    // Iteration entry edges are the only legal edges across containment boundaries.
    if (
      from.owner !== to.owner &&
      !(from.kind === "iteration" && to.owner === sourceId)
    ) {
      throw graphParseError(`node ${sourceId}: edge crosses a container boundary`);
    }
    outgoing.set(sourceId, [...(outgoing.get(sourceId) ?? []), targetId]);
  }
  const rootStart = starts.get(null);
  if (rootStart === undefined) {
    if (options.requireExecutable) throw graphParseError("missing start node");
    return { unusedNodeIds: [], unrecognizedNodes };
  }
  const active = new Set<string>();
  const frontier = [rootStart];
  while (frontier.length > 0) {
    const id = frontier.pop()!;
    if (active.has(id)) continue;
    active.add(id);
    if (nodes.get(id)?.kind === "loop") {
      const entry = starts.get(id);
      if (entry === undefined) {
        throw graphParseError("every active Loop requires one Start node");
      }
      frontier.push(entry);
    }
    frontier.push(...(outgoing.get(id) ?? []));
  }
  if (options.requireExecutable) {
    const blocked = unrecognizedNodes.find((node) => active.has(node.nodeId));
    if (blocked !== undefined) {
      throw graphParseError(`unknown node type: ${blocked.kind}`);
    }
  }
  return {
    unusedNodeIds: [...nodes.keys()].filter((id) => !active.has(id)).sort(),
    unrecognizedNodes,
  };
}

function graphParseError(reason: string): ContractError {
  return new ContractError("workflow_run_graph_parse", { reason });
}

function requiredString(value: unknown, reason: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw graphParseError(reason);
  }
  return value;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function publicWorkflow(workflow: StoredWorkflow): Workflow {
  const { isDeleted: _deleted, ...rest } = workflow;
  return { ...rest };
}

function publicSnapshot(snapshot: StoredSnapshot): WorkflowSnapshot {
  const { isDeleted: _deleted, ...rest } = snapshot;
  return { ...rest };
}

const BIGINT_FIELDS = new Set(["createdAt", "updatedAt"]);

/** JSON has no bigint: timestamps are written as decimal strings and read back as bigint. */
function encodeState(state: StoredState): string {
  return JSON.stringify(state, (_key, value: unknown) =>
    typeof value === "bigint" ? value.toString() : value,
  );
}

function decodeState(serialized: string | null): StoredState {
  if (serialized === null) return { workflows: [], snapshots: [] };
  try {
    const parsed = JSON.parse(serialized, (key, value: unknown) =>
      BIGINT_FIELDS.has(key) && typeof value === "string"
        ? BigInt(value)
        : value,
    ) as Partial<StoredState>;
    return {
      workflows: Array.isArray(parsed.workflows) ? parsed.workflows : [],
      snapshots: Array.isArray(parsed.snapshots) ? parsed.snapshots : [],
    };
  } catch {
    return { workflows: [], snapshots: [] };
  }
}
