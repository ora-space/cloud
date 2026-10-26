package core

import (
	"context"
	"strings"
	"testing"
)

func TestWorkflowIterationFailureLedgerAndFinalProjection(t *testing.T) {
	for _, strategy := range []string{"fail", "continue"} {
		t.Run(strategy, func(t *testing.T) {
			store := dispatcherDB(t)
			g := iterationRunGraph(strategy, 10, []any{"missing", "output"})
			id := seedWorkflowRunSceneWithInput(t, store, g, Object{"items": []any{"a", "b"}})
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			status, _, raw, _ := readWorkflowRun(t, store, id)
			state := workflowStatesFromJSON(raw)["it"]
			want := 1
			wantStatus := "failed"
			if strategy == "continue" {
				want = 2
				wantStatus = "succeeded"
			}
			if status != wantStatus || len(state.Iteration.Ledger) != want || len(state.Iteration.Completed) != want || state.Iteration.Failed != want || state.Iteration.Round != want-1 {
				t.Fatalf("bad terminal ledger: %s %+v", status, state.Iteration)
			}
			if workflowStatesFromJSON(raw)["agg"].Status != "failed" {
				t.Fatal("last member projection lost")
			}
		})
	}
}

func TestWorkflowBudgetCASPreservesCommittedState(t *testing.T) {
	store := dispatcherDB(t)
	g := iterationRunGraph("continue", 10, []any{"it", "item"})
	id := seedWorkflowRunScene(t, store, g)
	prior := map[string]workflowNodeState{"s": {Status: "succeeded", Output: Object{"items": []any{"kept"}}}, "it": {Status: "running", Iteration: &workflowIterationRunState{Round: 0, Items: []any{"kept"}, Ledger: []any{}, Output: []any{}, Completed: []any{}, Strategy: "continue"}}, "agg": {Status: "running", Attempt: 1}}
	if _, err := store.Pool.Exec("UPDATE workflow_runs SET status='running',node_states=$2 WHERE id=$1", id, jsonText(workflowEncodedStates(prior))); err != nil {
		t.Fatal(err)
	}
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		states := workflowStatesFromJSON(tx.one("SELECT node_states FROM workflow_runs WHERE id=$1", id).O("nodeStates"))
		candidate := states["agg"]
		candidate.DisplayName = strings.Repeat("x", workflowLocalStateLimit+1)
		states["agg"] = candidate
		node := readWorkflowGraph(g).ByNode["agg"]
		node.Kind = "agent"
		_, cont := commitWorkflowAgentTerminal(tx, id, 1, node, states, Object{"reason": "idle_timeout"})
		if cont {
			t.Fatal("oversized member continued")
		}
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _, raw, version := readWorkflowRun(t, store, id)
	state := workflowStatesFromJSON(raw)
	if status != "failed" || version <= 1 || state["it"].Output.S("reason") != "iteration_state_size_exceeded" {
		t.Fatalf("budget failed to settle: %s %d %v", status, version, raw)
	}
	priorEncoded := workflowEncodedStates(prior)
	if jsonText(raw["s"]) != jsonText(priorEncoded["s"]) || jsonText(raw["agg"]) != jsonText(priorEncoded["agg"]) || len(state["it"].Iteration.Ledger) != 0 {
		t.Fatal("overflow candidate leaked or committed state lost")
	}
	if len(jsonText(raw)) > workflowStateLimit {
		t.Fatal("failure reserve exceeded")
	}
	_, err = store.transact(context.Background(), func(tx *transaction) Object {
		if settleWorkflowRunStates(tx, id, 1, "running", "", prior) {
			t.Fatal("stale CAS applied")
		}
		return Object{}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkflowIterationOversizedInputNeverStartsRound(t *testing.T) {
	store := dispatcherDB(t)
	id := seedWorkflowRunSceneWithInput(t, store, iterationRunGraph("continue", 10, []any{"it", "item"}), Object{"items": []any{strings.Repeat("汉", 400000)}})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, _, raw, _ := readWorkflowRun(t, store, id)
	states := workflowStatesFromJSON(raw)
	if status != "failed" || states["it"].Output.S("reason") != "iteration_state_size_exceeded" {
		t.Fatalf("oversized input accepted: %s %v", status, raw)
	}
	if _, exists := raw["agg"]; exists {
		t.Fatal("member ran before item budget check")
	}
}
