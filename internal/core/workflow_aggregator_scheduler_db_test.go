package core

import (
	"context"
	"testing"
)

// The aggregator's scheduler behavior over PostgreSQL: the first assigned root is passed through as
// `{aggregator}.output`, presence is key existence (a null counts), and an empty candidate set is the
// stable no-match failure.

// aggregatorRunGraph is start → aggregator → output with two declared start variables, so a run can
// assign either, both or neither candidate.
func aggregatorRunGraph(selectors ...[]any) Object {
	start := wfNode("s", "start", "开始")
	start["data"].(Object)["inputVariables"] = []any{
		Object{"name": "a", "valueType": "string"},
		Object{"name": "b", "valueType": "string"},
	}
	agg := wfNode("agg", "aggregator", "合并")
	choices := make([]any, 0, len(selectors))
	for _, selector := range selectors {
		choices = append(choices, selector)
	}
	agg["data"].(Object)["aggregatorConfig"] = Object{"variables": choices}
	return wfGraph(
		[]Object{start, agg, wfNode("o", "output", "输出")},
		[][3]string{{"s", "agg", ""}, {"agg", "o", ""}},
	)
}

// TestWorkflowAdvanceAggregatorPassesFirstAssignedCandidate pins the order contract: the first
// assigned selector wins even when a later candidate is also assigned.
func TestWorkflowAdvanceAggregatorPassesFirstAssignedCandidate(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		aggregatorRunGraph([]any{"s", "b"}, []any{"s", "a"}),
		Object{"a": "from-a", "b": "from-b"},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	agg, ok := asWorkflowObject(states["agg"])
	if !ok {
		t.Fatalf("aggregator has no committed state: %v", states)
	}
	if got := agg.O("output")["output"]; got != "from-b" {
		t.Fatalf("aggregator output = %v; want the first declared candidate from-b", got)
	}
	if got := wfNodeStatus(t, states, "o"); got != "succeeded" {
		t.Fatalf("downstream output status = %q; want succeeded", got)
	}
}

// TestWorkflowAdvanceAggregatorSkipsAnUnassignedCandidate pins the assignment rule: a declared but
// unproduced root is skipped, not treated as a null value.
func TestWorkflowAdvanceAggregatorSkipsAnUnassignedCandidate(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		aggregatorRunGraph([]any{"s", "a"}, []any{"s", "b"}),
		Object{"b": "only-b"},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	agg, _ := asWorkflowObject(states["agg"])
	if got := agg.O("output")["output"]; got != "only-b" {
		t.Fatalf("aggregator output = %v; want the only assigned candidate only-b", got)
	}
}

// TestWorkflowAdvanceAggregatorCountsANullAsAssigned pins the falsy rule: an explicit null is assigned
// (key existence), so the aggregator passes it through instead of failing no-match.
func TestWorkflowAdvanceAggregatorCountsANullAsAssigned(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		aggregatorRunGraph([]any{"s", "a"}, []any{"s", "b"}),
		Object{"a": nil},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded for an assigned null", status, runErr)
	}
	agg, _ := asWorkflowObject(states["agg"])
	if value, present := agg.O("output")["output"]; !present || value != nil {
		t.Fatalf("aggregator output = %v (present %v); want an assigned null", value, present)
	}
}

// TestWorkflowAdvanceAggregatorFailsWithoutAssignedCandidate pins the no-match failure: no candidate
// produced anything fails the node and the run with the stable reason.
func TestWorkflowAdvanceAggregatorFailsWithoutAssignedCandidate(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store,
		aggregatorRunGraph([]any{"s", "a"}, []any{"s", "b"}),
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "aggregator_no_match" {
		t.Fatalf("run settled %q error %q; want failed/aggregator_no_match", status, runErr)
	}
	agg, _ := asWorkflowObject(states["agg"])
	if agg.S("status") != "failed" || agg.O("output").S("reason") != "aggregator_no_match" {
		t.Fatalf("aggregator state = %v; want failed with its reason", states["agg"])
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("a node downstream of the failed aggregator committed: %v", states)
	}
}
