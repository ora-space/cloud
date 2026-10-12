package core

// Direct PostgreSQL evidence for the fences that guard how a clone result is taken over. The
// execution registry makes one promise to every result writer — a repeated report of the same fact is
// an idempotent no-op, and a second, different fact conflicts and leaves the recorded one untouched —
// and that promise has to hold on all three paths that can write `clone_executions.result`:
//
//   - clone.go `cloneResult`, the Node-reported takeover and queried result;
//   - credentialed_clone.go `recordCredentialedCloneResult`, Cloud's own clone registered with its
//     outcome already in hand;
//   - workspace_clone.go `advanceClone`, which turns a *successful* result into the Workspace's
//     baseline commit.
//
// Every scene runs on the shipped seams — `Store.Control` for `clone_dispatch`, `clone_takeover`,
// `clone_pending` and `advance`, `forceStopPublic`/`force_confirm` for the administrative force stop
// — against real `clone_executions` rows. Nothing here substitutes an in-memory store.

import (
	"context"
	"testing"
)

// anonymousWorkspaceCloneScene stages the clone step of a Workspace whose Project names no
// credential, so the ordinary workspace clone path — Node-dispatched, result written by the
// registry's takeover — is the one under test. The credentialed scene is reused and its Project's
// credential binding removed; everything else it stages (the running clone step, the connected Node,
// the reserved runtime control and the Controller lease) is exactly what this path needs.
func anonymousWorkspaceCloneScene(t *testing.T, store *Store) credentialedScene {
	t.Helper()
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/public.git", "")
	if _, err := store.Pool.Exec(`UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project); err != nil {
		t.Fatalf("unbind the project's credential: %v", err)
	}
	return scene
}

// driveControl runs one control action as the lease-holding Controller, the way the gRPC services do.
// The epoch matches the lease `seedControllerLease` writes. The operation identity is set both on the
// body (the clone_* actions read it there) and on the request (the operation-level actions read that
// field), which is exactly how the two gRPC services split them.
func driveControl(t *testing.T, store *Store, action string, body Object) (Object, error) {
	t.Helper()
	body["epoch"] = int64(1)
	return store.Control(context.Background(), &ControlRequest{Action: action, OperationID: body.S("operationId"), Body: body, Service: controllerClaims()})
}

// cloneExecutionWitness reads the recorded result together with the row's `updated_at`, which is the
// durable witness that a write happened: the takeover path bumps it only when it writes a result, so
// two identical readings prove a later report wrote nothing.
func cloneExecutionWitness(t *testing.T, store *Store, execution string) (result []byte, updated string) {
	t.Helper()
	if err := store.Pool.QueryRow(`SELECT result,updated_at::text FROM clone_executions WHERE execution_id=$1`, execution).Scan(&result, &updated); err != nil {
		t.Fatalf("read clone execution %s: %v", execution, err)
	}
	return result, updated
}

// TestCloneResultTakeoverIsIdempotentAndDifferingFactsConflict pins the registry's result contract on
// the Node-reported takeover path (clone.go `cloneResult`): a repeat of the same fact writes nothing,
// and a different fact is refused with `result_conflict` while the recorded result stays exactly as
// it was. The obligation is the one a lost-reply retry leans on.
func TestCloneResultTakeoverIsIdempotentAndDifferingFactsConflict(t *testing.T) {
	store := dispatcherDB(t)
	scene := anonymousWorkspaceCloneScene(t, store)
	execution := "exec-fence-" + scene.workspace[:8]
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("register the clone execution: %v", err)
	}
	if recorded, _ := cloneExecutionWitness(t, store, execution); len(recorded) != 0 {
		t.Fatalf("a fresh dispatch must register with no result, got %s", recorded)
	}

	first := Object{"outcome": "clone_ready", "node": Object{"nodeId": scene.node}, "commit": baseCommit}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": first}); err != nil {
		t.Fatalf("take over the clone result: %v", err)
	}
	before, stamp := cloneExecutionWitness(t, store, execution)

	// Repeating the same fact must be an idempotent no-op: same result, and nothing written.
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": first}); err != nil {
		t.Fatalf("repeating the same clone result must be a no-op, got %v", err)
	}
	if after, stamp2 := cloneExecutionWitness(t, store, execution); string(after) != string(before) || stamp2 != stamp {
		t.Fatalf("an identical repeated result must write nothing: %s/%s -> %s/%s", before, stamp, after, stamp2)
	}

	// A different fact for the same execution is refused and leaves the original untouched.
	differing := Object{"outcome": "clone_ready", "node": Object{"nodeId": scene.node}, "commit": "fedcba9876543210fedcba9876543210fedcba98"}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": differing}); err == nil {
		t.Fatal("a differing result for the same execution must be refused")
	} else if code := faultCode(t, err); code != "result_conflict" {
		t.Fatalf("differing result: got %q, want result_conflict", code)
	}
	if after, stamp2 := cloneExecutionWitness(t, store, execution); string(after) != string(before) || stamp2 != stamp {
		t.Fatalf("a differing result must leave the recorded one untouched: %s/%s -> %s/%s", before, stamp, after, stamp2)
	}
}

// sequencedCloner answers successive Execute calls with the commits it is given, which is what makes
// a replay observably different from the first clone.
type sequencedCloner struct {
	commits []string
	next    int
}

func (c *sequencedCloner) Execute(_ context.Context, clone CredentialedClone) (CredentialedCloneOutcome, error) {
	commit := c.commits[len(c.commits)-1]
	if c.next < len(c.commits) {
		commit = c.commits[c.next]
	}
	c.next++
	return CredentialedCloneOutcome{Commit: commit, CredentialVersion: clone.CredentialVersion}, nil
}

// TestCredentialedCloneReplayDoesNotOverwriteTheRecordedResult pins the same contract on the path
// Cloud's own clone writes through (credentialed_clone.go `recordCredentialedCloneResult`). A
// `clone_dispatch` replayed after a lost reply is idempotent on `(operation, execution, input)`, so
// its registration returns the row that already carries the first clone's result — and the second
// clone's outcome must not silently replace it. Without the fence the recorded commit changed under
// a retry, and the Workspace's registered baseline moved with it.
func TestCredentialedCloneReplayDoesNotOverwriteTheRecordedResult(t *testing.T) {
	store := dispatcherDB(t)
	const secondCommit = "fedcba9876543210fedcba9876543210fedcba98"
	store.SetCredentialedCloner(&sequencedCloner{commits: []string{baseCommit, secondCommit}})
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	execution := "exec-credential-replay"
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("first credentialed dispatch: %v", err)
	}
	recorded, _ := cloneExecutionWitness(t, store, execution)
	if got := mustObject(t, recorded).S("commit"); got != baseCommit {
		t.Fatalf("the first clone's commit must be recorded, got %q", got)
	}

	if _, err := scene.dispatch(store, execution); err == nil {
		t.Fatal("a replay whose clone produced a different commit must conflict, not overwrite the recorded result")
	} else if code := faultCode(t, err); code != "result_conflict" {
		t.Fatalf("replay: got %q, want result_conflict", code)
	}
	recorded, _ = cloneExecutionWitness(t, store, execution)
	if got := mustObject(t, recorded).S("commit"); got != baseCommit {
		t.Fatalf("the replay overwrote the recorded result: got %q, want %q", got, baseCommit)
	}
}

// TestFailedCloneDoesNotAdvanceTheWorkspaceBaseline pins workspace_clone.go `advanceClone`: the
// Workspace's baseline commit is written only from a `clone_ready` execution. A failed clone must not
// leave a baseline behind. The second scene carries a commit inside a `clone_failed` result — the
// registry accepts extra fields — so that removing the guard makes `advanceClone` write that commit
// and the assertion fails directly, rather than failing only because the column's own CHECK rejected
// the empty string.
func TestFailedCloneDoesNotAdvanceTheWorkspaceBaseline(t *testing.T) {
	store := dispatcherDB(t)
	scene := anonymousWorkspaceCloneScene(t, store)
	execution := "exec-failed-" + scene.workspace[:8]
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("register the clone execution: %v", err)
	}
	failed := Object{"outcome": "clone_failed", "node": Object{"nodeId": scene.node}, "commit": "fedcba9876543210fedcba9876543210fedcba98"}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": failed}); err != nil {
		t.Fatalf("record the failed clone result: %v", err)
	}

	version := scalarOf(t, store, "SELECT version FROM operations WHERE id=$1", scene.operation)
	if _, err := driveControl(t, store, "advance", Object{"operationId": scene.operation, "version": version}); err == nil {
		t.Fatal("advancing past a failed clone must be refused")
	} else if code := faultCode(t, err); code != "clone_incomplete" {
		t.Fatalf("advance after a failed clone: got %q, want clone_incomplete", code)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id IS NULL", scene.workspace); n != 1 {
		t.Fatal("a failed clone advanced the Workspace's baseline commit")
	}
}

// TestForceStoppedCloneResultCannotReviveTheRuntime pins the force-stop fence on in-flight clone
// executions (runtime_force_stop.go). An execution intercepted by a confirmed administrative force
// stop is marked, so it is never offered to a Node again; a clone result that arrives afterwards is a
// historical fact about that execution, and it cannot re-open the runtime's admission or revive the
// operation the force stop already failed.
func TestForceStoppedCloneResultCannotReviveTheRuntime(t *testing.T) {
	store := dispatcherDB(t)
	scene := anonymousWorkspaceCloneScene(t, store)
	execution := "exec-forced-" + scene.workspace[:8]
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("register the clone execution: %v", err)
	}

	// Register the force stop exactly as the admin endpoint does, then confirm it exactly as the
	// Controller does: the one in-flight sandbox must be reported terminated with late-ensure fenced.
	wsVersion := scalarOf(t, store, "SELECT version FROM workspaces WHERE id=$1", scene.workspace)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		return forceStopPublic(tx, &PublicRequest{TenantID: scene.tenant, WorkspaceID: scene.workspace, Method: "POST", Body: Object{"version": wsVersion, "impactConfirmed": true, "reason": "terminate the in-flight clone"}}, scene.user)
	}); err != nil {
		t.Fatalf("register the force stop: %v", err)
	}
	forceStopID := textOf(t, store, "SELECT id FROM runtime_force_stops WHERE workspace_id=$1 AND state<>'succeeded'", scene.workspace)
	forceStopVersion := scalarOf(t, store, "SELECT version FROM runtime_force_stops WHERE id=$1", forceStopID)
	targetID := textOf(t, store, "SELECT id FROM runtime_force_stop_targets WHERE force_stop_id=$1", forceStopID)
	if _, err := driveControl(t, store, "force_confirm", Object{"forceStopId": forceStopID, "version": forceStopVersion, "effectId": targetID, "terminated": true, "lateEnsureFenced": true}); err != nil {
		t.Fatalf("confirm the force stop: %v", err)
	}

	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE execution_id=$1 AND terminated_by_force_stop_id IS NOT NULL", execution); n != 1 {
		t.Fatal("the in-flight clone execution was not fenced by the confirmed force stop")
	}
	// The fenced execution must not be handed to a Node again.
	pending, err := driveControl(t, store, "clone_pending", Object{"nodeId": scene.node})
	if err != nil {
		t.Fatalf("list pending dispatches: %v", err)
	}
	rows, _ := pending["executions"].([]Object)
	for _, row := range rows {
		if row.S("executionId") == execution {
			t.Fatal("a force-stopped clone execution was offered to a Node again")
		}
	}

	// A late result is reconciled against the same execution — the force-stop decision keeps every
	// real result ("已经完成的结果保留原结果") — but it must not revive the runtime. Recording the fact
	// is not the obligation's failure; re-opening admission or the failed operation would be.
	late := Object{"outcome": "clone_ready", "node": Object{"nodeId": scene.node}, "commit": baseCommit}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": late}); err != nil {
		t.Fatalf("a late clone result must still reconcile against its original execution, got %v", err)
	}
	if recorded, _ := cloneExecutionWitness(t, store, execution); mustObject(t, recorded).S("outcome") != "clone_ready" {
		t.Fatalf("the late clone result was not reconciled onto its execution: %s", recorded)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE execution_id=$1 AND terminated_by_force_stop_id IS NOT NULL", execution); n != 1 {
		t.Fatal("taking over the late result cleared the force-stop fence")
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND admission_open=false AND observed_state='stopped'", scene.workspace); n != 1 {
		t.Fatal("a late clone result re-opened the force-stopped runtime")
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM operations WHERE id=$1 AND state='failed'", scene.operation); n != 1 {
		t.Fatal("a late clone result revived the operation the force stop failed")
	}
}

// textOf reads one string straight from PostgreSQL for assertions the public shapes strip.
func textOf(t *testing.T, store *Store, q string, args ...any) string {
	t.Helper()
	var s string
	if err := store.Pool.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("read %q: %v", q, err)
	}
	return s
}
