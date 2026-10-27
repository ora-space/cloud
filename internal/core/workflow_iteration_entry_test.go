package core

import (
	"context"
	"reflect"
	"testing"
)

// iterationEntryGraph preserves the editor/Desktop wire shape: parentId membership and an
// optional iteration-entry edge. Two members make their ordering and per-round scope observable.
func iterationEntryGraph() Object {
	graph := iterationRunGraph("fail", 10, []any{"it", "item"})
	nodes := graph["nodes"].([]any)
	member, _ := asWorkflowObject(nodes[2])
	delete(member.O("data"), "containerId")
	member["parentId"] = "it"
	next := wfNode("next", "aggregator", "next")
	next["parentId"] = "it"
	next.O("data")["aggregatorConfig"] = Object{"variables": []any{[]any{"agg", "output"}}}
	nodes = append(nodes, next)
	iteration, _ := asWorkflowObject(nodes[1])
	iteration.O("data").O("iterationConfig")["collectSelector"] = []any{"next", "output"}
	graph["nodes"] = nodes
	graph["edges"] = append(graph["edges"].([]any), Object{"source": "agg", "target": "next"})
	return graph
}

func TestWorkflowIterationEntryPublication(t *testing.T) {
	cases := []struct {
		name   string
		edge   Object
		mutate func(Object)
		want   []workflowGraphProblem
	}{
		{name: "entry edge optional"},
		{name: "own member", edge: Object{"source": "it", "sourceHandle": "iteration-entry", "target": "agg"}},
		{
			name: "explicit container takes precedence", edge: Object{"source": "it", "sourceHandle": "iteration-entry", "target": "agg"},
			mutate: func(graph Object) {
				member, _ := asWorkflowObject(graph["nodes"].([]any)[2])
				member["parentId"] = "other"
				member.O("data")["containerId"] = "it"
			},
		},
		{
			name: "wrong handle", edge: Object{"source": "it", "sourceHandle": "other", "target": "agg"},
			want: []workflowGraphProblem{{Code: "loop_cross_container_edge", NodeID: "it"}},
		},
		{
			name: "missing handle", edge: Object{"source": "it", "target": "agg"},
			want: []workflowGraphProblem{{Code: "loop_cross_container_edge", NodeID: "it"}},
		},
		{
			name: "external node cannot forge entry", edge: Object{"source": "s", "sourceHandle": "iteration-entry", "target": "agg"},
			want: []workflowGraphProblem{{Code: "loop_cross_container_edge", NodeID: "s"}},
		},
		{
			name: "member cannot escape", edge: Object{"source": "agg", "sourceHandle": "iteration-entry", "target": "o"},
			want: []workflowGraphProblem{{Code: "loop_cross_container_edge", NodeID: "agg"}},
		},
		{
			name: "another iteration cannot enter", edge: Object{"source": "other", "sourceHandle": "iteration-entry", "target": "agg"},
			mutate: func(graph Object) {
				// Structure-only checking isolates the ownership rule from the other node's config.
				graph["nodes"] = append(graph["nodes"].([]any), wfNode("other", "iteration", "other"))
			},
			want: []workflowGraphProblem{{Code: "loop_cross_container_edge", NodeID: "other"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			graph := iterationEntryGraph()
			if tc.edge != nil {
				graph["edges"] = append(graph["edges"].([]any), tc.edge)
			}
			if tc.mutate != nil {
				tc.mutate(graph)
			}
			graph = decodeGraph(t, graph)
			before := decodeGraph(t, graph)
			problems := validateWorkflowContainers(graph["nodes"].([]any), graph["edges"].([]any))
			if len(problems) != len(tc.want) || (len(problems) > 0 && !reflect.DeepEqual(problems, tc.want)) {
				t.Fatalf("container problems = %+v, want %+v", problems, tc.want)
			}
			if len(tc.want) == 0 {
				if problems := validateWorkflowGraph(graph); len(problems) != 0 {
					t.Fatalf("valid editor graph rejected: %+v", problems)
				}
			}
			if !reflect.DeepEqual(graph, before) {
				t.Fatal("publication rewrote the graph instead of preserving its wire shape")
			}
		})
	}
}

func TestWorkflowIterationEntryDoesNotBecomeADependency(t *testing.T) {
	graph := iterationEntryGraph()
	graph["edges"] = append(graph["edges"].([]any), Object{"source": "it", "sourceHandle": "iteration-entry", "target": "agg"})
	view := readWorkflowGraph(decodeGraph(t, graph))
	body := workflowBodyGraph(view, "it")
	projection := projectWorkflowNodes(body, map[string]workflowNodeState{"it": {Status: "running"}})
	ready, ok := projection.readyNode()
	if !ok || ready.ID != "agg" {
		t.Fatalf("entry must be ready while its owner runs, got %+v / %v", ready, ok)
	}
	want := map[string]bool{"agg": true}
	if got := workflowAncestorIDs(view, "next"); !reflect.DeepEqual(got, want) {
		t.Fatalf("runtime predecessors = %v, want %v", got, want)
	}
	if got := workflowRegionAncestors(workflowNodeContainers(graph["nodes"].([]any)), graph["edges"].([]any), "next"); !reflect.DeepEqual(got, want) {
		t.Fatalf("publication predecessors = %v, want %v", got, want)
	}
}

func TestWorkflowIterationEntryRunsWithAndWithoutEdge(t *testing.T) {
	for _, withEntry := range []bool{false, true} {
		name := "without entry"
		if withEntry {
			name = "with entry"
		}
		t.Run(name, func(t *testing.T) {
			graph := iterationEntryGraph()
			if withEntry {
				graph["edges"] = append(graph["edges"].([]any), Object{"source": "it", "sourceHandle": "iteration-entry", "target": "agg"})
			}
			if problems := validateWorkflowGraph(decodeGraph(t, graph)); len(problems) != 0 {
				t.Fatalf("publish: %+v", problems)
			}
			store := dispatcherDB(t)
			items := []any{"first", "second"}
			runID := seedWorkflowRunSceneWithInput(t, store, graph, Object{"items": items})
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatalf("advance: %v", err)
			}
			status, runErr, states, version := readWorkflowRun(t, store, runID)
			if status != "succeeded" || runErr != "" {
				t.Fatalf("run = %s / %s, states = %v", status, runErr, states)
			}
			iteration := states.O("it")
			wantEntries := []any{
				map[string]any{"item": "first", "status": "succeeded", "output": "first", "error": nil},
				map[string]any{"item": "second", "status": "succeeded", "output": "second", "error": nil},
			}
			want := Object{"output": items, "entries": wantEntries, "failed_count": float64(0)}
			if got := iteration.O("output"); !reflect.DeepEqual(got, want) {
				t.Fatalf("output = %#v, want %#v", got, want)
			}
			completed, _ := iteration.O("iteration")["completed"].([]any)
			if len(completed) != 2 {
				t.Fatalf("round archives = %v, want two", completed)
			}
			for i, raw := range completed {
				round, _ := asWorkflowObject(raw)
				for _, id := range []string{"agg", "next"} {
					member := round.O("states").O(id)
					if member.S("status") != "succeeded" || member.O("output")["output"] != items[i] {
						t.Fatalf("round %d member %s = %v", i, id, member)
					}
				}
			}
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatalf("replay: %v", err)
			}
			_, _, replay, replayVersion := readWorkflowRun(t, store, runID)
			if replayVersion != version || !reflect.DeepEqual(replay, states) {
				t.Fatal("terminal replay changed the committed rounds")
			}
		})
	}
}
