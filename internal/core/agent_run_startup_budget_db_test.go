package core

import (
	"context"
	"testing"
	"time"
)

func expireStartupWindow(t *testing.T, store *Store, runID string) {
	t.Helper()
	if _, err := store.Pool.Exec("UPDATE issue_runs SET startup_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
}

func expireConfirmation(t *testing.T, store *Store, runID string) {
	t.Helper()
	if _, err := store.Pool.Exec("UPDATE issue_runs SET startup_ending_deadline_at=clock_timestamp()-interval '1 second' WHERE id=$1", runID); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyStartupFreezesDeadlineEvenWhenDeclarationCannotReachNode(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput()) // No current Node: declaration rolls back.
	startupPass(t, store)
	var before, after time.Time
	if err := store.Pool.QueryRow("SELECT startup_deadline_at FROM issue_runs WHERE id=$1", scene.seed.run).Scan(&before); err != nil {
		t.Fatalf("legacy window rolled back with failed declaration: %v", err)
	}
	store.AgentStartTimeout = time.Hour
	startupPass(t, store)
	if err := store.Pool.QueryRow("SELECT startup_deadline_at FROM issue_runs WHERE id=$1", scene.seed.run).Scan(&after); err != nil || !before.Equal(after) {
		t.Fatalf("legacy deadline changed on retry: %v", err)
	}
}

// A second Store models a restarted/rolling deployment with different settings; pending input
// and rescheduled work must not move the absolute ready-based business deadline.
func TestStartupDeadlineIsFrozenAcrossReplicasAndRetries(t *testing.T) {
	store := commandStore(t)
	store.AgentStartTimeout = 2 * time.Minute
	scene, _ := seedDispatchableWork(t, store)
	var before, after time.Time
	if err := store.Pool.QueryRow("SELECT startup_deadline_at FROM issue_runs WHERE id=$1", scene.run).Scan(&before); err != nil {
		t.Fatal(err)
	}
	seedQueuedUserTurn(t, store, scene.run, "does not extend startup")
	if _, err := store.Pool.Exec("UPDATE execution_work SET available_at=clock_timestamp()+interval '1 hour' WHERE run_id=$1", scene.run); err != nil {
		t.Fatal(err)
	}
	restarted := *store
	restarted.AgentStartTimeout = time.Hour
	startupPass(t, &restarted)
	if err := store.Pool.QueryRow("SELECT startup_deadline_at FROM issue_runs WHERE id=$1", scene.run).Scan(&after); err != nil || !after.Equal(before) {
		t.Fatalf("deadline changed across configuration/retry: %v -> %v (%v)", before, after, err)
	}
	expireStartupWindow(t, store, scene.run)
	startupPass(t, &restarted)
	phase, status, reason, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "releasing" || status != "failed" || reason != "agent_start_failed" {
		t.Fatalf("delayed work escaped original cutoff: %s/%s/%s", phase.String, status, reason)
	}
}

func TestUnknownStartupEndsWithResidualWithoutFabricatingNodeResult(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	requestStartupEnd(t, store, scene.dispatchableScene)
	var deadline time.Time
	if err := store.Pool.QueryRow("SELECT startup_ending_deadline_at FROM issue_runs WHERE id=$1", scene.run).Scan(&deadline); err != nil {
		t.Fatal(err)
	}
	store.AgentEndConfirmTimeout = time.Hour
	startupPass(t, store)
	var after time.Time
	if err := store.Pool.QueryRow("SELECT startup_ending_deadline_at FROM issue_runs WHERE id=$1", scene.run).Scan(&after); err != nil || !after.Equal(deadline) {
		t.Fatalf("ending deadline renewed: %v", err)
	}
	expireConfirmation(t, store, scene.run)
	startupPass(t, store)
	startupPass(t, store)
	phase, status, reason, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "done" || status != "failed" || reason != "agent_start_failed" || runThreadState(t, store, scene.run).String != "ended" {
		t.Fatalf("unbounded startup ending: %s/%s/%s", phase.String, status, reason)
	}
	var retained, protected bool
	if err := store.Pool.QueryRow(`SELECT e.result IS NULL,NOT w.admission_open
	 FROM node_executions e JOIN workspaces w ON w.id=e.workspace_id WHERE e.execution_id=$1`, scene.execution).Scan(&retained, &protected); err != nil || !retained || !protected {
		t.Fatalf("unknown execution lost protection: %v %v %v", retained, protected, err)
	}
	var count int
	if err := store.Pool.QueryRow("SELECT count(*) FROM agent_run_residuals WHERE run_id=$1", scene.run).Scan(&count); err != nil || count != 1 {
		t.Fatalf("residual responsibility must be unique: %d %v", count, err)
	}
	if countDeleteOperations(t, store, scene.run) != 0 {
		t.Fatal("unknown session was physically deleted")
	}
	if _, err := store.DeleteRunWorkspace(context.Background(), scene.run); err == nil {
		t.Fatal("ordinary delete bypassed residual protection")
	}
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if len(objectsOf(claimThreadCommands(tx, 100)["commands"])) != 0 {
			t.Fatal("terminal run still exposes thread commands")
		}
		return Object{}
	}); err != nil {
		t.Fatal(err)
	}
}

