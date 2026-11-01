import { ContractError } from "../contracts";
import type { WorkflowContracts } from "./contracts-client";
import {
  LocalWorkflowBackend,
  browserWorkflowStorage,
  type WorkflowBackendStorage,
} from "./local-workflow-backend";

/** Set once the user has imported or dismissed this browser's old library, so it is offered once. */
const HANDLED_KEY = "ora.v2.workflows.legacy-handled";

/** One workflow the browser kept before v2-server stored workflows: its name and draft graph. */
export interface LegacyWorkflow {
  name: string;
  graph: string;
}

/** Whether the old library still waits for the user's answer; it never does once answered. */
export interface LegacyLibraryStorage extends WorkflowBackendStorage {
  handled(): boolean;
  markHandled(): void;
}

export const browserLegacyLibraryStorage: LegacyLibraryStorage = {
  ...browserWorkflowStorage,
  handled() {
    try {
      return window.localStorage.getItem(HANDLED_KEY) !== null;
    } catch {
      return true;
    }
  },
  markHandled() {
    try {
      window.localStorage.setItem(HANDLED_KEY, "1");
    } catch {
      // Without storage the offer comes back next visit; importing again only skips taken names.
    }
  },
};

/**
 * The workflows this browser kept locally, newest first, unless the user already answered the
 * offer. Only each draft is carried over: the old published versions lived in this browser and
 * are not replayed onto the server.
 */
export function readLegacyWorkflows(
  storage: LegacyLibraryStorage = browserLegacyLibraryStorage,
): LegacyWorkflow[] {
  if (storage.handled()) return [];
  const backend = new LocalWorkflowBackend({ storage });
  return backend.list().workflows.map((workflow) => ({
    name: workflow.name,
    graph: backend.getDraft({ workflowId: workflow.id }).snapshot.graph,
  }));
}

/** What importing did to each old workflow, by name. */
export interface LegacyImportResult {
  imported: string[];
  /** A workflow of the same name is already on the server; the old one is left out. */
  skipped: string[];
}

/**
 * Creates each old workflow on the server with its draft graph, oldest first so the library keeps
 * their order. A taken name is skipped rather than renamed; any other refusal stops the import and
 * leaves the offer open, so nothing is silently dropped.
 */
export async function importLegacyWorkflows(
  contracts: WorkflowContracts,
  workflows: LegacyWorkflow[],
  storage: LegacyLibraryStorage = browserLegacyLibraryStorage,
): Promise<LegacyImportResult> {
  const result: LegacyImportResult = { imported: [], skipped: [] };
  for (const workflow of [...workflows].reverse()) {
    try {
      await contracts.create({ name: workflow.name, graph: workflow.graph });
      result.imported.push(workflow.name);
    } catch (error) {
      if (error instanceof ContractError && error.code === "workflow_name_conflict") {
        result.skipped.push(workflow.name);
        continue;
      }
      throw error;
    }
  }
  storage.markHandled();
  return result;
}
