package core

import (
	"context"
	"testing"
)

// Iteration's scheduler behavior over PostgreSQL: one round per item in order, the ledger projection,
// the fixed exposed trio, both error strategies, the empty array and the over-limit refusal.

// iterationRunGraph builds start → iteration → output. The region holds one aggregator member, so the
// rounds need no agent session: it passes the outer `items` array through as its output, and the
// collect selector reads that output.
func iterationRunGraph(strategy string, maxIterations int, selectors ...[]any) Object {
	start := wfNode("s", "start", "开始")
	start["data"].(Object)["inputVariables"] = []any{Object{"name": "items", "valueType": "array"}}
	iteration := wfNode("it", "iteration", "迭代")
	iteration["data"].(Object)["iterationConfig"] = Object{
		"iteratorSelector": []any{"s", "items"},
		"collectSelector":  []any{"agg", "output"},
		"errorStrategy":    strategy,
		"maxIterations":    float64(maxIterations),
	}
	member := wfNode("agg", "aggregator", "聚合")
	member["data"].(Object)["containerId"] = "it"
	choices := make([]any, 0, len(selectors))
	for _, selector := range selectors {
		choices = append(choices, selector)
	}
	member["data"].(Object)["aggregatorConfig"] = Object{"variables": choices}
	return wfGraph(
		[]Object{start, iteration, member, wfNode("o", "output", "输出")},
		[][3]string{{"s", "it", ""}, {"it", "o", ""}},
	)
}

// TestWorkflowAdvanceIterationCollectsEveryRound pins the happy path: two items, two rounds, the
// collected values in input order and the ledger aligned with them.
func TestWorkflowAdvanceIterationCollectsEveryRound(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		iterationRunGraph("fail", 10, []any{"s", "items"}),
		Object{"items": []any{"a", "b"}},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	iteration, _ := asWorkflowObject(states["it"])
	output := iteration.O("output")
	collected, _ := output["output"].([]any)
	if len(collected) != 2 {
		t.Fatalf("collected %v; want two values", output["output"])
	}
	entries, _ := output["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries %v; want two rounds", output["entries"])
	}
	first, _ := asWorkflowObject(entries[0])
	if first.S("item") != "a" || first.S("status") != "succeeded" {
		t.Fatalf("first entry = %v; want item a succeeded", entries[0])
	}
	if failed, ok := workflowNumberOf(output["failed_count"]); !ok || failed != 0 {
		t.Fatalf("failed_count = %v; want 0", output["failed_count"])
	}
}

// TestWorkflowAdvanceIterationEmptyArraySucceeds pins the empty input: zero rounds, empty projections.
func TestWorkflowAdvanceIterationEmptyArraySucceeds(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		iterationRunGraph("fail", 10, []any{"s", "items"}),
		Object{"items": []any{}},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	iteration, _ := asWorkflowObject(states["it"])
	output := iteration.O("output")
	if collected, _ := output["output"].([]any); len(collected) != 0 {
		t.Fatalf("collected %v; want empty", output["output"])
	}
	if entries, _ := output["entries"].([]any); len(entries) != 0 {
		t.Fatalf("entries %v; want empty", output["entries"])
	}
	if failed, _ := workflowNumberOf(output["failed_count"]); failed != 0 {
		t.Fatalf("failed_count = %v; want 0", output["failed_count"])
	}
}

// TestWorkflowAdvanceIterationContinueAbsorbsFailedRounds pins `continue`: a failing collect target
// makes every round a ledger entry with a reason, and the node still completes with a count.
func TestWorkflowAdvanceIterationContinueAbsorbsFailedRounds(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		iterationRunGraph("continue", 10, []any{"s", "unassigned"}),
		Object{"items": []any{"a", "b"}},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded with absorbed failures", status, runErr)
	}
	iteration, _ := asWorkflowObject(states["it"])
	output := iteration.O("output")
	if failed, _ := workflowNumberOf(output["failed_count"]); failed != 2 {
		t.Fatalf("failed_count = %v; want 2", output["failed_count"])
	}
	if collected, _ := output["output"].([]any); len(collected) != 0 {
		t.Fatalf("collected %v; want empty for failed rounds", output["output"])
	}
	entries, _ := output["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("entries %v; want two failed rounds", output["entries"])
	}
	first, _ := asWorkflowObject(entries[0])
	if first.S("status") != "failed" || first.S("error") == "" {
		t.Fatalf("first entry = %v; want failed with a reason", entries[0])
	}
}

// TestWorkflowAdvanceIterationFailStopsAtTheFirstFailedRound pins `fail`: the region failure becomes
// the iteration's failure and the run fails with the same reason.
func TestWorkflowAdvanceIterationFailStopsAtTheFirstFailedRound(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		iterationRunGraph("fail", 10, []any{"s", "unassigned"}),
		Object{"items": []any{"a", "b"}},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr == "" {
		t.Fatalf("run settled %q error %q; want failed with a reason", status, runErr)
	}
	iteration, _ := asWorkflowObject(states["it"])
	if iteration.S("status") != "failed" {
		t.Fatalf("run error %q; all states %v; iteration state = %v; want failed", runErr, states, states["it"])
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("downstream output committed after a failed iteration: %v", states)
	}
}

// TestWorkflowAdvanceIterationRefusesAnOverLimitArray pins the start-boundary ceiling: no round runs,
// never a silent truncation.
func TestWorkflowAdvanceIterationRefusesAnOverLimitArray(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunSceneWithInput(t, store,
		iterationRunGraph("fail", 2, []any{"s", "items"}),
		Object{"items": []any{"a", "b", "c"}},
	)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "iteration_over_limit" {
		t.Fatalf("run settled %q error %q; want failed/iteration_over_limit", status, runErr)
	}
	iteration, _ := asWorkflowObject(states["it"])
	if iteration.S("status") != "failed" {
		t.Fatalf("iteration state = %v; want failed", states["it"])
	}
}

// TestWorkflowAdvanceCancelsARunningIteration pins the cancellation reaction: a run cancelled while
// its iteration is mid-flight settles `cancelled`, and the node is cancelled with it instead of
// staying `running` in a terminal run.
func TestWorkflowAdvanceCancelsARunningIteration(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, iterationRunGraph("fail", 10, []any{"s", "items"}))
	commitWorkflowRunMidpoint(t, store, runID, Object{
		"s": Object{"status": "succeeded", "output": Object{"items": []any{"a"}}},
		"it": Object{"status": "running", "startedAt": nowRFC3339(), "iteration": Object{
			"round":       0,
			"items":       []any{"a"},
			"ledger":      []any{},
			"output":      []any{},
			"failedCount": 0,
			"strategy":    "fail",
			"completed":   []any{},
		}},
	})
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET cancel_requested_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, store, runID)
	if status != "cancelled" {
		t.Fatalf("run settled %q; want cancelled", status)
	}
	iteration, _ := asWorkflowObject(states["it"])
	if iteration.S("status") != "cancelled" {
		t.Fatalf("iteration state = %v; want cancelled", states["it"])
	}
}
