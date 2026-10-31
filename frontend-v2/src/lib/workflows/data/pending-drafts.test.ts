import { describe, expect, it, vi } from "vitest";
import { ContractError } from "../contracts";
import { localWorkflowContracts, memoryWorkflowStorage } from "../test/workflow-server";
import { LocalWorkflowBackend } from "./local-workflow-backend";
import {
  forgetPendingDraft,
  graphFingerprint,
  pendingDrafts,
  rememberPendingDraft,
  replayPendingDrafts,
  settleDraftConflict,
  type PendingDraft,
  type PendingDraftStorage,
} from "./pending-drafts";

const mine = '{"nodes":[{"id":"mine"}],"edges":[]}';
const theirs = '{"nodes":[{"id":"theirs"}],"edges":[]}';

function memory(): PendingDraftStorage & { value: string | null } {
  const storage = {
    value: null as string | null,
    load: () => storage.value,
    save: (serialized: string | null) => {
      storage.value = serialized;
    },
  };
  return storage;
}

/** A server whose clock moves on every write, as PostgreSQL's `now()` does between requests. */
function server() {
  let clock = 1_000;
  const backend = new LocalWorkflowBackend({
    storage: memoryWorkflowStorage(),
    now: () => (clock += 1),
  });
  return { backend, contracts: localWorkflowContracts(backend) };
}

function draftAt(backend: LocalWorkflowBackend, workflowId: string) {
  const { snapshot } = backend.getDraft({ workflowId });
  return { graph: snapshot.graph, updatedAt: Number(snapshot.updatedAt) };
}

/** What the editor keeps when it leaves `workflowId` with `graph` unsaved. */
function left(
  backend: LocalWorkflowBackend,
  workflowId: string,
  graph: string,
  extra: Partial<PendingDraft> = {},
): PendingDraft {
  return {
    workflowId,
    graph,
    name: null,
    baseUpdatedAt: draftAt(backend, workflowId).updatedAt,
    sent: [],
    ...extra,
  };
}

const draft = (workflowId: string, graph: string): PendingDraft => ({
  workflowId,
  graph,
  name: null,
  baseUpdatedAt: null,
  sent: [],
});

