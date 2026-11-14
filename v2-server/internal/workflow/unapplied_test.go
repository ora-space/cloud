package workflow_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/workflow"
)

// placedAgents builds one graph holding Agents at every placement the run path distinguishes:
// the root graph, an Iteration region and a Loop body, each reachable and spare, each with every
// setting variant that can run there. With wire set, the spare Agents are connected so the run path can view them
// with the context they are authored in.
func placedAgents(wire bool) (obj, []string) {
	variants := map[string]obj{
		"retry":             {"retry": obj{"enabled": true, "maxRetries": 2, "initialDelaySeconds": 5}},
		"role":              {"roleId": "reviewer"},
		"interactive":       {"interactive": true},
		"mcp":               {"mcps": []any{obj{"mcpId": "official/tools", "enabled": true}}},
		"interactive-retry": {"interactive": true, "retry": obj{"enabled": true, "maxRetries": 1, "initialDelaySeconds": 1}},
		"plain":             {},
	}
	g := obj{"schemaVersion": 2, "nodes": []any{
		obj{"id": "start", "data": obj{"kind": "start", "inputVariables": []any{obj{"name": "items", "valueType": "array[string]"}}}},
		obj{"id": "iter", "data": obj{"kind": "iteration", "iterationConfig": obj{
			"iteratorSelector": []any{"start", "items"}, "collectSelector": []any{"iter-plain", "output"}}}},
		obj{"id": "loop", "data": obj{"kind": "loop", "loopConfig": obj{"maxIterations": 3, "variables": []any{},
			"until": obj{"logic": "and", "conditions": []any{}}, "outputs": []any{}}}},
		obj{"id": "entry", "parentId": "loop", "data": obj{"kind": "start", "containerId": "loop"}},
	}, "edges": []any{obj{"source": "start", "target": "iter"}, obj{"source": "iter", "target": "loop"}}}
	ids := []string{}
	for _, at := range []string{"root", "iter", "loop"} {
		for variant, config := range variants {
			if at == "iter" && config["interactive"] == true {
				continue // Publication rejects interactive Iteration members, so no run path exists.
			}
			for _, spare := range []bool{false, true} {
				id := at + "-" + variant
				if spare {
					id += "-spare"
				}
				ids = append(ids, id)
				agent := obj{"prompt": id}
				for k, v := range config {
					agent[k] = v
				}
				data := obj{"kind": "agent", "agentConfig": agent}
				n := obj{"id": id, "data": data}
				from := map[string]string{"root": "start", "iter": "iter", "loop": "entry"}[at]
				switch at {
				case "iter":
					n["parentId"] = "iter"
				case "loop":
					n["parentId"] = "loop"
					data["containerId"] = "loop"
				}
				g["nodes"] = append(g["nodes"].([]any), n)
				if !spare || wire {
					connect(g, from, id)
				}
			}
		}
	}
	return g, ids
}

// runPathView returns the node view the engine executes for id: the root graph holds root and
// Iteration-region Agents, the Loop body graph holds Loop members.
func runPathView(t *testing.T, g *workflow.Graph, id string) workflow.Node {
	t.Helper()
	if strings.HasPrefix(id, "loop-") {
		body, ok := g.LoopBody("loop")
		if !ok {
			t.Fatal("Loop body missing from the run graph")
		}
		g = body
	}
	n, ok := g.Node(id)
	if !ok {
		t.Fatalf("node %s missing from its run graph", id)
	}
	return n
}

// The analysis must report, for every Agent wherever it sits, exactly what the run path records
// as unapplied. Both sides are computed, never spelled out, so the test keeps holding when the
// rule changes (for instance when retry starts depending on the node's container).
func TestAnalysisMatchesRunPathForEveryPlacement(t *testing.T) {
	authored, ids := placedAgents(false)
	analysis, err := workflow.AnalyzeWorkflow(encode(t, authored))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]string{}
	for _, entry := range analysis.UnappliedSettings {
		got[entry.NodeID] = entry.Settings
	}
	wired, _ := placedAgents(true)
	run, err := workflow.ParseGraph(encode(t, wired))
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, id := range ids {
		want := runPathView(t, run, id).Agent().Unapplied
		if len(want) == 0 {
			want = nil
		} else {
			listed++
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Errorf("node %s: analysis lists %v, the run records %v", id, got[id], want)
		}
	}
	if len(got) != listed {
		t.Errorf("analysis lists %d Agents, the run path %d: %v", len(got), listed, got)
	}
	if listed == 0 {
		t.Fatal("no setting is unapplied any more; the comparison checks nothing")
	}
}

