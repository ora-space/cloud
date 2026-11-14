import { describe, expect, it } from "vitest";
import { ContractError } from "../contracts";
import {
  LocalWorkflowBackend,
  isValidUserVersion,
  type WorkflowBackendStorage,
} from "./local-workflow-backend";

function memoryStorage(): WorkflowBackendStorage & { value: string | null } {
  return {
    value: null,
    load() {
      return this.value;
    },
    save(serialized) {
      this.value = serialized;
    },
  };
}

/** A backend with a controllable clock and sequential ids, so every answer is deterministic. */
function backend(storage = memoryStorage()) {
  let clock = 1_000;
  let id = 0;
  const instance = new LocalWorkflowBackend({
    storage,
    now: () => clock,
    newId: () => `id-${++id}`,
  });
  return {
    instance,
    storage,
    tick: (ms = 1) => {
      clock += ms;
    },
  };
}

function refusal(action: () => unknown): string {
  try {
    action();
  } catch (error) {
    if (error instanceof ContractError) return error.code;
    throw error;
  }
  throw new Error("expected the operation to be refused");
}

const twoNodeGraph = JSON.stringify({
  nodes: [
    { id: "start", data: { kind: "start" } },
    { id: "out", data: { kind: "output" } },
    { id: "spare", data: { kind: "agent" } },
  ],
  edges: [{ source: "start", target: "out" }],
});

