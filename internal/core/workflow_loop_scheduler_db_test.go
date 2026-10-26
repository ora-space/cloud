package core

import (
	"context"
	"testing"
)

// These are the loop round engine's end-to-end scheduler tests over PostgreSQL. They use no agent
// node, so every round is decided inline and the assertions are about the round machine itself:
// simultaneous feedback, until, exhaustion and loopExit.

// loopSceneGraph builds start → loop → output with a body of the given members and edges. The loop's
// config is supplied by the caller so each test states the contract it exercises.
func loopSceneGraph(loopConfig Object, bodyNodes []Object, bodyEdges [][3]string) Object {
	loop := wfNode("l", "loop", "循环 1")
	loop["data"].(Object)["loopConfig"] = loopConfig
	nodes := []Object{wfNode("s", "start", "开始 1"), loop, wfNode("o", "output", "输出 1")}
	nodes = append(nodes, bodyNodes...)
	edges := [][3]string{{"s", "l", ""}, {"l", "o", ""}}
	edges = append(edges, bodyEdges...)
	return wfGraph(nodes, edges)
}

// loopBodyStart is the one start node a loop body requires.
func loopBodyStart(id string) Object {
	node := wfNode(id, "start", "入口")
	node["data"].(Object)["containerId"] = "l"
	return node
}

// loopVar builds one carried variable with a constant initial and a feedback selector.
func loopVar(name, valueType string, initial any, feedback ...string) Object {
	selector := make([]any, 0, len(feedback))
	for _, part := range feedback {
		selector = append(selector, part)
	}
	return Object{
		"name":      name,
		"valueType": valueType,
		"initial":   Object{"kind": "constant", "value": initial},
		"feedback":  selector,
	}
}

// loopUntilEquals builds the round-end condition `selector equals value`.
func loopUntilEquals(selector []any, value any) Object {
	return Object{
		"logic": "and",
		"conditions": []any{
			Object{"variableSelector": selector, "operator": "equals", "value": value},
		},
	}
}

// readLoopState projects the committed loop progress for assertions.
func readLoopState(t *testing.T, states Object) (round int64, reason string, completed int) {
	t.Helper()
	node, ok := asWorkflowObject(states["l"])
	if !ok {
		t.Fatalf("loop node has no committed state")
	}
	loop, ok := asWorkflowObject(node["loop"])
	if !ok {
		t.Fatalf("loop node has no committed loop state: %v", node)
	}
	finished, _ := loop["completed"].([]any)
	return loop.N("round"), loop.S("reason"), len(finished)
}

// TestWorkflowAdvanceLoopFinishesWhenUntilHolds drives two rounds with a carried pair that swaps
// values each round: the first round does not satisfy until, the second does. It pins that feedback
// is read from one completed snapshot (a swap must exchange values, not collapse both onto one) and
// that the loop publishes the final carried value.
func TestWorkflowAdvanceLoopFinishesWhenUntilHolds(t *testing.T) {
	store := dispatcherDB(t)
	config := Object{
		"maxIterations": 3,
		"variables": []any{
			loopVar("a", "string", "a", "l", "b"),
			loopVar("b", "string", "b", "l", "a"),
		},
		"until":   loopUntilEquals([]any{"l", "a"}, "b"),
		"outputs": []any{Object{"name": "result", "variableSelector": []any{"l", "a"}}},
	}
	runID := seedWorkflowRunScene(t, store, loopSceneGraph(config,
		[]Object{loopBodyStart("entry")},
		[][3]string{},
	))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	round, reason, completed := readLoopState(t, states)
	if round != 2 || reason != "" || completed != 2 {
		t.Fatalf("loop round=%d reason=%q completed=%d; want 2 with no extra reason field and 2 archives", round, reason, completed)
	}
	node, _ := asWorkflowObject(states["l"])
	output, _ := asWorkflowObject(node["output"])
	if got := output["result"]; got != "b" {
		t.Fatalf("loop output result = %v; want the swapped carried value b", got)
	}
	if got := wfNodeStatus(t, states, "o"); got != "succeeded" {
		t.Fatalf("output node status = %q; want succeeded", got)
	}
}