// The analysis must hand the rule the same configuration and placement the run path does, so a
// context-dependent rule cannot diverge. A probe rule echoes its inputs to make that visible
// even while today's rule ignores the placement.
func TestAnalysisFeedsTheRuleTheRunPathInputs(t *testing.T) {
	authored, ids := placedAgents(false)
	got, err := workflow.ProbeAnalysis(encode(t, authored))
	if err != nil {
		t.Fatal(err)
	}
	wired, _ := placedAgents(true)
	run, err := workflow.ParseGraph(encode(t, wired))
	if err != nil {
		t.Fatal(err)
	}
	contextual := 0
	for _, id := range ids {
		want := workflow.ProbeRunPath(runPathView(t, run, id))
		at := map[string][]string{"root": {"region=", "loop="}, "iter": {"region=iter", "loop="}, "loop": {"region=", "loop=loop"}}[strings.SplitN(id, "-", 2)[0]]
		if !reflect.DeepEqual(want[1:], at) {
			t.Errorf("node %s: the run path places it at %v, want %v", id, want[1:], at)
		}
		if at[0] != "region=" || at[1] != "loop=" {
			contextual++
		}
		if !reflect.DeepEqual(got[id], want) {
			t.Errorf("node %s: analysis passes the rule %v, the run path passes %v", id, got[id], want)
		}
	}
	if len(got) != len(ids) {
		t.Errorf("analysis probed %d Agents, want all %d: %v", len(got), len(ids), got)
	}
	if contextual == 0 {
		t.Fatal("no Agent sits in a container; the placement comparison checks nothing")
	}
}

// The rule follows desktop's effective retry policy (retry.rs:66-74, 96-99 at ebd5b725): an absent
// policy is the default, enabled with two retries, and an interactive node never retries. It lists settings in the payload's fixed order,
// and counts an MCP binding without an enabled flag as enabled, as AgentConfig.MCPs does. Each
// case is checked on the analysis and on the run path for a root Agent.
func TestUnappliedAgentSettingsRule(t *testing.T) {
	off := obj{"enabled": false, "maxRetries": 2, "initialDelaySeconds": 5}
	for name, tc := range map[string]struct {
		config obj
		want   []string
		// spare marks a draft-only configuration publication rejects; only the analysis sees it.
		spare bool
	}{
		"absent retry is desktop's default":   {obj{}, []string{"retry"}, false},
		"null retry is desktop's default":     {obj{"retry": nil}, []string{"retry"}, false},
		"enabled retry":                       {obj{"retry": obj{"enabled": true, "maxRetries": 2, "initialDelaySeconds": 5}}, []string{"retry"}, false},
		"disabled retry":                      {obj{"retry": off}, nil, false},
		"enabled retry with no budget":        {obj{"retry": obj{"enabled": true, "maxRetries": 0, "initialDelaySeconds": 5}}, nil, false},
		"mcp binding without an enabled flag": {obj{"retry": off, "mcps": []any{obj{"mcpId": "official/tools"}}}, []string{"mcp"}, true},
		"disabled mcp binding":                {obj{"retry": off, "mcps": []any{obj{"mcpId": "official/tools", "enabled": false}}}, nil, false},
		// Desktop never retries an interactive node (node_runtime.rs:373-379), so retry is not listed.
		"every setting, interactive": {obj{"retry": obj{"enabled": true, "maxRetries": 1, "initialDelaySeconds": 1},
			"interactive": true, "mcps": []any{obj{"mcpId": "official/tools", "enabled": true}}, "roleId": "reviewer"},
			[]string{"role", "mcp", "interactive"}, false},
		"interactive without a retry policy": {obj{"interactive": true}, []string{"interactive"}, false},
		"every setting but interactive, in the payload order": {obj{"retry": obj{"enabled": true, "maxRetries": 1, "initialDelaySeconds": 1},
			"mcps": []any{obj{"mcpId": "official/tools", "enabled": true}}, "roleId": "reviewer"},
			[]string{"role", "mcp", "retry"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.config["prompt"] = "x"
			g := flat()
			addNode(g, "a", "agent", obj{"agentConfig": tc.config})
			if !tc.spare {
				connect(g, "start", "a")
			}
			source := encode(t, g)
			analysis, err := workflow.AnalyzeWorkflow(source)
			if err != nil {
				t.Fatal(err)
			}
			var listed []string
			for _, entry := range analysis.UnappliedSettings {
				listed = entry.Settings
			}
			if tc.spare {
				if !reflect.DeepEqual(listed, tc.want) {
					t.Fatalf("analysis lists %v; want %v", listed, tc.want)
				}
				return
			}
			run, err := workflow.ParseGraph(source)
			if err != nil {
				t.Fatal(err)
			}
			node, _ := run.Node("a")
			if !reflect.DeepEqual(listed, tc.want) || !reflect.DeepEqual(node.Agent().Unapplied, tc.want) {
				t.Fatalf("analysis lists %v, run records %v; want %v", listed, node.Agent().Unapplied, tc.want)
			}
		})
	}
}