func TestStartupResidualFailureRollsBackBusinessTerminalAndProtection(t *testing.T) {
	store := commandStore(t)
	scene := seedTakeoverScene(t, store)
	requestStartupEnd(t, store, scene.dispatchableScene)
	expireConfirmation(t, store, scene.run)
	if _, err := store.Pool.Exec(`CREATE FUNCTION reject_residual() RETURNS trigger LANGUAGE plpgsql AS
	 $$ BEGIN RAISE EXCEPTION 'injected residual failure'; END $$;
	 CREATE TRIGGER reject_residual BEFORE INSERT ON agent_run_residuals FOR EACH ROW EXECUTE FUNCTION reject_residual()`); err != nil {
		t.Fatal(err)
	}
	startupPass(t, store)
	phase, status, _, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "starting" || status != "dispatched" {
		t.Fatal("business terminal committed without residual responsibility")
	}
	if _, err := store.Pool.Exec("DROP TRIGGER reject_residual ON agent_run_residuals; DROP FUNCTION reject_residual()"); err != nil {
		t.Fatal(err)
	}
	startupPass(t, store)
	phase, status, _, _, _, _ = runFields(t, store, scene.run)
	if phase.String != "done" || status != "failed" {
		t.Fatal("recovery did not settle after the ledger became writable")
	}
}

// A transport reconnect is availability evidence, not permission to clear a business residual.
func TestNodeReconnectPreservesStartupResidualAdmissionFence(t *testing.T) {
	for _, abandoned := range []bool{false, true} {
		name := "live run"
		if abandoned {
			name = "abandoned run"
		}
		t.Run(name, func(t *testing.T) {
			store := commandStore(t)
			scene := seedTakeoverScene(t, store)
			if abandoned {
				requestStartupEnd(t, store, scene.dispatchableScene)
				expireConfirmation(t, store, scene.run)
				startupPass(t, store)
			}
			for _, state := range []string{"disconnected", "connected", "connected"} {
				if _, err := store.transact(context.Background(), func(tx *transaction) Object {
					n := tx.one("SELECT * FROM node_instances WHERE node_id=$1", scene.node)
					w := tx.one("SELECT * FROM workspaces WHERE id=$1", scene.ws)
					nodeStatus(tx, &ControlRequest{Body: Object{
						"version": n.N("version"), "connectionState": state, "initialized": true,
					}}, n, w)
					return Object{}
				}); err != nil {
					t.Fatal(err)
				}
			}
			var open, connected bool
			var observed string
			if err := store.Pool.QueryRow(`SELECT w.admission_open,w.observed_state,
			 n.connection_state='connected' FROM workspaces w JOIN node_instances n ON n.workspace_id=w.id
			 WHERE w.id=$1`, scene.ws).Scan(&open, &observed, &connected); err != nil {
				t.Fatal(err)
			}
			if open == abandoned || observed != "ready" || !connected {
				t.Fatalf("reconnect admission=%v ready=%s connected=%v, abandoned=%v", open, observed, connected, abandoned)
			}
			if abandoned {
				phase, status, _, _, _, _ := runFields(t, store, scene.run)
				if phase.String != "done" || status != "failed" {
					t.Fatal("connection evidence resurrected the abandoned run")
				}
			}
		})
	}
}
