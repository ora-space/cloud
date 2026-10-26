package core

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A running iteration may yield while an agent executes. The same wire codec used by the CAS write
// must preserve its settled ledger and active round before a different scheduler pass resumes it.
func TestWorkflowIterationProgressSurvivesStateJSON(t *testing.T) {
	progress := &workflowIterationRunState{
		Round: 1, Items: []any{"a", "b"}, Strategy: "continue", Failed: 0,
		Ledger: []any{Object{"item": "a", "status": "succeeded", "output": "first", "error": nil}},
		Output: []any{"first"},
		Completed: []any{Object{"index": float64(0), "states": Object{
			"worker": Object{"status": "succeeded", "output": Object{"output": "first"}},
		}}},
	}
	state := workflowNodeState{Status: "running", DisplayName: "foreach", Iteration: progress}
	encoded, err := json.Marshal(Object{"iter": state.object()})
	if err != nil {
		t.Fatal(err)
	}
	var raw Object
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	restored := workflowStatesFromJSON(raw)
	got := restored["iter"]
	if got.Iteration == nil || got.Iteration.Round != 1 || got.Iteration.Strategy != "continue" ||
		!reflect.DeepEqual(got.Iteration.Output, []any{"first"}) || len(got.Iteration.Completed) != 1 || len(got.Iteration.Ledger) != 1 {
		t.Fatalf("persisted iteration progress was lost: %#v", got.Iteration)
	}
	beforeCancel := got.Iteration
	cancelWorkflowIterations(restored)
	got = restored["iter"]
	if got.Status != "cancelled" || got.FinishedAt == "" || !reflect.DeepEqual(got.Iteration, beforeCancel) {
		t.Fatalf("cancellation lost the committed ledger: %#v", got)
	}
}
