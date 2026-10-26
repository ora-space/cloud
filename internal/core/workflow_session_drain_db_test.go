package core

import (
	"context"
	"testing"
)

func TestWorkflowLoopTimeoutDrainsBeforeFailure(t *testing.T) {
	for _, outcome := range []string{"confirmed", "expired"} {
		t.Run(outcome, func(t *testing.T) {
			store := dispatcherDB(t)
			id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, bodyAgentLoopGraph())
			advance := func() {
				t.Helper()
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			advance()
			ws := settleSessionWorkspace(t, store, id)
			service, spec := startSessionNode(t, store, id, ws, "agg")
			execution := dispatchSession(t, store, id, "agg", service, spec)
			ageWorkflowSessionExecution(t, store, execution, "7260")
			advance()
			advance()
			status, _, raw, _ := readWorkflowRun(t, store, id)
			states := workflowStatesFromJSON(raw)
			if status != "running" || states["agg"].Status != "running" || len(states["l"].Loop.Completed) != 0 {
				t.Fatal("loop settled before terminal evidence or confirmation deadline")
			}
			wantReason, wantError := workflowSessionTurnDeadlineExceeded, workflowSessionTurnDeadlineExceeded
			if outcome == "confirmed" {
				reportSessionEnd(t, store, id, execution, service, "cancelled")
			} else {
				if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes' WHERE workflow_run_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				wantReason, wantError = "session_end_deadline_exceeded", "session end deadline exceeded"
			}
			advance()
			status, detail, raw, version := readWorkflowRun(t, store, id)
			states = workflowStatesFromJSON(raw)
			loop := states["l"].Loop
			if status != "failed" || detail != wantError || loop.Round != 1 || len(loop.Completed) != 1 || states["l"].Status != "failed" || states["agg"].Output.S("reason") != wantReason {
				t.Fatalf("loop drain lost failure: %s %s %v", status, detail, raw)
			}
			archive, _ := asWorkflowObject(loop.Completed[0])
			if archive.O("states").O("agg").O("output").S("reason") != wantReason {
				t.Fatal("loop archive disagrees with member diagnosis")
			}
			assertRunWorkspaceReleased(t, store, id)
			var commands, attempts int
			if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_commands WHERE workflow_run_id=$1`, id).Scan(&commands); err != nil || commands != 1 {
				t.Fatalf("end command not unique: %d %v", commands, err)
			}
			if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND workflow_node_attempt>1`, id).Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("loop dispatched after failure: %d %v", attempts, err)
			}
			advance()
			_, _, after, afterVersion := readWorkflowRun(t, store, id)
			if version != afterVersion || jsonText(raw) != jsonText(after) {
				t.Fatal("loop terminal replay changed archive")
			}
		})
	}
}

func TestWorkflowIterationTimeoutDrainsBeforeNextRound(t *testing.T) {
	for _, outcome := range []string{"confirmed", "expired", "end-expired"} {
		t.Run(outcome, func(t *testing.T) {
			store := dispatcherDB(t)
			id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, bodyAgentIterationGraph())
			if _, err := store.Pool.Exec(`UPDATE workflow_runs SET input=$2 WHERE id=$1`, id, jsonText(Object{"items": []any{"a", "b"}})); err != nil {
				t.Fatal(err)
			}
			advance := func() {
				t.Helper()
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			advance()
			ws := settleSessionWorkspace(t, store, id)
			service, spec := startSessionNode(t, store, id, ws, "agg")
			execution := dispatchSession(t, store, id, "agg", service, spec)
			if outcome == "end-expired" {
				takeOverWorkflowEvents(t, store, id, execution, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
			} else {
				ageWorkflowSessionExecution(t, store, execution, "7260")
			}
			advance()
			// Repeated scheduler passes read the persisted barrier and reuse the same command.
			for range 3 {
				advance()
			}
			status, _, raw, _ := readWorkflowRun(t, store, id)
			states := workflowStatesFromJSON(raw)
			if status != "running" || states["agg"].Status != "running" || states["it"].Iteration.Round != 0 || len(states["it"].Iteration.Ledger) != 0 {
				t.Fatalf("advanced before termination: %s %v", status, raw)
			}
			var commands, attempts int
			if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id='agg' AND workflow_node_attempt=1`, id).Scan(&commands); err != nil || commands != 1 {
				t.Fatalf("drain command is not unique: %d %v", commands, err)
			}
			if outcome == "confirmed" {
				reportSessionEnd(t, store, id, execution, service, "cancelled")
				advance()
				status, _, raw, _ = readWorkflowRun(t, store, id)
				states = workflowStatesFromJSON(raw)
				if status != "running" || states["it"].Iteration.Round != 1 || states["it"].Iteration.Failed != 1 || states["agg"].Attempt != 2 {
					t.Fatalf("confirmed timeout did not continue: %s %v", status, raw)
				}
				archive, _ := asWorkflowObject(states["it"].Iteration.Completed[0])
				if archive.O("states").O("agg").O("output").S("reason") != workflowSessionTurnDeadlineExceeded {
					t.Fatal("termination acknowledgement replaced timeout diagnosis")
				}
			} else {
				if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes' WHERE workflow_run_id=$1`, id); err != nil {
					t.Fatal(err)
				}
				advance()
				status, _, raw, _ = readWorkflowRun(t, store, id)
				states = workflowStatesFromJSON(raw)
				if status != "failed" || states["it"].Status != "failed" || states["it"].Iteration.Round != 0 || len(states["it"].Iteration.Completed) != 1 || states["it"].Iteration.Failed != 1 {
					t.Fatalf("unconfirmed session continued or lost archive: %s %v", status, raw)
				}
				_, detail, _, _ := readWorkflowRun(t, store, id)
				if detail != "session end deadline exceeded" || states["agg"].Error != "session_end_deadline_exceeded" || states["agg"].Output.S("reason") != "session_end_deadline_exceeded" {
					t.Fatal("end-confirmation timeout does not follow D4 diagnostics")
				}
				assertRunWorkspaceReleased(t, store, id)
			}
			if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1 AND workflow_node_attempt>1`, id).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			if outcome != "confirmed" && attempts != 0 {
				t.Fatal("dispatched another attempt without terminal evidence")
			}
		})
	}
}