describe("pending drafts", () => {
  it("keeps one draft per workflow and tenant, the latest winning", () => {
    const storage = memory();
    rememberPendingDraft("t1", draft("w1", "old"), storage);
    rememberPendingDraft("t1", draft("w1", "new"), storage);
    rememberPendingDraft("t2", draft("w9", "other"), storage);

    expect(pendingDrafts("t1", storage)).toEqual([draft("w1", "new")]);
    forgetPendingDraft("t1", "w1", undefined, storage);
    expect(pendingDrafts("t1", storage)).toEqual([]);
    forgetPendingDraft("t2", "w9", undefined, storage);
    expect(storage.value).toBeNull();
  });

  it("is not forgotten by a write of another graph landing late", () => {
    const storage = memory();
    rememberPendingDraft("t1", draft("w1", "newer"), storage);

    forgetPendingDraft("t1", "w1", "older", storage);
    expect(pendingDrafts("t1", storage)).toEqual([draft("w1", "newer")]);
    forgetPendingDraft("t1", "w1", "newer", storage);
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("writes a draft the server still has at its base, and renames the workflow", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "旧名", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine, { name: "新名" }), storage);

    const result = await replayPendingDrafts("t1", contracts, storage);

    expect(result).toEqual({ waiting: 0, conflicts: [], refusedNames: [] });
    expect(draftAt(backend, workflow.id).graph).toBe(mine);
    expect(backend.get({ workflowId: workflow.id }).workflow.name).toBe("新名");
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("writes nothing when the keepalive already landed", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "名", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine), storage);
    backend.updateDraft({ workflowId: workflow.id, graph: mine });
    const updateDraft = vi.spyOn(contracts, "updateDraft");

    await replayPendingDrafts("t1", contracts, storage);

    expect(updateDraft).not.toHaveBeenCalled();
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("leaves a teammate's later edit alone when the keepalive had landed before it", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "共用", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine), storage);
    backend.updateDraft({ workflowId: workflow.id, graph: mine }); // the keepalive
    backend.updateDraft({ workflowId: workflow.id, graph: theirs }); // a teammate, later

    const result = await replayPendingDrafts("t1", contracts, storage);

    expect(draftAt(backend, workflow.id).graph).toBe(theirs);
    expect(result.conflicts).toEqual([
      {
        draft: { ...left(backend, workflow.id, mine), baseUpdatedAt: expect.any(Number), conflict: true },
        workflowName: "共用",
      },
    ]);
    // Still put to the user on the next visit, and the server is still not written.
    const again = await replayPendingDrafts("t1", contracts, storage);
    expect(again.conflicts.map((c) => c.draft.graph)).toEqual([mine]);
    expect(draftAt(backend, workflow.id).graph).toBe(theirs);
  });

  it("keeps a waiting conflict when the page edits and leaves the workflow again", () => {
    const storage = memory();
    rememberPendingDraft("t1", { ...draft("w1", mine), conflict: true }, storage);

    rememberPendingDraft("t1", draft("w1", "later"), storage);

    expect(pendingDrafts("t1", storage)).toEqual([{ ...draft("w1", mine), conflict: true }]);
  });

  it("overwrites a draft only its own unanswered write moved", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "名", graph: null });
    const inFlight = '{"nodes":[{"id":"in-flight"}],"edges":[]}';
    rememberPendingDraft(
      "t1",
      left(backend, workflow.id, mine, { sent: [graphFingerprint(inFlight)] }),
      storage,
    );
    backend.updateDraft({ workflowId: workflow.id, graph: inFlight });

    const result = await replayPendingDrafts("t1", contracts, storage);

    expect(result.conflicts).toEqual([]);
    expect(draftAt(backend, workflow.id).graph).toBe(mine);
  });

  it("drops a deleted workflow's draft and keeps one the server could not take", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "留着", graph: null });
    rememberPendingDraft("t1", draft("gone", mine), storage);
    rememberPendingDraft("t1", left(backend, workflow.id, mine), storage);
    const refusing = {
      ...contracts,
      updateDraft: async () => {
        throw new ContractError("workflow_store_unavailable");
      },
    };

    expect((await replayPendingDrafts("t1", refusing, storage)).waiting).toBe(1);
    expect(pendingDrafts("t1", storage).map((d) => d.workflowId)).toEqual([workflow.id]);
  });

  it("retries a title that failed for a passing reason without writing the graph twice", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "原名", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine, { name: "新名" }), storage);
    const flaky = {
      ...contracts,
      update: async () => {
        throw new ContractError("workflow_store_unavailable");
      },
    };

    expect((await replayPendingDrafts("t1", flaky, storage)).waiting).toBe(1);
    expect(draftAt(backend, workflow.id).graph).toBe(mine);
    expect(pendingDrafts("t1", storage)).toHaveLength(1);

    const updateDraft = vi.spyOn(contracts, "updateDraft");
    expect((await replayPendingDrafts("t1", contracts, storage)).waiting).toBe(0);
    expect(updateDraft).not.toHaveBeenCalled();
    expect(backend.get({ workflowId: workflow.id }).workflow.name).toBe("新名");
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("keeps the graph and reports a title that is taken", async () => {
    const storage = memory();
    const { backend, contracts } = server();
    backend.create({ name: "占用", graph: null });
    const { workflow } = backend.create({ name: "原名", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine, { name: "占用" }), storage);

    const result = await replayPendingDrafts("t1", contracts, storage);

    expect(result.refusedNames).toEqual(["占用"]);
    expect(draftAt(backend, workflow.id).graph).toBe(mine);
    expect(backend.get({ workflowId: workflow.id }).workflow.name).toBe("原名");
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });
});

describe("settleDraftConflict", () => {
  async function conflicted() {
    const storage = memory();
    const { backend, contracts } = server();
    const { workflow } = backend.create({ name: "共用", graph: null });
    rememberPendingDraft("t1", left(backend, workflow.id, mine), storage);
    backend.updateDraft({ workflowId: workflow.id, graph: theirs });
    const [conflict] = (await replayPendingDrafts("t1", contracts, storage)).conflicts;
    return { storage, backend, contracts, workflow, conflict };
  }

  it("overwrites the server draft when asked", async () => {
    const { storage, backend, contracts, workflow, conflict } = await conflicted();

    await settleDraftConflict("t1", contracts, conflict, "overwrite", storage);

    expect(draftAt(backend, workflow.id).graph).toBe(mine);
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("saves the kept draft as a new workflow, numbering a taken name", async () => {
    const { storage, backend, contracts, workflow, conflict } = await conflicted();
    backend.create({ name: "共用（离开前的修改）", graph: null });

    const name = await settleDraftConflict("t1", contracts, conflict, "copy", storage);

    expect(name).toBe("共用（离开前的修改） 2");
    const copy = backend.list().workflows.find((w) => w.name === name)!;
    expect(draftAt(backend, copy.id).graph).toBe(mine);
    expect(draftAt(backend, workflow.id).graph).toBe(theirs);
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });

  it("discards the kept draft, and keeps it when settling fails", async () => {
    const { storage, backend, contracts, workflow, conflict } = await conflicted();
    const failing = {
      ...contracts,
      updateDraft: async () => {
        throw new ContractError("workflow_store_unavailable");
      },
    };

    await expect(settleDraftConflict("t1", failing, conflict, "overwrite", storage)).rejects.toThrow();
    expect(pendingDrafts("t1", storage)).toHaveLength(1);

    await settleDraftConflict("t1", contracts, conflict, "discard", storage);
    expect(draftAt(backend, workflow.id).graph).toBe(theirs);
    expect(pendingDrafts("t1", storage)).toEqual([]);
  });
});
