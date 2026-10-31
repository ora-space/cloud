import { ContractError } from "../contracts";
import type { WorkflowContracts } from "./contracts-client";

const STORAGE_KEY = "ora.v2.workflows.pending-drafts";

/** A draft the page was still writing when it went away, kept until the server has it. */
export interface PendingDraft {
  workflowId: string;
  graph: string;
  /** The edited title still to apply, or null when the title was not changed. */
  name: string | null;
  /**
   * The server draft the edits started from: its `updatedAt` in milliseconds, as last read or
   * saved by the page. A server draft that moved past it holds someone else's edits.
   */
  baseUpdatedAt: number | null;
  /**
   * Fingerprints of the graphs this page sent after that base without hearing back. Finding one
   * of them on the server means only the page's own writes moved it, so it is not a conflict.
   */
  sent: string[];
  /** Set once a replay found the server draft changed by someone else; the user decides. */
  conflict?: true;
}

/** Where pending drafts live between visits: synchronous, so a closing page can still write. */
export interface PendingDraftStorage {
  load(): string | null;
  save(serialized: string | null): void;
}

export const browserPendingDraftStorage: PendingDraftStorage = {
  load() {
    try {
      return window.localStorage.getItem(STORAGE_KEY);
    } catch {
      return null;
    }
  },
  save(serialized) {
    try {
      if (serialized === null) window.localStorage.removeItem(STORAGE_KEY);
      else window.localStorage.setItem(STORAGE_KEY, serialized);
    } catch {
      // Without storage only the keepalive request carries the last edits.
    }
  },
};

type Stored = Record<string, Record<string, PendingDraft>>;

function read(storage: PendingDraftStorage): Stored {
  try {
    const parsed = JSON.parse(storage.load() ?? "{}") as unknown;
    return parsed !== null && typeof parsed === "object" ? (parsed as Stored) : {};
  } catch {
    return {};
  }
}

function write(storage: PendingDraftStorage, stored: Stored): void {
  const tenants = Object.fromEntries(
    Object.entries(stored).filter(([, drafts]) => Object.keys(drafts).length > 0),
  );
  storage.save(Object.keys(tenants).length === 0 ? null : JSON.stringify(tenants));
}

/**
 * A short fingerprint of a graph, so a pending record can name the graphs it sent without
 * storing each one (cyrb53: 53 bits, plenty to tell one page's few writes apart).
 */
export function graphFingerprint(graph: string): string {
  let h1 = 0xdeadbeef;
  let h2 = 0x41c6ce57;
  for (let i = 0; i < graph.length; i += 1) {
    const ch = graph.charCodeAt(i);
    h1 = Math.imul(h1 ^ ch, 2654435761);
    h2 = Math.imul(h2 ^ ch, 1597334677);
  }
  h1 = Math.imul(h1 ^ (h1 >>> 16), 2246822507) ^ Math.imul(h2 ^ (h2 >>> 13), 3266489909);
  h2 = Math.imul(h2 ^ (h2 >>> 16), 2246822507) ^ Math.imul(h1 ^ (h1 >>> 13), 3266489909);
  return (4294967296 * (2097151 & h2) + (h1 >>> 0)).toString(36);
}

/**
 * Remembers `draft` as the tenant's latest unsaved edit of that workflow. A draft already waiting
 * for the user's decision is kept: the page that edited on regardless does not replace it.
 */
export function rememberPendingDraft(
  tenantId: string,
  draft: PendingDraft,
  storage: PendingDraftStorage = browserPendingDraftStorage,
): void {
  const stored = read(storage);
  if (stored[tenantId]?.[draft.workflowId]?.conflict) return;
  stored[tenantId] = { ...stored[tenantId], [draft.workflowId]: draft };
  write(storage, stored);
}

/**
 * Forgets the workflow's pending draft once the server has it: only when `graph` is the graph
 * remembered, so an older write landing late does not drop newer edits. Without `graph` the
 * draft is forgotten unconditionally, as when the user discards it.
 */
export function forgetPendingDraft(
  tenantId: string,
  workflowId: string,
  graph?: string,
  storage: PendingDraftStorage = browserPendingDraftStorage,
): void {
  const stored = read(storage);
  const draft = stored[tenantId]?.[workflowId];
  if (draft === undefined || (graph !== undefined && draft.graph !== graph)) return;
  const { [workflowId]: _forgotten, ...rest } = stored[tenantId];
  stored[tenantId] = rest;
  write(storage, stored);
}

export function pendingDrafts(
  tenantId: string,
  storage: PendingDraftStorage = browserPendingDraftStorage,
): PendingDraft[] {
  return Object.values(read(storage)[tenantId] ?? {});
}

function markConflict(tenantId: string, draft: PendingDraft, storage: PendingDraftStorage): void {
  const stored = read(storage);
  if (stored[tenantId]?.[draft.workflowId]?.graph !== draft.graph) return;
  stored[tenantId] = { ...stored[tenantId], [draft.workflowId]: { ...draft, conflict: true } };
  write(storage, stored);
}

/** A kept draft whose workflow someone else changed after the page left it. */
export interface DraftConflict {
  draft: PendingDraft;
  /** The workflow's name on the server now. */
  workflowName: string;
}

