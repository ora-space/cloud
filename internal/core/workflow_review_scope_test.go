package core

import "testing"

func TestWorkflowStartSelectionUsesParentFallback(t *testing.T) {
	body := Object{"id": "body", "parentId": "l", "data": Object{"kind": "start", "input": "body", "inputVariables": []any{Object{"name": "snapshotId", "valueType": "string"}}}}
	outer := Object{"id": "outer", "data": Object{"kind": "start", "input": "outer"}}
	nodes := decodeGraph(t, Object{"nodes": []any{body, outer}})["nodes"].([]any)
	if got := workflowStartNode(Object{"nodes": nodes}); got.S("input") != "outer" {
		t.Fatal("body start became kickoff form")
	}
	if problems := validateWorkflowReservedFields(nodes, nil); len(problems) != 0 {
		t.Fatalf("body variable incorrectly entered outer form: %v", problems)
	}
}

func TestWorkflowOuterConditionKeepsDeclaredNonAncestorCompatibility(t *testing.T) {
	condition := graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"a", "output"}, "equals", "ok")})})
	g := workflowGraph([]any{graphStart("s"), condition, Object{"id": "a", "data": Object{"kind": "agent", "agentConfig": Object{"schemaVersion": 3, "prompt": "work"}}}}, []any{graphEdge("s", "c"), graphEdge("s", "a")}, nil)
	for _, problem := range validateWorkflowGraph(decodeGraph(t, g)) {
		if problem.Code == "condition_selector_undeclared" {
			t.Fatalf("outer condition acquired a new predecessor gate: %v", problem)
		}
	}
}

func TestWorkflowLoopRecoveryRejectsInconsistentArchivePrefix(t *testing.T) {
	g := readWorkflowGraph(loopTestGraph(validLoopConfig()))
	config, err := parseWorkflowLoopConfig(g.ByNode["loop-1"].Data)
	if err != nil {
		t.Fatal(err)
	}
	valid := Object{"round": 2, "maxIterations": 3, "carried": Object{"draft": "next"}, "completed": []any{Object{"index": 1, "carried": Object{"draft": ""}, "states": Object{"entry": Object{"status": "succeeded"}, "worker": Object{"status": "succeeded", "output": Object{"output": "next"}}}}}}
	if !validWorkflowLoopProgress(workflowLoopRunStateFromJSON(decodeGraph(t, valid)), &config, g, "loop-1") {
		t.Fatal("valid prefix refused")
	}
	mutations := []func(Object){
		func(raw Object) { raw["round"] = 1 },
		func(raw Object) { raw["maxIterations"] = 2 },
		func(raw Object) { raw["completed"] = []any{} },
		func(raw Object) { raw["carried"] = Object{"draft": false} },
		func(raw Object) { archive, _ := asWorkflowObject(raw["completed"].([]any)[0]); archive["index"] = 2 },
		func(raw Object) {
			archive, _ := asWorkflowObject(raw["completed"].([]any)[0])
			archive.O("states")["worker"] = Object{"status": "running"}
		},
	}
	for i, mutate := range mutations {
		raw := decodeGraph(t, valid)
		mutate(raw)
		if validWorkflowLoopProgress(workflowLoopRunStateFromJSON(raw), &config, g, "loop-1") {
			t.Fatalf("inconsistent prefix %d accepted", i)
		}
	}
}