describe("LocalWorkflowBackend", () => {
  it("creates workflows with desktop's default graph and lists the newest first", () => {
    const { instance, tick } = backend();
    const first = instance.create({ name: "  甲  ", graph: null });
    tick();
    instance.create({ name: "乙", graph: null });

    expect(first.workflow.name).toBe("甲");
    expect(JSON.parse(first.draft.graph).nodes[0].data.kind).toBe("start");
    expect(first.draft.version).toBe("draft");
    expect(instance.list().workflows.map((workflow) => workflow.name)).toEqual([
      "乙",
      "甲",
    ]);
  });

  it("refuses blank and duplicate names, including on rename", () => {
    const { instance } = backend();
    const { workflow } = instance.create({ name: "甲", graph: null });
    instance.create({ name: "乙", graph: null });

    expect(refusal(() => instance.create({ name: " ", graph: null }))).toBe(
      "workflow_name_blank",
    );
    expect(refusal(() => instance.create({ name: "甲", graph: null }))).toBe(
      "workflow_name_conflict",
    );
    expect(
      refusal(() => instance.update({ workflowId: workflow.id, name: "乙" })),
    ).toBe("workflow_name_conflict");
    expect(instance.update({ workflowId: workflow.id, name: "甲" }).workflow.name).toBe(
      "甲",
    );
  });

  it("publishes immutable snapshots that later draft edits do not change", () => {
    const { instance, tick } = backend();
    const { workflow } = instance.create({ name: "甲", graph: twoNodeGraph });
    const published = instance.publish({ workflowId: workflow.id, version: "v1" });
    tick();
    instance.updateDraft({ workflowId: workflow.id, graph: '{"nodes":[],"edges":[]}' });

    expect(
      instance.getVersion({ workflowId: workflow.id, version: "v1" }).snapshot.graph,
    ).toBe(twoNodeGraph);
    expect(instance.get({ workflowId: workflow.id }).published?.id).toBe(
      published.snapshot.id,
    );
    expect(instance.list().workflows[0]?.publishedVersion).toBe("v1");
  });

  it("applies desktop's version rules", () => {
    const { instance } = backend();
    const { workflow } = instance.create({ name: "甲", graph: twoNodeGraph });
    const publish = (version: string | null) =>
      instance.publish({ workflowId: workflow.id, version });

    expect(refusal(() => publish("draft"))).toBe("workflow_version_reserved");
    expect(refusal(() => publish("a/b"))).toBe("workflow_version_invalid");
    publish("v1");
    expect(refusal(() => publish("v1"))).toBe("workflow_version_already_exists");
    // Automatic versions in the same millisecond get a suffix instead of colliding.
    expect(publish(null).snapshot.version).toBe("v1000");
    expect(publish(null).snapshot.version).toBe("v1000-1");
  });

  it("refuses to publish a graph the engine could not run, but keeps saving it as a draft", () => {
    const { instance } = backend();
    const { workflow } = instance.create({
      name: "甲",
      graph: '{"nodes":[{"id":"x","data":{"kind":"agent"}}],"edges":[]}',
    });

    expect(refusal(() => instance.publish({ workflowId: workflow.id, version: null }))).toBe(
      "workflow_run_graph_parse",
    );
    instance.updateDraft({ workflowId: workflow.id, graph: "not json" });
    expect(instance.getDraft({ workflowId: workflow.id }).snapshot.graph).toBe("not json");
  });

  it("activates and rolls back by copying a version into the draft", () => {
    const { instance } = backend();
    const { workflow } = instance.create({ name: "甲", graph: twoNodeGraph });
    const v1 = instance.publish({ workflowId: workflow.id, version: "v1" }).snapshot;
    const edited = twoNodeGraph.replace('"spare"', '"spare-2"');
    instance.updateDraft({ workflowId: workflow.id, graph: edited });
    const v2 = instance.publish({ workflowId: workflow.id, version: "v2" }).snapshot;

    instance.rollback({ workflowId: workflow.id, snapshotId: v1.id });
    expect(instance.get({ workflowId: workflow.id })).toMatchObject({
      draft: { graph: twoNodeGraph },
      published: { id: v2.id },
    });

    instance.activate({ workflowId: workflow.id, snapshotId: v2.id });
    expect(instance.get({ workflowId: workflow.id })).toMatchObject({
      draft: { graph: edited },
      published: { id: v2.id },
    });
    const draftId = instance.getDraft({ workflowId: workflow.id }).snapshot.id;
    expect(
      refusal(() => instance.activate({ workflowId: workflow.id, snapshotId: draftId })),
    ).toBe("workflow_cannot_activate_draft");
  });

  it("refuses to delete the draft or the published version, and soft-deletes the rest", () => {
    const { instance, tick } = backend();
    const { workflow } = instance.create({ name: "甲", graph: twoNodeGraph });
    instance.publish({ workflowId: workflow.id, version: "v1" });
    tick();
    instance.publish({ workflowId: workflow.id, version: "v2" });
    const remove = (version: string) =>
      instance.deleteSnapshot({ workflowId: workflow.id, version });

    expect(refusal(() => remove("draft"))).toBe("workflow_cannot_delete_draft");
    expect(refusal(() => remove("v2"))).toBe("workflow_cannot_delete_active_version");
    remove("v1");
    expect(
      instance.listVersions({ workflowId: workflow.id }).versions.map((v) => v.version),
    ).toEqual(["v2"]);

    instance.delete({ workflowId: workflow.id });
    expect(instance.list().workflows).toEqual([]);
    expect(refusal(() => instance.get({ workflowId: workflow.id }))).toBe(
      "workflow_not_found",
    );
    // The name is free again once the workflow is gone.
    instance.create({ name: "甲", graph: null });
  });

  it("reloads every workflow and snapshot from storage with bigint timestamps", () => {
    const first = backend();
    const { workflow } = first.instance.create({ name: "甲", graph: twoNodeGraph });
    first.instance.publish({ workflowId: workflow.id, version: "v1" });

    const reloaded = new LocalWorkflowBackend({ storage: first.storage });
    expect(reloaded.get({ workflowId: workflow.id })).toEqual(
      first.instance.get({ workflowId: workflow.id }),
    );
    expect(typeof reloaded.list().workflows[0]?.createdAt).toBe("bigint");
  });

  it("reports spare and unrecognized nodes like desktop's analysis", () => {
    const { instance } = backend();
    const graph = JSON.stringify({
      nodes: [
        { id: "start", data: { kind: "start" } },
        { id: "out", data: { kind: "output" } },
        { id: "spare", data: { kind: "agent" } },
        { id: "mystery", data: { kind: "teleport" } },
      ],
      edges: [{ source: "start", target: "out" }],
    });

    expect(instance.analyze({ graph })).toEqual({
      unusedNodeIds: ["spare"],
      unrecognizedNodes: [{ nodeId: "mystery", kind: "teleport" }],
      unappliedSettings: [],
      unsupportedNodes: [],
    });
  });
});

describe("isValidUserVersion", () => {
  it("accepts addressable versions and rejects the rest", () => {
    for (const version of ["v1.0.0", "1.0.0", "中文版本", "a", "a".repeat(128)]) {
      expect(isValidUserVersion(version), version).toBe(true);
    }
    for (const version of ["", "   ", ".", "..", "a/b", "a\\b", "a\tb", "a\nb", "a".repeat(129)]) {
      expect(isValidUserVersion(version), JSON.stringify(version)).toBe(false);
    }
  });
});
