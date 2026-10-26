package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWorkflowLoopAgentRetryBudgetResetsAcrossRounds(t *testing.T) {
	store := dispatcherDB(t)
	agent := wfAgentNode("a", Object{"prompt": "work", "retry": Object{"enabled": true, "maxRetries": 1, "initialDelaySeconds": 0}})
	agent.O("data")["containerId"] = "l"
	config := Object{"maxIterations": 2, "maxIterationsBehavior": "succeed", "variables": []any{}, "outputs": []any{}, "until": Object{"logic": "and", "conditions": []any{}}}
	id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, loopSceneGraph(config, []Object{loopBodyStart("entry"), agent}, [][3]string{{"entry", "a", ""}}))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := settleSessionWorkspace(t, store, id)
	nodeService, _ := startSessionNode(t, store, id, ws, "a")
	for attempt := int64(1); attempt <= 4; attempt++ {
		if got := sessionWorkAttempt(t, store, id, "a"); got != attempt {
			t.Fatalf("attempt=%d want=%d", got, attempt)
		}
		execution := fmt.Sprintf("loop-attempt-%d-%s", attempt, id[:8])
		dispatchSessionAttempt(t, store, id, "a", nodeService, execution, sessionWorkSpec(t, store, id, "a"))
		reason := "idle_timeout"
		if attempt%2 == 1 {
			reason = "agent_failed"
		}
		reportSessionEnd(t, store, id, execution, nodeService, reason)
		if attempt%2 == 1 {
			requireRetrying(t, readWorkflowNodeState(t, store, id, "a"), attempt)
			makeWorkflowRetryDue(t, store, id, "a")
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	status, detail, raw, _ := readWorkflowRun(t, store, id)
	state := workflowStatesFromJSON(raw)["l"]
	if status != "succeeded" || len(state.Loop.Completed) != 2 {
		t.Fatalf("rounds failed: %s %s %v", status, detail, raw)
	}
	for i, expected := range []int64{2, 4} {
		archive, _ := asWorkflowObject(state.Loop.Completed[i])
		if got := workflowStatesFromJSON(archive.O("states"))["a"].Attempt; got != expected {
			t.Fatalf("archive attempt=%d want=%d", got, expected)
		}
	}
}

func TestWorkflowBodyStartDeadlineSettlesIterationLedger(t *testing.T) {
	for _, strategy := range []string{"fail", "continue"} {
		store := dispatcherDB(t)
		g := iterationRunGraph(strategy, 10, []any{"it", "item"})
		for _, raw := range g["nodes"].([]any) {
			n, _ := asWorkflowObject(raw)
			if n.S("id") == "agg" {
				n.O("data")["kind"] = "agent"
			}
		}
		id := seedWorkflowRunScene(t, store, g)
		commitWorkflowRunMidpoint(t, store, id, Object{
			"s":   Object{"status": "succeeded", "output": Object{"items": []any{"a"}}},
			"it":  Object{"status": "running", "iteration": Object{"round": 0, "items": []any{"a"}, "ledger": []any{}, "completed": []any{}, "output": []any{}, "failedCount": 0, "strategy": strategy}},
			"agg": Object{"status": "running", "attempt": 1, "startedAt": time.Now().Add(-2 * workflowSessionStartDeadline).UTC().Format(time.RFC3339)},
		})
		for range 2 {
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		status, _, raw, _ := readWorkflowRun(t, store, id)
		want := "failed"
		if strategy == "continue" {
			want = "succeeded"
		}
		state := workflowStatesFromJSON(raw)["it"]
		if status != want || state.Status != want || len(state.Iteration.Ledger) != 1 || len(state.Iteration.Completed) != 1 || state.Iteration.Failed != 1 {
			t.Fatalf("deadline bypassed container: %s %v", status, raw)
		}
	}
}

func TestWorkflowLoopExitPreemptsReadyAgent(t *testing.T) {
	for _, agentFirst := range []bool{true, false} {
		store := dispatcherDB(t)
		exit, agent := wfNode("exit", "loopExit", "exit"), wfNode("worker", "agent", "worker")
		exit.O("data")["containerId"], agent.O("data")["containerId"] = "l", "l"
		condition := graphCondition("c", []any{graphConditionCase("yes", "and", []any{graphRule([]any{"l", "flag"}, "equals", "yes")})})
		condition.O("data")["containerId"] = "l"
		members := []Object{loopBodyStart("entry"), exit, agent, condition}
		if agentFirst {
			members[1], members[2] = agent, exit
		}
		config := Object{"maxIterations": 2, "variables": []any{loopVar("flag", "string", "yes", "l", "flag")}, "outputs": []any{}, "until": Object{"logic": "and", "conditions": []any{}}}
		id := seedWorkflowRunScene(t, store, loopSceneGraph(config, members, [][3]string{{"entry", "c", ""}, {"c", "exit", "yes"}, {"c", "worker", "yes"}}))
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		status, detail, raw, _ := readWorkflowRun(t, store, id)
		states := workflowStatesFromJSON(raw)
		if status != "succeeded" || states["worker"].Status != "inactive" || states["exit"].Status != "succeeded" {
			t.Fatalf("exit did not preempt: %s %s %v", status, detail, raw)
		}
		archive, _ := asWorkflowObject(states["l"].Loop.Completed[0])
		if workflowStatesFromJSON(archive.O("states"))["worker"].Status != "inactive" {
			t.Fatal("inactive member missing from archive")
		}
	}
}

func TestWorkflowLoopRecoveryRefusesMalformedRoundBeforeDispatch(t *testing.T) {
	for _, round := range []any{nil, 0, -1, 1.5, 4} {
		store := dispatcherDB(t)
		id := seedWorkflowRunScene(t, store, loopTestGraph(validLoopConfig()))
		commitWorkflowRunMidpoint(t, store, id, Object{
			"start-1": Object{"status": "succeeded"},
			"loop-1":  Object{"status": "running", "loop": Object{"round": round, "maxIterations": 3, "carried": Object{"draft": ""}, "completed": []any{}}},
		})
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		status, detail, raw, _ := readWorkflowRun(t, store, id)
		if status != "failed" || detail != "loop_invalid_round" {
			t.Fatalf("round %v resumed: %s %s", round, status, detail)
		}
		if _, dispatched := raw["worker"]; dispatched {
			t.Fatal("corrupt round dispatched worker")
		}
	}
}

func TestWorkflowFailureReservePreservesCompletedContainer(t *testing.T) {
	for _, terminal := range []string{"failed", "cancelled"} {
		store := dispatcherDB(t)
		g := loopTestGraph(validLoopConfig())
		id := seedWorkflowRunScene(t, store, g)
		states := map[string]workflowNodeState{"loop-1": {Status: "succeeded", Output: Object{"result": "kept"}}, "later": {Status: "running"}, "padding": {Status: "succeeded", Output: Object{"value": ""}}}
		n := workflowStateLimit - workflowFailureReserve - 100 - len(jsonText(workflowEncodedStates(states)))
		states["padding"] = workflowNodeState{Status: "succeeded", Output: Object{"value": strings.Repeat("x", n)}}
		commitWorkflowRunMidpoint(t, store, id, workflowEncodedStates(states))
		_, _, _, version := readWorkflowRun(t, store, id)
		states["later"] = workflowNodeState{Status: terminal, Error: strings.Repeat("e", 1024)}
		_, err := store.transact(context.Background(), func(tx *transaction) Object {
			finishWorkflowRun(tx, id, version, terminal, "terminal", states)
			return Object{}
		})
		if err != nil {
			t.Fatal(err)
		}
		status, _, raw, _ := readWorkflowRun(t, store, id)
		got := workflowStatesFromJSON(raw)
		if status != terminal || got["later"].Status != terminal || got["loop-1"].Status != "succeeded" || got["loop-1"].Output.S("result") != "kept" {
			t.Fatalf("terminal evidence lost: %s", status)
		}
	}
}
