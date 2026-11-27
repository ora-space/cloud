import { describe, expect, it } from "vitest";
import { graphHasEnabledPersonalSkills, isPersonalSkillId, reachableNodeIds } from "./personal-skills";
import type { WorkflowDefinitionEdge, WorkflowDefinitionNode, WorkflowNodeKind } from "./runtime";

describe("personal-skills", () => {
  it("distinguishes personal Skill names from marketplace plugin IDs", () => {
    expect(isPersonalSkillId("review-checklist")).toBe(true);
    expect(isPersonalSkillId("  code_review  ")).toBe(true);
    expect(isPersonalSkillId("")).toBe(false);
    expect(isPersonalSkillId("   ")).toBe(false);
    expect(isPersonalSkillId("official/mattpocock.skills")).toBe(false);
  });

  it("detects enabled personal Skills across root and container Agent nodes", () => {
    const enabledPersonal = JSON.stringify({
      nodes: [
        { id: "start", data: { kind: "start" } },
        {
          id: "writer",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: [
                { skillId: "official/mattpocock.skills", enabled: true },
                { skillId: "personal-checklist", enabled: true },
              ],
            },
          },
        },
      ],
      edges: [{ source: "start", target: "writer" }],
    });
    expect(graphHasEnabledPersonalSkills(enabledPersonal)).toBe(true);

    const disabledOrMarketOnly = JSON.stringify({
      nodes: [
        { id: "start", data: { kind: "start" } },
        {
          id: "writer",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: [
                { skillId: "official/mattpocock.skills", enabled: true },
                { skillId: "personal-checklist", enabled: false },
              ],
            },
          },
        },
      ],
      edges: [{ source: "start", target: "writer" }],
    });
    expect(graphHasEnabledPersonalSkills(disabledOrMarketOnly)).toBe(false);
    expect(graphHasEnabledPersonalSkills(undefined)).toBe(false);
    expect(graphHasEnabledPersonalSkills("not json")).toBe(false);
  });

  it("treats bindings omitting enabled as enabled and ignores malformed spare node skill configs", () => {
    const spareWithInvalidAndOmittedEnabled = JSON.stringify({
      nodes: [
        { id: "start", data: { kind: "start" } },
        {
          id: "spare-object-skills",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: {},
            },
          },
        },
        {
          id: "spare-malformed-entries",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: [null, {}, { skillId: 123, enabled: true }, { skillId: "personal-disabled", enabled: false }],
            },
          },
        },
      ],
      edges: [],
    });
    expect(graphHasEnabledPersonalSkills(spareWithInvalidAndOmittedEnabled)).toBe(false);

    const omittedEnabled = JSON.stringify({
      nodes: [
        {
          id: "spare-object-skills",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: {},
            },
          },
        },
        {
          id: "writer",
          data: {
            kind: "agent",
            agentConfig: {
              prompt: "Draft",
              skills: [{ skillId: "implicit-enabled-skill" }],
            },
          },
        },
      ],
      edges: [],
    });
    expect(graphHasEnabledPersonalSkills(omittedEnabled)).toBe(true);
  });


  // A typed node builder: the graph the server checks is the one the editor serializes.
  const node = (id: string, kind: WorkflowNodeKind, data: Record<string, unknown> = {}, parent?: string): WorkflowDefinitionNode => ({
    id,
    type: "workflow",
    position: { x: 0, y: 0 },
    ...(parent ? { parentId: parent } : {}),
    data: { kind, title: id, description: "", ...(parent ? { containerId: parent } : {}), ...data },
  });
  const agentWith = (id: string, skillId: string, enabled = true, parent?: string) =>
    node(id, "agent", { agentConfig: { prompt: "Draft", skills: [{ skillId, enabled }] } }, parent);
  const edge = (source: string, target: string, sourceHandle?: string): WorkflowDefinitionEdge => ({
    id: `${source}-${target}`,
    source,
    target,
    ...(sourceHandle ? { sourceHandle } : {}),
  });

  it("ignores spare Agent nodes the root Start cannot reach, as the server's preflight does", () => {
    const start = node("start", "start");
    // A published spare Agent keeps unfinished configuration; the hint must not count it.
    expect(
      graphHasEnabledPersonalSkills(JSON.stringify({ nodes: [start, agentWith("spare", "spare-checklist")], edges: [] })),
    ).toBe(false);

    // The same Agent wired in from the Start counts, and the spare one still does not.
    expect(
      graphHasEnabledPersonalSkills(
        JSON.stringify({
          nodes: [start, agentWith("writer", "personal-checklist"), agentWith("spare", "spare-checklist")],
          edges: [edge("start", "writer")],
        }),
      ),
    ).toBe(true);

    // Reachable Agents on branches this run would never take still count, as on the server.
    const cond = node("cond", "condition");
    expect(
      graphHasEnabledPersonalSkills(
        JSON.stringify({
          nodes: [start, cond, agentWith("yes", "yes-checklist"), agentWith("no", "official/plugin")],
          edges: [edge("start", "cond"), edge("cond", "yes", "yes"), edge("cond", "no", "else")],
        }),
      ),
    ).toBe(true);

    // A Loop body is entered through its own Start, so a member the root graph has no edge to counts.
    const loop = node("loop", "loop");
    const bodyStart = node("loop-start", "start", {}, "loop");
    expect(
      graphHasEnabledPersonalSkills(
        JSON.stringify({
          nodes: [start, loop, bodyStart, agentWith("member", "member-checklist", true, "loop"), agentWith("spare", "spare-checklist")],
          edges: [edge("start", "loop"), edge("loop-start", "member")],
        }),
      ),
    ).toBe(true);
    // The same body with the member's Skill disabled, or with a Loop nothing reaches, does not.
    expect(
      graphHasEnabledPersonalSkills(
        JSON.stringify({
          nodes: [start, loop, bodyStart, agentWith("member", "member-checklist", false, "loop")],
          edges: [edge("start", "loop"), edge("loop-start", "member")],
        }),
      ),
    ).toBe(false);
    expect(
      graphHasEnabledPersonalSkills(
        JSON.stringify({
          nodes: [start, loop, bodyStart, agentWith("member", "member-checklist", true, "loop")],
          edges: [edge("loop-start", "member")],
        }),
      ),
    ).toBe(false);
  });

  it("lists only the nodes a run reaches, and everything when there is no root Start", () => {
    const nodes = [
      node("start", "start"),
      node("a", "agent"),
      node("b", "output"),
      node("spare", "agent"),
      node("loop", "loop"),
      node("loop-start", "start", {}, "loop"),
      node("member", "agent", {}, "loop"),
    ];
    const edges = [edge("start", "a"), edge("a", "b"), edge("start", "loop"), edge("loop-start", "member"), edge("spare", "start")];
    // Edges are followed forward only: the spare node's edge into Start does not make it reachable.
    expect([...reachableNodeIds(nodes, edges)].sort()).toEqual(["a", "b", "loop", "loop-start", "member", "start"]);
    // Without a root Start the server checks the whole graph; so does the hint.
    expect([...reachableNodeIds(nodes.slice(1), edges)].sort()).toEqual(["a", "b", "loop", "loop-start", "member", "spare"]);
  });
});
