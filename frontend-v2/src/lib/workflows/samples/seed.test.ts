import { describe, expect, it } from "vitest";
import { LocalWorkflowBackend } from "../data/local-workflow-backend";
import { ContractError } from "../contracts";
import {
  localWorkflowContracts,
  memoryWorkflowStorage,
} from "../test/workflow-server";
import { addSampleWorkflows } from "./seed";

const library = () => new LocalWorkflowBackend({ storage: memoryWorkflowStorage() });

describe("addSampleWorkflows", () => {
  it("publishes every sample, the authored ones first", async () => {
    const backend = library();

    const result = await addSampleWorkflows(localWorkflowContracts(backend));

    const { workflows } = backend.list();
    expect(workflows.map((workflow) => [workflow.name, workflow.publishedVersion])).toEqual([
      ["示例：代码审查", "v1"],
      ["示例：逐段翻译", "v1"],
      ["循环综合测试：分支与退出", "v1"],
      ["循环综合测试：并行退出取消", "v1"],
      ["最大循环轮次开关测试（无需模型）", "v1"],
    ]);
    expect(result).toEqual({
      added: workflows.map((workflow) => workflow.name).reverse(),
      skipped: [],
      unpublished: [],
    });
  });

  it("covers all eight executable node kinds with no spare nodes", async () => {
    const backend = library();
    await addSampleWorkflows(localWorkflowContracts(backend));

    const kinds = new Set<string>();
    for (const { id } of backend.list().workflows) {
      const { draft } = backend.get({ workflowId: id });
      const graph = JSON.parse(draft.graph) as { nodes: { data: { kind: string } }[] };
      for (const node of graph.nodes) kinds.add(node.data.kind);
      expect(backend.analyze({ graph: draft.graph }), id).toEqual({
        unusedNodeIds: [],
        unrecognizedNodes: [],
      });
    }
    expect([...kinds].sort()).toEqual(
      ["agent", "aggregator", "condition", "iteration", "loop", "loopExit", "output", "start"].sort(),
    );
  });

  it("points desktop's Loop examples at the demo executor", async () => {
    const backend = library();
    await addSampleWorkflows(localWorkflowContracts(backend));
    const loop = backend.list().workflows.find((workflow) =>
      workflow.name.startsWith("循环综合测试：分支"),
    )!;
    const graph = JSON.parse(backend.get({ workflowId: loop.id }).draft.graph) as {
      nodes: { data: { agentConfig?: { executor: unknown; roleId: string } } }[];
    };
    const agents = graph.nodes.flatMap((node) =>
      node.data.agentConfig === undefined ? [] : [node.data.agentConfig],
    );
    expect(agents.length).toBeGreaterThan(0);
    for (const agent of agents) {
      expect(agent).toMatchObject({
        executor: { agentCli: "ora/claude-code", modelId: "claude-sonnet-5-5" },
        roleId: "",
      });
    }
  });

  it("adds the samples beside existing workflows, skipping a taken name", async () => {
    const backend = library();
    backend.create({ name: "我的工作流", graph: null });
    backend.create({ name: "示例：逐段翻译", graph: null });

    const result = await addSampleWorkflows(localWorkflowContracts(backend));

    const names = backend.list().workflows.map((workflow) => workflow.name);
    expect(names).toContain("我的工作流");
    expect(names).toContain("示例：代码审查");
    expect(names.filter((name) => name === "示例：逐段翻译")).toHaveLength(1);
    expect(names).toHaveLength(6);
    expect(result.skipped).toEqual(["示例：逐段翻译"]);
  });

  it("keeps a sample as a draft when publishing is refused", async () => {
    const backend = library();
    const contracts = {
      ...localWorkflowContracts(backend),
      publish: async () => {
        throw new ContractError("workflow_domain_unavailable");
      },
    };

    const result = await addSampleWorkflows(contracts);

    expect(result.added).toEqual([]);
    expect(result.unpublished).toHaveLength(5);
    expect(backend.list().workflows.every((w) => w.publishedVersion === null)).toBe(true);
  });
});
