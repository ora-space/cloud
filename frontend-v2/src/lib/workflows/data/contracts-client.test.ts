import { afterEach, describe, expect, it, vi } from "vitest";
import { http } from "@/lib/api-client";
import { fakeBackend } from "@/test/backend";
import { ContractError } from "../contracts";
import { memoryWorkflowStorage, serveWorkflows } from "../test/workflow-server";
import { browserDraftKeeper, createHttpWorkflowContracts } from "./contracts-client";
import { pendingDrafts } from "./pending-drafts";
import { LocalWorkflowBackend } from "./local-workflow-backend";

const tenantId = "00000000-0000-4000-8000-0000000000aa";
const originalAdapter = http.defaults.adapter;

function server() {
  fakeBackend(() => undefined);
  return serveWorkflows(tenantId, new LocalWorkflowBackend({ storage: memoryWorkflowStorage() }));
}

describe("createHttpWorkflowContracts", () => {
  afterEach(() => {
    http.defaults.adapter = originalAdapter;
  });

  it("answers desktop's DTOs over the tenant's routes, with bigint instants", async () => {
    const fake = server();
    const contracts = createHttpWorkflowContracts(tenantId);

    const created = await contracts.create({ name: "审查", graph: null });
    const { workflow, draft } = created;
    expect(typeof workflow.createdAt).toBe("bigint");
    expect(draft.version).toBe("draft");

    await contracts.updateDraft({ workflowId: workflow.id, graph: draft.graph });
    const published = await contracts.publish({ workflowId: workflow.id, version: "v1" });
    expect(published.snapshot).toMatchObject({ version: "v1", updatedAt: null });
    expect(typeof published.snapshot.createdAt).toBe("bigint");

    const detail = await contracts.get({ workflowId: workflow.id });
    expect(detail.published?.id).toBe(published.snapshot.id);
    expect((await contracts.list({})).workflows[0]).toMatchObject({
      name: "审查",
      publishedVersion: "v1",
    });
    expect((await contracts.listVersions({ workflowId: workflow.id })).versions.map((v) => v.version)).toEqual(
      expect.arrayContaining(["v1"]),
    );
    expect(fake.seen).toEqual([
      `POST /api/v2/tenants/${tenantId}/workflows`,
      `PUT /api/v2/tenants/${tenantId}/workflows/${workflow.id}/draft`,
      `POST /api/v2/tenants/${tenantId}/workflows/${workflow.id}/publish`,
      `GET /api/v2/tenants/${tenantId}/workflows/${workflow.id}`,
      `GET /api/v2/tenants/${tenantId}/workflows`,
      `GET /api/v2/tenants/${tenantId}/workflows/${workflow.id}/versions`,
    ]);
  });

  it("reads and deletes versions whose names are not URL-safe", async () => {
    const fake = server();
    const contracts = createHttpWorkflowContracts(tenantId);
    const { workflow } = await contracts.create({ name: "版本名", graph: null });

    for (const version of ["release?x", "a#b", "50%", "v1 中文"]) {
      await contracts.publish({ workflowId: workflow.id, version });
      expect((await contracts.getVersion({ workflowId: workflow.id, version })).snapshot.version).toBe(
        version,
      );
      expect(fake.seen.at(-1)).toBe(
        `GET /api/v2/tenants/${tenantId}/workflows/${workflow.id}/versions/${encodeURIComponent(version)}`,
      );
    }
    // The newest is active; the others can be deleted by their names.
    for (const version of ["release?x", "a#b", "50%"]) {
      expect(await contracts.deleteSnapshot({ workflowId: workflow.id, version })).toMatchObject({ version });
    }
    expect((await contracts.listVersions({ workflowId: workflow.id })).versions.map((v) => v.version)).not.toContain(
      "a#b",
    );
  });

  it("turns a refusal into the ContractError the editor localizes", async () => {
    server();
    const contracts = createHttpWorkflowContracts(tenantId);
    await contracts.create({ name: "审查", graph: null });

    const refused = await contracts.create({ name: "审查", graph: null }).catch((error: unknown) => error);

    expect(refused).toBeInstanceOf(ContractError);
    expect(refused).toMatchObject({ code: "workflow_name_conflict" });
  });

  it("keeps a failure without a fault body as it came", async () => {
    const fake = server();
    fake.refuse = { match: /GET .*\/workflows$/, status: 502, data: "bad gateway" };

    const failed = await createHttpWorkflowContracts(tenantId).list({}).catch((error: unknown) => error);

    expect(failed).not.toBeInstanceOf(ContractError);
  });
});

describe("browserDraftKeeper", () => {
  afterEach(() => {
    http.defaults.adapter = originalAdapter;
    window.localStorage.clear();
  });

  it("remembers the draft and sends it with keepalive through the generated client", async () => {
    const fake = server();
    const { workflow } = fake.backend.create({ name: "名", graph: null });
    const keeper = browserDraftKeeper(tenantId);
    const graph = '{"nodes":[],"edges":[]}';

    keeper.keep({ workflowId: workflow.id, graph, name: null, baseUpdatedAt: null, sent: [] });

    expect(pendingDrafts(tenantId)).toHaveLength(1);
    await vi.waitFor(() => expect(pendingDrafts(tenantId)).toEqual([]));
    expect(fake.keptAlive).toEqual([`PUT /api/v2/tenants/${tenantId}/workflows/${workflow.id}/draft`]);
    expect(fake.backend.getDraft({ workflowId: workflow.id }).snapshot.graph).toBe(graph);
  });

  it("keeps the draft when the keepalive fails, or a title is still to apply", async () => {
    const fake = server();
    const { workflow } = fake.backend.create({ name: "名", graph: null });
    const keeper = browserDraftKeeper(tenantId);

    keeper.keep({ workflowId: workflow.id, graph: "{}", name: "新名", baseUpdatedAt: null, sent: [] });
    await vi.waitFor(() => expect(fake.keptAlive).toHaveLength(1));
    fake.refuse = { match: /PUT .*\/draft$/, status: 503, data: { code: "workflow_store_unavailable" } };
    keeper.keep({ workflowId: "w2", graph: "{}", name: null, baseUpdatedAt: null, sent: [] });
    await vi.waitFor(() => expect(fake.keptAlive).toHaveLength(2));

    expect(pendingDrafts(tenantId).map((d) => d.workflowId).sort()).toEqual([workflow.id, "w2"].sort());
  });
});
