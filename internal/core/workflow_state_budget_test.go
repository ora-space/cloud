package core

import (
	"strings"
	"testing"
)

func TestWorkflowStateBudgetExactJSONBytes(t *testing.T) {
	graph := readWorkflowGraph(iterationRunGraph("continue", 100, []any{"it", "item"}))
	for _, text := range []string{strings.Repeat("x", workflowLocalStateLimit-2), strings.Repeat("汉", 349524) + "aa", strings.Repeat("\n", 524287)} {
		if len(jsonText(text)) != workflowLocalStateLimit {
			t.Fatal("fixture must hit exact encoded boundary")
		}
		states := map[string]workflowNodeState{"it": {Status: "running", Iteration: &workflowIterationRunState{Items: []any{text}}}}
		if over := workflowStateBudget(graph, states); over != nil {
			t.Fatalf("boundary refused: %+v", over)
		}
		states["it"].Iteration.Items[0] = text + "x"
		if over := workflowStateBudget(graph, states); over == nil || over.reason != "iteration_state_size_exceeded" || over.bytes != workflowLocalStateLimit+1 {
			t.Fatalf("oversized UTF-8/escaped input accepted: %+v", over)
		}
	}
}

func TestWorkflowStateBudgetLocalObligations(t *testing.T) {
	large := strings.Repeat("x", workflowLocalStateLimit)
	cases := []struct {
		name  string
		graph Object
		state workflowNodeState
	}{
		{"collect", iterationRunGraph("continue", 100, []any{"it", "item"}), workflowNodeState{Iteration: &workflowIterationRunState{Output: []any{large}}}},
		{"iteration archive", iterationRunGraph("continue", 100, []any{"it", "item"}), workflowNodeState{Iteration: &workflowIterationRunState{Completed: []any{Object{"index": 0, "states": Object{"agg": Object{"output": large}}}}}}},
		{"loop initial", loopTestGraph(validLoopConfig()), workflowNodeState{Loop: &workflowLoopRunState{Carried: map[string]any{"draft": large}}}},
		{"loop archive", loopTestGraph(validLoopConfig()), workflowNodeState{Loop: &workflowLoopRunState{Completed: []any{Object{"index": 1, "carried": Object{"draft": large}, "states": Object{}}}}}},
		{"loop outputs", loopTestGraph(validLoopConfig()), workflowNodeState{Status: "succeeded", Output: Object{"result": large}, Loop: &workflowLoopRunState{Carried: map[string]any{}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := readWorkflowGraph(tc.graph)
			id := "it"
			reason := "iteration_state_size_exceeded"
			if tc.state.Loop != nil {
				id = "loop-1"
				reason = "loop_state_size_exceeded"
			}
			if over := workflowStateBudget(g, map[string]workflowNodeState{id: tc.state}); over == nil || over.reason != reason {
				t.Fatalf("missing local budget: %+v", over)
			}
		})
	}
}

func TestWorkflowTotalBudgetCountsRepeatedValues(t *testing.T) {
	g := readWorkflowGraph(iterationRunGraph("continue", 100, []any{"it", "item"}))
	states := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"items": strings.Repeat("x", workflowStateLimit-workflowFailureReserve-1000)}}}
	states["it"] = workflowNodeState{Status: "running", Iteration: &workflowIterationRunState{Items: []any{strings.Repeat("x", 500)}}}
	if over := workflowStateBudget(g, states); over != nil {
		t.Fatalf("under budget: %+v", over)
	}
	s := states["it"]
	s.Iteration.Output = []any{strings.Repeat("x", 500)}
	s.Output = Object{"output": s.Iteration.Output}
	states["it"] = s
	if over := workflowStateBudget(g, states); over == nil || over.reason != "workflow_state_size_exceeded" {
		t.Fatalf("duplicate output was not counted: %+v", over)
	}
}

func TestWorkflowIterationRejectsCorruptRecovery(t *testing.T) {
	g := readWorkflowGraph(iterationRunGraph("continue", 10, []any{"it", "item"}))
	node := g.ByNode["it"]
	valid := Object{"round": 1, "items": []any{"a", "b"}, "strategy": "continue", "ledger": []any{Object{"item": "a", "status": "succeeded", "output": "a", "error": nil}}, "output": []any{"a"}, "failedCount": 0, "completed": []any{Object{"index": 0, "states": Object{"agg": Object{"status": "succeeded", "output": Object{"output": "a"}}}}}}
	config, err := parseWorkflowIterationConfig(node.Data)
	if err != nil || !validWorkflowIterationProgress(workflowIterationRunStateFromJSON(decodeGraph(t, valid)), &config, g, node.ID) {
		t.Fatalf("valid recovery rejected: %v", err)
	}
	mutations := map[string]func(Object){
		"derived output": func(raw Object) { raw["output"] = []any{"wrong"} },
		"failed count":   func(raw Object) { raw["failedCount"] = 1 },
		"archive index":  func(raw Object) { archive, _ := asWorkflowObject(raw["completed"].([]any)[0]); archive["index"] = 1 },
		"archive contradicts ledger": func(raw Object) {
			archive, _ := asWorkflowObject(raw["completed"].([]any)[0])
			member, _ := asWorkflowObject(archive.O("states")["agg"])
			member["status"] = "failed"
		},
		"invented failure": func(raw Object) {
			entry, _ := asWorkflowObject(raw["ledger"].([]any)[0])
			entry["status"] = "failed"
			entry["output"] = nil
			entry["error"] = "invented"
			raw["output"] = []any{}
			raw["failedCount"] = 1
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			raw := decodeGraph(t, valid)
			mutate(raw)
			if validWorkflowIterationProgress(workflowIterationRunStateFromJSON(raw), &config, g, node.ID) {
				t.Fatal("inconsistent recovery accepted")
			}
		})
	}
	for _, field := range []string{"round", "items", "ledger", "output", "failedCount", "completed", "strategy"} {
		t.Run(field, func(t *testing.T) {
			raw := decodeGraph(t, valid)
			delete(raw, field)
			states := map[string]workflowNodeState{"it": {Status: "running", Iteration: workflowIterationRunStateFromJSON(raw)}}
			advanceWorkflowIteration(nil, Object{}, node, 1, states, nil, g)
			if states["it"].Output.S("reason") != "iteration_invalid_round" {
				t.Fatalf("accepted missing %s", field)
			}
		})
	}
	for _, round := range []any{-1, 2, 1.5} {
		raw := decodeGraph(t, valid)
		raw["round"] = round
		states := map[string]workflowNodeState{"it": {Status: "running", Iteration: workflowIterationRunStateFromJSON(raw)}}
		advanceWorkflowIteration(nil, Object{}, node, 1, states, nil, g)
		if states["it"].Output.S("reason") != "iteration_invalid_round" {
			t.Fatalf("accepted round %v", round)
		}
	}
}
