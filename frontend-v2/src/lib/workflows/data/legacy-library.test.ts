import { describe, expect, it } from "vitest";
import { ContractError } from "../contracts";
import {
  localWorkflowContracts,
  memoryWorkflowStorage,
} from "../test/workflow-server";
import {
  importLegacyWorkflows,
  readLegacyWorkflows,
  type LegacyLibraryStorage,
} from "./legacy-library";
import { LocalWorkflowBackend } from "./local-workflow-backend";

function legacyStorage(): LegacyLibraryStorage & { answered: boolean } {
  const storage = memoryWorkflowStorage();
  const state = {
    ...storage,
    answered: false,
    handled: () => state.answered,
    markHandled: () => {
      state.answered = true;
    },
  };
  return state;
}

describe("legacy library", () => {
  it("reads each old workflow's draft, newest first", () => {
    const storage = legacyStorage();
    const old = new LocalWorkflowBackend({ storage });
    const first = old.create({ name: "旧的一个", graph: null });
    old.updateDraft({ workflowId: first.workflow.id, graph: '{"nodes":[],"edges":[]}' });
    old.create({ name: "旧的两个", graph: null });

    const workflows = readLegacyWorkflows(storage);

    expect(workflows.map((w) => w.name)).toEqual(["旧的两个", "旧的一个"]);
    expect(workflows[1]?.graph).toBe('{"nodes":[],"edges":[]}');
  });

  it("offers nothing once answered, and nothing for a browser that kept none", () => {
    const storage = legacyStorage();
    expect(readLegacyWorkflows(storage)).toEqual([]);
    new LocalWorkflowBackend({ storage }).create({ name: "旧的", graph: null });
    storage.markHandled();
    expect(readLegacyWorkflows(storage)).toEqual([]);
  });

  it("creates them on the server in their order, skipping taken names", async () => {
    const storage = legacyStorage();
    const server = new LocalWorkflowBackend({ storage: memoryWorkflowStorage() });
    server.create({ name: "旧的两个", graph: null });

    const result = await importLegacyWorkflows(
      localWorkflowContracts(server),
      [
        { name: "旧的两个", graph: '{"nodes":[],"edges":[]}' },
        { name: "旧的一个", graph: '{"nodes":[],"edges":[]}' },
      ],
      storage,
    );

    expect(result).toEqual({ imported: ["旧的一个"], skipped: ["旧的两个"] });
    expect(server.list().workflows.map((w) => w.name)).toEqual(["旧的一个", "旧的两个"]);
    expect(storage.answered).toBe(true);
  });

  it("leaves the offer open when the server refuses for another reason", async () => {
    const storage = legacyStorage();
    const contracts = {
      ...localWorkflowContracts(new LocalWorkflowBackend({ storage: memoryWorkflowStorage() })),
      create: async () => {
        throw new ContractError("workflow_store_unavailable");
      },
    };

    await expect(
      importLegacyWorkflows(contracts, [{ name: "旧的", graph: "{}" }], storage),
    ).rejects.toThrow("workflow_store_unavailable");
    expect(storage.answered).toBe(false);
  });
});
