package core

import (
	"errors"
	"testing"
)

func TestWorkflowLoopRejectsInitialAndFeedbackTypeMismatch(t *testing.T) {
	graph := readWorkflowGraph(loopTestGraph(validLoopConfig()))
	config, err := parseWorkflowLoopConfig(graph.ByNode["loop-1"].Data)
	if err != nil {
		t.Fatal(err)
	}
	config.Variables[0].InitialKind = "variable"
	config.Variables[0].InitialSelector = workflowSelector{nodeID: "start-1", root: "value"}
	states := map[string]workflowNodeState{"start-1": {Status: "succeeded", Output: Object{"value": 42}}}
	if value, err := initializeWorkflowLoopCarried(&config, graph, "loop-1", states, nil); !errors.Is(err, errWorkflowLoopType) || value != nil {
		t.Fatalf("mistyped initial accepted: %v %v", value, err)
	}
	states["worker"] = workflowNodeState{Status: "succeeded", Output: Object{"output": false}}
	if value, err := resolveWorkflowLoopFeedback(&config, states); !errors.Is(err, errWorkflowLoopType) || value != nil {
		t.Fatalf("mistyped feedback accepted: %v %v", value, err)
	}
}

func TestWorkflowBodyAttemptsDoNotReuseArchivedReceipts(t *testing.T) {
	node := workflowNodeView{ID: "agent", Container: "it", Kind: "agent"}
	states := map[string]workflowNodeState{"it": {Iteration: &workflowIterationRunState{Completed: []any{
		Object{"index": 0, "states": Object{"agent": Object{"status": "succeeded", "attempt": 2}}},
		Object{"index": 1, "states": Object{"agent": Object{"status": "inactive"}}},
		Object{"index": 2, "states": Object{"agent": Object{"status": "succeeded", "attempt": 3}}},
	}}}}
	if got := workflowArchivedAgentAttempt(states, node); got != 3 {
		t.Fatalf("archived execution fence = %d", got)
	}
	if _, exists := states["agent"]; exists {
		t.Fatal("fixture must model cleared active round")
	}
}
