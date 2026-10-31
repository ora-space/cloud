import {
  normalizeWorkflowGlobalVariables,
  type DemoWorkflow,
} from "../mock";
import { normalizeWorkflowDocument, serializeWorkflowGraph } from "../runtime";
import { parseWorkflowImportFile } from "../editor/workflow-transfer";
import { ContractError } from "../contracts";
import type { WorkflowContracts } from "../data/contracts-client";
import { reviewChainSample, translationBatchSample } from "./authored";
import loopBranchExit from "./loop-branch-exit.json?raw";
import loopBranchExitParallel from "./loop-branch-exit-parallel.json?raw";
import loopMaxIterationsSwitch from "./loop-max-iterations-switch.json?raw";

/** Desktop's Loop examples name agents this page's demo catalog does not have. */
const demoExecutor = { agentCli: "ora/claude-code", modelId: "claude-sonnet-5-5" };

const samples: { file: string; version: string; retarget: boolean }[] = [
  { file: JSON.stringify(reviewChainSample), version: "v1", retarget: false },
  { file: JSON.stringify(translationBatchSample), version: "v1", retarget: false },
  { file: loopBranchExit, version: "v1", retarget: true },
  { file: loopBranchExitParallel, version: "v1", retarget: true },
  { file: loopMaxIterationsSwitch, version: "v1", retarget: true },
];

/** What adding the samples did to each one, by name. */
export interface SampleWorkflowsResult {
  added: string[];
  /** A workflow of the same name was already in the library. */
  skipped: string[];
  /** Added as a draft, but publishing `v1` was refused (for instance while publishing is unavailable). */
  unpublished: string[];
}

/**
 * Adds the sample workflows to the library when the user asks for them, each published as `v1`,
 * through the same parse, normalization and serialization the import dialog uses. Workflows
 * already there are kept; a sample whose name is taken is skipped. Desktop's Loop examples get
 * the demo executor and no Role, so their Agent nodes show as configured.
 */
export async function addSampleWorkflows(
  contracts: WorkflowContracts,
): Promise<SampleWorkflowsResult> {
  const result: SampleWorkflowsResult = { added: [], skipped: [], unpublished: [] };
  // The list shows the newest first, so the first sample is created last.
  for (const sample of [...samples].reverse()) {
    const parsed = parseWorkflowImportFile(sample.file);
    if (!parsed.ok) continue;
    const workflow = sample.retarget
      ? withDemoExecutor(parsed.workflow)
      : parsed.workflow;
    const definition = normalizeWorkflowDocument({
      id: workflow.id,
      name: workflow.name,
      description: workflow.description,
      updatedAt: workflow.updatedAt,
      viewport: workflow.viewport,
      nodes: workflow.nodes,
      edges: workflow.edges,
      globalVariables: normalizeWorkflowGlobalVariables(workflow.globalVariables),
    });
    const graph = serializeWorkflowGraph({
      nodes: definition.nodes,
      edges: definition.edges,
      viewport: definition.viewport,
      annotations: workflow.annotations ?? [],
      globalVariables: definition.globalVariables,
      description: definition.description,
    });
    let created;
    try {
      created = await contracts.create({ name: workflow.name, graph });
    } catch (error) {
      if (error instanceof ContractError && error.code === "workflow_name_conflict") {
        result.skipped.push(workflow.name);
        continue;
      }
      throw error;
    }
    try {
      await contracts.publish({ workflowId: created.workflow.id, version: sample.version });
      result.added.push(workflow.name);
    } catch {
      result.unpublished.push(workflow.name);
    }
  }
  return result;
}

function withDemoExecutor(workflow: DemoWorkflow): DemoWorkflow {
  return {
    ...workflow,
    nodes: workflow.nodes.map((node) =>
      node.data.agentConfig === undefined
        ? node
        : {
            ...node,
            data: {
              ...node.data,
              agentConfig: {
                ...node.data.agentConfig,
                executor: demoExecutor,
                roleId: "",
              },
            },
          },
    ),
  };
}