// TestWorkflowAdvanceLoopFailsOnExhaustion pins the default exhaustion policy: a loop that never
// satisfies until fails the run with a stable reason instead of publishing its last round.
func TestWorkflowAdvanceLoopFailsOnExhaustion(t *testing.T) {
	store := dispatcherDB(t)
	config := Object{
		"maxIterations": 2,
		"variables": []any{
			loopVar("a", "string", "a", "l", "b"),
			loopVar("b", "string", "b", "l", "a"),
		},
		"until":   loopUntilEquals([]any{"l", "a"}, "never"),
		"outputs": []any{},
	}
	runID := seedWorkflowRunScene(t, store, loopSceneGraph(config,
		[]Object{loopBodyStart("entry")},
		[][3]string{},
	))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "loop_max_iterations_reached" {
		t.Fatalf("run settled %q error %q; want failed loop_max_iterations_reached", status, runErr)
	}
	round, _, completed := readLoopState(t, states)
	if round != 2 || completed != 2 {
		t.Fatalf("loop round=%d completed=%d; want 2 2", round, completed)
	}
}

// TestWorkflowAdvanceLoopFailsWhenFeedbackIsUnset pins the atomic feedback rule: a selector that names
// an assigned-but-absent root fails the loop instead of starting a round with a hole.
func TestWorkflowAdvanceLoopFailsWhenFeedbackIsUnset(t *testing.T) {
	store := dispatcherDB(t)
	config := Object{
		"maxIterations": 2,
		"variables": []any{
			loopVar("a", "string", "a", "s", "missing"),
		},
		"until":   loopUntilEquals([]any{"l", "a"}, "never"),
		"outputs": []any{},
	}
	runID := seedWorkflowRunScene(t, store, loopSceneGraph(config,
		[]Object{loopBodyStart("entry")},
		[][3]string{},
	))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, _, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "loop_value_unset" {
		t.Fatalf("run settled %q error %q; want failed loop_value_unset", status, runErr)
	}
}

// TestWorkflowAdvanceCancelsARunningLoop pins the cancellation reaction: a run cancelled while its
// loop is mid-flight settles `cancelled`, and the loop node is cancelled with it instead of staying
// `running` in a terminal run.
func TestWorkflowAdvanceCancelsARunningLoop(t *testing.T) {
	store := dispatcherDB(t)
	config := Object{
		"maxIterations": 3,
		"variables":     []any{},
		"until":         Object{"logic": "and", "conditions": []any{}},
		"outputs":       []any{},
	}
	runID := seedWorkflowRunScene(t, store, loopSceneGraph(config,
		[]Object{loopBodyStart("entry")},
		[][3]string{},
	))
	commitWorkflowRunMidpoint(t, store, runID, Object{
		"s": Object{"status": "succeeded", "output": Object{"input": ""}},
		"l": Object{"status": "running", "startedAt": nowRFC3339(), "loop": Object{
			"round":         1,
			"maxIterations": 3,
			"carried":       Object{},
			"completed":     []any{},
		}},
	})
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET cancel_requested_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("request cancellation: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q; want cancelled", status, runErr)
	}
	if got := wfNodeStatus(t, states, "l"); got != "cancelled" {
		t.Fatalf("loop node status = %q; want cancelled", got)
	}
	if _, ran := states["o"]; ran {
		t.Fatalf("a node downstream of the cancelled loop committed: %v", states)
	}
}

// TestWorkflowAdvanceLoopExitsEarly pins loopExit: the marker ends the round before feedback, and the
// loop publishes the outputs resolved from the round that produced it.
func TestWorkflowAdvanceLoopExitsEarly(t *testing.T) {
	store := dispatcherDB(t)
	exit := wfNode("exit", "loopExit", "早退")
	exit["data"].(Object)["containerId"] = "l"
	config := Object{
		"maxIterations": 5,
		"variables": []any{
			loopVar("draft", "string", "x", "l", "draft"),
		},
		"until":   loopUntilEquals([]any{"l", "draft"}, "never"),
		"outputs": []any{Object{"name": "result", "variableSelector": []any{"l", "draft"}}},
	}
	runID := seedWorkflowRunScene(t, store, loopSceneGraph(config,
		[]Object{loopBodyStart("entry"), exit},
		[][3]string{{"entry", "exit", ""}},
	))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q; want succeeded", status, runErr)
	}
	round, reason, completed := readLoopState(t, states)
	if round != 1 || reason != "" || completed != 1 {
		t.Fatalf("loop round=%d reason=%q completed=%d; want 1 with no extra reason field and 1 archive", round, reason, completed)
	}
	node, _ := asWorkflowObject(states["l"])
	output, _ := asWorkflowObject(node["output"])
	if got := output["result"]; got != "x" {
		t.Fatalf("loop output result = %v; want the carried value x", got)
	}
}