/** What replaying the tenant's pending drafts did. */
export interface ReplayResult {
  /** Drafts the server could not take now (offline, unavailable); kept for the next visit. */
  waiting: number;
  /** Drafts kept until the user decides, since the server draft holds someone else's edits. */
  conflicts: DraftConflict[];
  /** Titles refused for good (taken, blank) after the graph itself was saved. */
  refusedNames: string[];
}

type Outcome =
  | { kind: "saved" | "gone" | "waiting" | "name-refused" }
  | { kind: "conflict"; workflowName: string };

/**
 * Writes every draft the tenant's page left unsaved, before the editor reads the library, so a
 * reload shows the last edits. Each draft is checked against the server draft first: one still
 * at the draft's base, or at a graph the page itself sent, is overwritten; one that already
 * holds the draft (the keepalive landed) is left alone; one someone else changed meanwhile is
 * not touched, and the draft waits for the user, also on later visits until they decide. The
 * contract has no conditional write, so a change landing between the read and the write is not
 * caught here.
 */
export async function replayPendingDrafts(
  tenantId: string,
  contracts: WorkflowContracts,
  storage: PendingDraftStorage = browserPendingDraftStorage,
): Promise<ReplayResult> {
  const result: ReplayResult = { waiting: 0, conflicts: [], refusedNames: [] };
  for (const draft of pendingDrafts(tenantId, storage)) {
    let outcome: Outcome;
    try {
      outcome = await replayOne(contracts, draft);
    } catch {
      outcome = { kind: "waiting" };
    }
    if (outcome.kind === "conflict") {
      if (!draft.conflict) markConflict(tenantId, draft, storage);
      result.conflicts.push({
        draft: { ...draft, conflict: true },
        workflowName: outcome.workflowName,
      });
      continue;
    }
    if (outcome.kind === "waiting") {
      result.waiting += 1;
      continue;
    }
    if (outcome.kind === "name-refused") result.refusedNames.push(draft.name ?? "");
    forgetPendingDraft(tenantId, draft.workflowId, draft.graph, storage);
  }
  return result;
}

async function replayOne(contracts: WorkflowContracts, draft: PendingDraft): Promise<Outcome> {
  let current;
  try {
    current = await contracts.get({ workflowId: draft.workflowId });
  } catch (error) {
    return { kind: isCode(error, "workflow_not_found") ? "gone" : "waiting" };
  }
  const server = current.draft;
  if (server.graph !== draft.graph) {
    const untouched =
      draft.baseUpdatedAt !== null &&
      server.updatedAt !== null &&
      Number(server.updatedAt) === draft.baseUpdatedAt;
    const ownWrite = draft.sent.includes(graphFingerprint(server.graph));
    if (draft.conflict || (!untouched && !ownWrite)) {
      return { kind: "conflict", workflowName: current.workflow.name };
    }
    try {
      await contracts.updateDraft({ workflowId: draft.workflowId, graph: draft.graph });
    } catch (error) {
      return { kind: isCode(error, "workflow_not_found") ? "gone" : "waiting" };
    }
  }
  // The graph is on the server now; a title that failed for a passing reason is tried again on
  // the next visit (the graph then matches and is not written twice), one refused for good is not.
  const name = draft.name?.trim() ?? "";
  if (name === "" || current.workflow.name === name) return { kind: "saved" };
  try {
    await contracts.update({ workflowId: draft.workflowId, name });
    return { kind: "saved" };
  } catch (error) {
    if (isCode(error, "workflow_not_found")) return { kind: "gone" };
    if (isCode(error, "workflow_name_conflict") || isCode(error, "workflow_name_blank")) {
      return { kind: "name-refused" };
    }
    return { kind: "waiting" };
  }
}

/** How the user settles a draft kept against someone else's edits. */
export type DraftConflictChoice = "overwrite" | "copy" | "discard";

/**
 * Settles one conflict as the user chose: `overwrite` writes the kept draft over the server's,
 * `copy` saves it as a new workflow beside the changed one (named after it, numbered when the
 * name is taken), `discard` drops it. The kept draft is forgotten only once that worked, so a
 * failed attempt can be repeated. Returns the new workflow's name for `copy`.
 */
export async function settleDraftConflict(
  tenantId: string,
  contracts: WorkflowContracts,
  conflict: DraftConflict,
  choice: DraftConflictChoice,
  storage: PendingDraftStorage = browserPendingDraftStorage,
): Promise<string | null> {
  const { draft } = conflict;
  let created: string | null = null;
  if (choice === "overwrite") {
    await contracts.updateDraft({ workflowId: draft.workflowId, graph: draft.graph });
    const name = draft.name?.trim() ?? "";
    if (name !== "" && name !== conflict.workflowName) {
      // The graph was the point; a title someone took meanwhile stays as the server has it.
      await contracts.update({ workflowId: draft.workflowId, name }).catch(() => undefined);
    }
  } else if (choice === "copy") {
    const base = `${draft.name?.trim() || conflict.workflowName}（离开前的修改）`;
    for (let attempt = 1; created === null; attempt += 1) {
      const name = attempt === 1 ? base : `${base} ${attempt}`;
      try {
        await contracts.create({ name, graph: draft.graph });
        created = name;
      } catch (error) {
        if (!isCode(error, "workflow_name_conflict") || attempt >= 20) throw error;
      }
    }
  }
  forgetPendingDraft(tenantId, draft.workflowId, undefined, storage);
  return created;
}

const isCode = (error: unknown, code: string): boolean =>
  error instanceof ContractError && error.code === code;
