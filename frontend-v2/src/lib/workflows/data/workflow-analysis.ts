import { useQuery } from "@tanstack/react-query";
import { useContractsClient } from "./contracts-client";

/** Analysis fields the editor and the run workspace both project onto the canvas. */
interface WorkflowAnalysisMembership {
  unusedNodeIds: readonly string[];
  unrecognizedNodes: readonly { nodeId: string; kind: string }[];
}

/**
 * Node ids that render as excluded from execution.
 *
 * Unreachable unrecognized kinds are omitted from `unusedNodeIds`. The badge
 * still has to mark them, and an id that is already spare is not listed twice.
 */
export function excludedWorkflowNodeIds(
  analysis: WorkflowAnalysisMembership | undefined,
): string[] {
  const unused = analysis?.unusedNodeIds ?? [];
  const extra = (analysis?.unrecognizedNodes ?? [])
    .map((node) => node.nodeId)
    .filter((nodeId) => !unused.includes(nodeId));
  return [...unused, ...extra];
}

/** First-seen unrecognized kind names, joined for the status line. */
export function unrecognizedWorkflowKinds(
  analysis: WorkflowAnalysisMembership | undefined,
): string {
  return [
    ...new Set((analysis?.unrecognizedNodes ?? []).map((node) => node.kind)),
  ].join(", ");
}

/**
 * The settings of one Agent node v2-server says a run saves but does not apply, in its order.
 * Empty for any other node, and while no analysis has arrived.
 */
export function unappliedWorkflowSettings(
  analysis:
    | {
        unappliedSettings: readonly {
          nodeId: string;
          settings: readonly string[];
        }[];
      }
    | undefined,
  nodeId: string | undefined,
): readonly string[] {
  return (
    analysis?.unappliedSettings.find((entry) => entry.nodeId === nodeId)
      ?.settings ?? noSettings
  );
}

const noSettings: readonly string[] = [];

/** Document identity prevents stale responses crossing edits or workspace switches. */
export function useWorkflowAnalysis(ownerId: string, graph: string) {
  const client = useContractsClient();
  return useQuery({
    queryKey: ["workflow", "analysis", ownerId, graph],
    queryFn: ({ signal }) => client.workflow.analyze({ graph }, { signal }),
    staleTime: Infinity,
    gcTime: 0,
    retry: false,
    enabled: ownerId !== "",
    // A config edit changes the document key before the next membership
    // arrives. Keep this workflow's previous result so unused badges and the
    // status line do not blank out and repaint every card.
    placeholderData: (previousData, previousQuery) =>
      previousQuery?.queryKey[2] === ownerId ? previousData : undefined,
  });
}
