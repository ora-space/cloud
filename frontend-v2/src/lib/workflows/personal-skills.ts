import { useQuery } from "@tanstack/react-query";
import { getWorkflow, getWorkflowSnapshot } from "./api/generated";
import {
  parseWorkflowGraph,
  type WorkflowDefinitionEdge,
  type WorkflowDefinitionNode,
} from "./runtime";

/**
 * Team visibility warning shown wherever a workflow referencing personal Skills is configured or
 * launched: v2 resolves personal Skill names against the run starter's catalog, and node sessions
 * are readable by the whole workspace.
 */
export const WORKFLOW_PERSONAL_SKILL_TEAM_VISIBILITY_HINT =
  "运行时读取发起人同名的个人 Skill，其内容在节点会话中对团队可见。";

/** Reports whether a Skill identifier refers to a personal Skill (plain name, not `namespace/id`). */
export function isPersonalSkillId(skillId: string): boolean {
  const trimmed = skillId.trim();
  return trimmed !== "" && !trimmed.includes("/");
}

/** The parts of a workflow graph the Skill check needs. */
export interface WorkflowGraphNodes {
  nodes: readonly WorkflowDefinitionNode[];
  edges: readonly WorkflowDefinitionEdge[];
}

/**
 * Lists the nodes a run would execute, as the server's start preflight does (`workflow.SkillReferences`
 * over the graph `project` keeps): everything downstream of the root Start, entering each Loop body
 * through its own Start. Publication lets an unconnected Agent keep unfinished configuration, so
 * such a spare node must not count. Without a root Start every node counts, as on the server.
 */
export function reachableNodeIds(nodes: readonly WorkflowDefinitionNode[], edges: readonly WorkflowDefinitionEdge[]): Set<string> {
  const byId = new Map(nodes.map((node) => [node.id, node]));
  const outgoing = new Map<string, string[]>();
  for (const edge of edges) {
    const targets = outgoing.get(edge.source);
    if (targets) targets.push(edge.target);
    else outgoing.set(edge.source, [edge.target]);
  }
  const rootStart = nodes.find(
    (node) => node.data.kind === "start" && !node.parentId && !node.data.containerId,
  );
  const reachable = new Set<string>();
  if (!rootStart) {
    for (const node of nodes) reachable.add(node.id);
    return reachable;
  }
  const frontier = [rootStart.id];
  while (frontier.length > 0) {
    const id = frontier.pop()!;
    if (reachable.has(id)) continue;
    reachable.add(id);
    if (byId.get(id)?.data.kind === "loop") {
      const entry = nodes.find(
        (node) => node.data.kind === "start" && (node.parentId === id || node.data.containerId === id),
      );
      if (entry) frontier.push(entry.id);
    }
    frontier.push(...(outgoing.get(id) ?? []));
  }
  return reachable;
}

/**
 * Reports whether any Agent node a run of `graph` would execute enables at least one personal
 * Skill. Spare (unreachable) Agent nodes are skipped like the server does; malformed `skills`
 * configurations are ignored so a published spare node never breaks the hint.
 */
export function hasEnabledPersonalSkills(graph: WorkflowGraphNodes): boolean {
  const reachable = reachableNodeIds(graph.nodes, graph.edges);
  for (const node of graph.nodes) {
    if (node.data.kind !== "agent" || !reachable.has(node.id)) continue;
    const skills: unknown = node.data.agentConfig?.skills;
    if (!Array.isArray(skills)) continue;
    for (const binding of skills) {
      if (
        binding !== null &&
        typeof binding === "object" &&
        "skillId" in binding &&
        typeof binding.skillId === "string" &&
        (!("enabled" in binding) || binding.enabled !== false) &&
        isPersonalSkillId(binding.skillId)
      ) {
        return true;
      }
    }
  }
  return false;
}

/**
 * Safely parses a serialized workflow graph and reports whether any Agent node a run would
 * execute enables a personal Skill. Malformed or empty graphs return `false` so launch surfaces
 * never break on hint rendering.
 */
export function graphHasEnabledPersonalSkills(graph: string | undefined | null): boolean {
  if (!graph) return false;
  try {
    const { nodes, edges } = parseWorkflowGraph(graph);
    return hasEnabledPersonalSkills({ nodes, edges });
  } catch {
    return false;
  }
}

/**
 * Checks whether the graph a launch will run enables any personal Skill: the published graph by
 * default, or the snapshot `snapshotId` names when the caller pins one (a schedule fixed to a
 * version keeps running that snapshot after later publications). Failures fall back to `false` so
 * non-blocking visibility copy never blocks launching or configuring a workflow.
 */
export function useWorkflowHasPersonalSkills(
  tenantId: string | undefined,
  workflowId: string | undefined,
  snapshotId?: string,
): boolean {
  const pinned = snapshotId || undefined;
  const query = useQuery({
    queryKey: ["workflow-personal-skills", tenantId, workflowId, pinned],
    enabled: Boolean(tenantId && workflowId),
    staleTime: 60_000,
    retry: false,
    queryFn: async ({ signal }) => {
      try {
        if (pinned) {
          const { snapshot } = await getWorkflowSnapshot(tenantId!, workflowId!, pinned, undefined, signal);
          return graphHasEnabledPersonalSkills(snapshot?.graph);
        }
        const detail = await getWorkflow(tenantId!, workflowId!, undefined, signal);
        return graphHasEnabledPersonalSkills(detail?.published?.graph);
      } catch {
        return false;
      }
    },
  });
  return query.data === true;
}
