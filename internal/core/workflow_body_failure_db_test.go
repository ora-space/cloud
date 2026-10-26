package core

import (
	"context"
	"testing"
	"time"
)

func TestWorkflowIterationArchivesInactiveCollectTarget(t *testing.T) {
	store := dispatcherDB(t)
	g := iterationRunGraph("continue", 10, []any{"it", "item"})
	condition := wfNode("c", "condition", "choose")
	condition.O("data")["containerId"] = "it"
	condition.O("data")["cases"] = []any{}
	g["nodes"] = append(g["nodes"].([]any), condition)
	g["edges"] = append(g["edges"].([]any), Object{"source": "c", "target": "agg", "sourceHandle": "yes"})
	id := seedWorkflowRunSceneWithInput(t, store, g, Object{"items": []any{"a"}})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	status, detail, raw, _ := readWorkflowRun(t, store, id)
	states := workflowStatesFromJSON(raw)
	if status != "succeeded" || states["agg"].Status != "inactive" || states["it"].Iteration.Failed != 1 {
		t.Fatalf("inactive target was collected: %s %s %v", status, detail, raw)
	}
	archive, _ := asWorkflowObject(states["it"].Iteration.Completed[0])
	if workflowStatesFromJSON(archive.O("states"))["agg"].Status != "inactive" {
		t.Fatal("inactive target missing in archive")
	}
}

func bodyAgentIterationGraph() Object {
	g := iterationRunGraph("continue", 10, []any{"it", "item"})
	for _, raw := range g["nodes"].([]any) {
		node, _ := asWorkflowObject(raw)
		if node.S("id") == "agg" {
			node.O("data")["kind"] = "agent"
			node.O("data")["agentConfig"] = Object{"prompt": "work"}
		}
	}
	return g
}

func TestWorkflowBodyOperationalFailuresReachContainer(t *testing.T) {
	for _, fault := range []string{"turn", "end", "unreachable", "workspace"} {
		t.Run(fault, func(t *testing.T) {
			store := dispatcherDB(t)
			store.DeliveryUnreachableAfter = 30 * time.Minute
			id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, bodyAgentIterationGraph())
			if _, err := store.Pool.Exec(`UPDATE workflow_runs SET input=$2 WHERE id=$1`, id, jsonText(Object{"items": []any{"a"}})); err != nil {
				t.Fatal(err)
			}
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fault == "workspace" {
				if _, err := store.Pool.Exec(`UPDATE operations SET state='failed' WHERE workspace_id IN (SELECT id FROM workspaces WHERE workflow_run_id=$1) AND kind='create_workspace'`, id); err != nil {
					t.Fatal(err)
				}
			} else {
				ws := settleSessionWorkspace(t, store, id)
				service, spec := startSessionNode(t, store, id, ws, "agg")
				execution := dispatchSession(t, store, id, "agg", service, spec)
				switch fault {
				case "turn":
					ageWorkflowSessionExecution(t, store, execution, "7260")
					if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
						t.Fatal(err)
					}
					reportSessionEnd(t, store, id, execution, service, "cancelled")
				case "unreachable":
					if _, err := store.Pool.Exec(`UPDATE node_instances SET last_seen_at=clock_timestamp()-interval '2 hours' WHERE workspace_id=$1`, ws); err != nil {
						t.Fatal(err)
					}
				case "end":
					takeOverWorkflowEvents(t, store, id, execution, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
					if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
						t.Fatal(err)
					}
					if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes' WHERE workflow_run_id=$1`, id); err != nil {
						t.Fatal(err)
					}
				}
			}
			for range 2 {
				if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			status, detail, raw, _ := readWorkflowRun(t, store, id)
			state := workflowStatesFromJSON(raw)["it"]
			want := "succeeded"
			if fault == "end" {
				want = "failed" // no terminal evidence: release, never reuse the live Workspace
			}
			if status != want || state.Status != want || state.Iteration.Failed != 1 || len(state.Iteration.Completed) != 1 || len(state.Iteration.Ledger) != 1 {
				t.Fatalf("%s bypassed container: %s %s %v", fault, status, detail, raw)
			}
		})
	}
}

func TestWorkflowIterationCancellationDrainsInflightAgent(t *testing.T) {
	store := dispatcherDB(t)
	id, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, bodyAgentIterationGraph())
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET input=$2 WHERE id=$1`, id, jsonText(Object{"items": []any{"a", "b"}})); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	ws := settleSessionWorkspace(t, store, id)
	service, spec := startSessionNode(t, store, id, ws, "agg")
	execution := dispatchSession(t, store, id, "agg", service, spec)
	if _, err := store.Pool.Exec(`UPDATE workflow_runs SET cancel_requested_at=now() WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, _, _, _ := readWorkflowRun(t, store, id); status != "running" {
		t.Fatal("cancel did not wait for in-flight evidence")
	}
	reportSessionEnd(t, store, id, execution, service, "cancelled")
	status, _, raw, _ := readWorkflowRun(t, store, id)
	states := workflowStatesFromJSON(raw)
	if status != "cancelled" || states["it"].Status != "cancelled" || states["agg"].Status != "cancelled" || states["it"].Iteration.Round != 0 {
		t.Fatalf("cancel lost current round: %s %v", status, raw)
	}
	assertRunWorkspaceReleased(t, store, id)
}
