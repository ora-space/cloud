import "@/lib/workflows/test/dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { I18nextProvider } from "react-i18next";
import { describe, expect, it, vi } from "vitest";
import { UserContext } from "@/app/auth/user";
import { TooltipProvider } from "@/components/ui/tooltip";
import { createContractsClient } from "@/lib/workflows/data/contracts-client";
import { ContractsClientProvider } from "@/lib/workflows/data/contracts-client-provider";
import { LocalWorkflowBackend } from "@/lib/workflows/data/local-workflow-backend";
import { workflowI18n } from "@/lib/workflows/host/i18n";
import { createMockWorkflowCapabilities } from "@/lib/workflows/mock";
import { localWorkflowContracts, memoryWorkflowStorage } from "@/lib/workflows/test/workflow-server";
import { demoUser, fakeBackend } from "@/test/backend";
import { WorkflowEditor } from "./workflow-editor";
import { useWorkflowEditorStore } from "./workflow-editor-store";

const tenantId = "model-hydration";
const agent = "official/ora-space.opencode";
const catalog = [
  { id: "other-agent", agentId: "another-agent", agentName: "Other Agent", modelId: "foreign", modelName: "Foreign" },
  { id: "first", agentId: agent, agentName: "OpenCode", modelId: "configured", modelName: "Current" },
  { id: "second", agentId: agent, agentName: "OpenCode", modelId: "second", modelName: "Second" },
];

/** Mounts the real editor with a catalog already available when the draft hydrates. */
async function openEditor(modelId: string, options: { models?: typeof catalog; agentCli?: string; override?: boolean } = {}) {
  fakeBackend(() => undefined);
  const backend = new LocalWorkflowBackend({ storage: memoryWorkflowStorage() });
  const initialExecutor = { agentCli: options.agentCli ?? agent, modelId };
  const created = backend.create({ name: "Model hydration", graph: JSON.stringify({
    nodes: [
      { id: "start", type: "workflow", position: { x: 0, y: 0 }, data: { kind: "start", title: "开始" } },
      { id: "agent", type: "workflow", position: { x: 320, y: 0 }, data: { kind: "agent", title: "模型测试", agentConfig: {
        ...createMockWorkflowCapabilities("zh-CN").defaultAgentConfig, executor: initialExecutor,
      } } },
    ],
    edges: [{ id: "edge", source: "start", target: "agent" }],
  }) });
  const workflowId = created.workflow.id;
  const update = vi.spyOn(backend, "updateDraft");
  const client = createContractsClient(localWorkflowContracts(backend), tenantId);
  const queries = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  queries.setQueryData(["workflow-catalog", "executors", tenantId], { items: options.models ?? catalog });
  useWorkflowEditorStore.setState({ selectedWorkflowId: workflowId, managerError: null });
  render(<I18nextProvider i18n={workflowI18n}><QueryClientProvider client={queries}>
    <ContractsClientProvider client={client}><UserContext.Provider value={demoUser}><TooltipProvider>
      <WorkflowEditor {...(options.override ? { capabilities: createMockWorkflowCapabilities("zh-CN") } : {})} />
    </TooltipProvider></UserContext.Provider></ContractsClientProvider>
  </QueryClientProvider></I18nextProvider>);
  await screen.findByDisplayValue("Model hydration");
  await screen.findByLabelText("工作流画布");
  const savedExecutor = () => JSON.parse(backend.getDraft({ workflowId }).snapshot.graph).nodes
    .find((node: { id: string }) => node.id === "agent").data.agentConfig.executor;
  return { backend, workflowId, update, initialExecutor, savedExecutor };
}

describe("editor model hydration", () => {
  it.each([
    { name: "stale model", model: "deepseek/deepseek-v4-flash", expected: "configured" },
    { name: "blank model", model: "", expected: "configured" },
    { name: "available non-first model", model: "second", expected: "second" },
    { name: "model of another Agent", model: "foreign", expected: "configured" },
    { name: "missing Agent", model: "stale", expected: "stale", agentCli: "missing-agent" },
    { name: "empty catalog", model: "stale", expected: "stale", models: [] },
    { name: "capability override", model: "stale", expected: "stale", override: true },
  ])("hydrates $name and only persists on a later save", async ({ model, expected, ...options }) => {
    const state = await openEditor(model, options);
    expect(screen.queryByText("未保存")).not.toBeInTheDocument();
    expect(state.update).not.toHaveBeenCalled();
    expect(state.savedExecutor()).toEqual(state.initialExecutor);
    // Like desktop, hydration is a clean baseline. Publishing first forces a save of that baseline.
    fireEvent.click(screen.getByRole("button", { name: "发布" }));
    await screen.findByRole("alertdialog", { name: "发布工作流" });
    await waitFor(() => expect(state.update).toHaveBeenCalledOnce());
    expect(state.savedExecutor()).toEqual({ agentCli: options.agentCli ?? agent, modelId: expected });
  });

  it("uses desktop's nonblank base executor for a new Agent when the catalog is empty", async () => {
    const state = await openEditor("stale", { models: [] });
    fireEvent.click(screen.getByRole("button", { name: "Agent" }));
    expect(await screen.findByText("未保存")).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "发布" }));
    await screen.findByRole("alertdialog", { name: "发布工作流" });
    const nodes = JSON.parse(state.backend.getDraft({ workflowId: state.workflowId }).snapshot.graph).nodes;
    const added = nodes.find((node: { id: string; data: { kind: string } }) => node.data.kind === "agent" && node.id !== "agent");
    expect(added.data.agentConfig.executor).toEqual(createMockWorkflowCapabilities("zh-CN").defaultAgentConfig.executor);
    expect(added.data.agentConfig.executor.modelId).not.toBe("");
  });
});
