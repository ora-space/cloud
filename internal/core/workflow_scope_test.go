package core

import "testing"

func TestWorkflowAmbientSelectorsNeedDeclarationsButNotEdges(t *testing.T) {
	for _, source := range []string{"sys", "g"} {
		for _, declared := range []bool{true, false} {
			root := "missing"
			if declared {
				if source == "sys" {
					root = "workflow_id"
				} else {
					root = "items"
				}
			}
			for _, kind := range []string{"aggregator", "iteration"} {
				g := aggregatorTestGraph(Object{"variables": []any{[]any{source, root}}})
				code := "aggregator_selector_undeclared"
				if kind == "iteration" {
					g = iterationRunGraph("fail", 10, []any{"it", "item"})
					scopeTestNodeData(g, 1).O("iterationConfig")["iteratorSelector"] = []any{source, root}
					code = "iteration_iterator_undeclared"
				}
				g["globalVariables"] = []any{Object{"name": "g.items", "valueType": "array", "value": []any{1}}}
				problems := validateWorkflowGraph(decodeGraph(t, g))
				found := false
				for _, problem := range problems {
					if problem.Code == code {
						found = true
					}
				}
				if found == declared || (declared && len(problems) != 0) {
					t.Fatalf("%s %s declared=%v: %v", kind, source, declared, problems)
				}
			}
		}
	}
}

func scopeTestNodeData(g Object, index int) Object {
	node, _ := asWorkflowObject(g["nodes"].([]any)[index])
	return node.O("data")
}

func TestWorkflowIterationProductionGate(t *testing.T) {
	cases := []struct {
		name, code string
		mutate     func(Object)
	}{
		{"missing config", "iteration_config_missing", func(g Object) { delete(scopeTestNodeData(g, 1), "iterationConfig") }},
		{"over ceiling", "iteration_max_iterations_invalid", func(g Object) {
			scopeTestNodeData(g, 1).O("iterationConfig")["maxIterations"] = 101
		}},
		{"self source", "iteration_iterator_undeclared", func(g Object) {
			scopeTestNodeData(g, 1).O("iterationConfig")["iteratorSelector"] = []any{"it", "output"}
		}},
		{"non predecessor", "iteration_iterator_undeclared", func(g Object) { g["edges"] = []any{Object{"source": "it", "target": "o"}} }},
		{"empty region", "iteration_body_empty", func(g Object) { scopeTestNodeData(g, 2)["containerId"] = "" }},
		{"nested", "loop_nested_composite_unsupported", func(g Object) { scopeTestNodeData(g, 2)["kind"] = "loop" }},
		{"output member", "loop_body_output_unsupported", func(g Object) { scopeTestNodeData(g, 2)["kind"] = "output" }},
	}
	valid := iterationRunGraph("continue", 100, []any{"it", "item"})
	if problems := validateWorkflowGraph(decodeGraph(t, valid)); len(problems) > 0 {
		t.Fatalf("valid: %v", problems)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := iterationRunGraph("continue", 100, []any{"it", "item"})
			tc.mutate(g)
			for _, p := range validateWorkflowGraph(decodeGraph(t, g)) {
				if p.Code == tc.code {
					return
				}
			}
			t.Fatalf("missing %s", tc.code)
		})
	}
}

func TestWorkflowAggregatorStrictPredecessors(t *testing.T) {
	for _, source := range []string{"a1", "agg", "o"} {
		t.Run(source, func(t *testing.T) {
			g := aggregatorTestGraph(Object{"variables": []any{[]any{source, "output"}}})
			// a1 is still reachable from start but no longer an ancestor of agg.
			g["edges"] = []any{Object{"source": "s", "target": "a1"}, Object{"source": "s", "target": "a2"}, Object{"source": "a2", "target": "agg"}, Object{"source": "agg", "target": "o"}}
			for _, p := range validateWorkflowGraph(decodeGraph(t, g)) {
				if p.Code == "aggregator_selector_not_upstream" {
					return
				}
			}
			t.Fatal("accepted non-ancestor")
		})
	}
}

func TestWorkflowBodyScopeIsolation(t *testing.T) {
	for _, source := range []string{"s", "it", "other", "foreign"} {
		t.Run(source, func(t *testing.T) {
			root := "output"
			if source == "s" {
				root = "items"
			}
			if source == "it" {
				root = "item"
			}
			g := iterationRunGraph("fail", 10, []any{source, root})
			other := wfNode("other", "agent", "sibling")
			other["data"].(Object)["agentConfig"] = Object{"schemaVersion": 3, "prompt": "x"}
			foreign := wfNode("foreign", "agent", "foreign")
			foreign["data"].(Object)["containerId"] = "another"
			foreign["data"].(Object)["agentConfig"] = Object{"schemaVersion": 3, "prompt": "x"}
			g["nodes"] = append(g["nodes"].([]any), other, foreign)
			found := false
			for _, p := range validateWorkflowGraph(decodeGraph(t, g)) {
				if p.Code == "aggregator_selector_not_upstream" {
					found = true
				}
			}
			if found != (source == "other" || source == "foreign") {
				t.Fatalf("scope refusal=%v", found)
			}
		})
	}
}

func TestWorkflowLoopFeedbackReadsCarried(t *testing.T) {
	config := validLoopConfig()
	config["variables"].([]any)[0].(Object)["feedback"] = []any{"loop-1", "draft"}
	if problems := validateWorkflowGraph(decodeGraph(t, loopTestGraph(config))); len(problems) > 0 {
		t.Fatalf("carried feedback rejected: %v", problems)
	}
}
