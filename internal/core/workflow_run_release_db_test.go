package core

// White-box acceptance for the WorkflowRun Workspace release (circus#75 S5; specs decisions/cloud/
// workflow 0-cloud-side-scheduler-owns-workflow-execution D3/D7). Everything here is real PostgreSQL
// in an isolated schema, and the transitions are driven the way production drives them: the terminal
// cleanup in the transaction that settles the run, the delete-completion hook in the transaction
// that writes the operation `succeeded`, and the scheduler's own bounded pass for the give-up and
// reconciliation half.
//
// The obligation under test is a pair of independent facts: a terminal run may not leave its
// Workspace's fate unrecorded, and Cloud may never report a Workspace `released` that it did not see
// deleted. The end-to-end chain (a real Controller taking a delete_workspace operation through every
// step) is the acceptance stack's job; these pin the transaction-scoped transitions and the give-up
// bound without standing up a Node.

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// workflowReleaseShape is the durable release fact plus the version it was written under.
type workflowReleaseShape struct {
	state      string
	reason     string
	releasedAt sql.NullTime
	version    int64
}

func readWorkflowRelease(t *testing.T, store *Store, runID string) workflowReleaseShape {
	t.Helper()
	var s workflowReleaseShape
	if err := store.Pool.QueryRow(`SELECT workspace_release_state, workspace_release_error, workspace_released_at, version
		FROM workflow_runs WHERE id=$1`, runID).Scan(&s.state, &s.reason, &s.releasedAt, &s.version); err != nil {
		t.Fatalf("read workflow release: %v", err)
	}
	return s
}

// finishWorkflowRunNow calls the scheduler's real terminal transition on a run whose workspace scene
// is staged. It runs in one transaction, exactly as a scheduler pass does.
func finishWorkflowRunNow(t *testing.T, store *Store, runID, status string) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		run := tx.one("SELECT version FROM workflow_runs WHERE id=$1", runID)
		finishWorkflowRun(tx, runID, run.N("version"), status, "", nil)
		return Object{}
	}); err != nil {
		t.Fatalf("finish workflow run: %v", err)
	}
}

// stageProvisionedWorkflowRun seeds a run whose Workspace was provisioned and settled ready with a
// connected Node, then settles the run terminal through the scheduler's own cleanup. It is the state
// every release transition starts from: the delete has been declared and the Node that would execute
// it is live.
func stageProvisionedWorkflowRun(t *testing.T, store *Store) (runID, wsID string) {
	t.Helper()
	runID, _, _, _, _ = seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to provision the workspace: %v", err)
	}
	wsID = settleSessionWorkspace(t, store, runID)
	bindConnectedNode(t, store, wsID)
	finishWorkflowRunNow(t, store, runID, "succeeded")
	return runID, wsID
}

func countWorkflowDeleteOperations(t *testing.T, store *Store, runID string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.workflow_run_id=$1 AND o.kind='delete_workspace'`, runID).Scan(&n); err != nil {
		t.Fatalf("count delete operations: %v", err)
	}
	return n
}

// A terminal run with a live Workspace commits its release intent and the delete declaration in the
// settling transaction. Repeating the cleanup writes nothing new: the intent is already durable and
// the existing operation is returned rather than duplicated.
func TestWorkflowRunCleanupRecordsTheReleaseIntent(t *testing.T) {
	store := dispatcherDB(t)
	runID, wsID := stageProvisionedWorkflowRun(t, store)

	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleaseReleasing || release.reason != "" || release.releasedAt.Valid {
		t.Fatalf("release after terminal cleanup = %+v; want releasing with no error and no release instant", release)
	}
	var deletedAt sql.NullTime
	if err := store.Pool.QueryRow(`SELECT deleted_at FROM workspaces WHERE id=$1`, wsID).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if deletedAt.Valid {
		t.Fatal("the terminal cleanup only declares the delete; it must not delete the Workspace itself")
	}
	if n := countWorkflowDeleteOperations(t, store, runID); n != 1 {
		t.Fatalf("%d delete operations declared; want exactly 1", n)
	}

	// The reconciliation pass calls the same cleanup again on every tick until the delete settles.
	// It must not bump the version or declare a second operation.
	before := readWorkflowRelease(t, store, runID)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		finishWorkflowRunCleanup(tx, runID)
		return Object{}
	}); err != nil {
		t.Fatalf("replayed cleanup: %v", err)
	}
	after := readWorkflowRelease(t, store, runID)
	if after.state != before.state || after.version != before.version {
		t.Fatalf("a replayed cleanup changed %+v to %+v", before, after)
	}
	if n := countWorkflowDeleteOperations(t, store, runID); n != 1 {
		t.Fatalf("a replayed cleanup declared %d delete operations; want 1", n)
	}
}

// The delete-completion hook is the only writer of `released`. Driven through the control plane's
// own `advance` at the cleanup step — the transaction that writes the operation `succeeded` and
// soft-deletes the Workspace — it records the release instant on the WorkflowRun, proving both the
// workflow-run binding in control.go and the B-side transition.
func TestWorkflowRunDeleteCompletionRecordsReleased(t *testing.T) {
	store := dispatcherDB(t)
	runID, wsID := stageProvisionedWorkflowRun(t, store)
	var opID, projectID string
	if err := store.Pool.QueryRow(`SELECT o.id, o.project_id FROM operations o WHERE o.workspace_id=$1 AND o.kind='delete_workspace'`, wsID).
		Scan(&opID, &projectID); err != nil {
		t.Fatalf("read delete operation: %v", err)
	}
	// The Controller has taken the operation and finished the data-delete effect; the next advance is
	// the cleanup step that deletes the Workspace row and settles the operation.
	if _, err := store.Pool.Exec(`UPDATE operations SET state='running', step='cleanup', controller_epoch=1, version=version+1 WHERE id=$1`, opID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`INSERT INTO external_effects(id,operation_id,project_id,workspace_id,kind,state,request,reconciled_epoch)
		VALUES($1,$2,$3,$4,'workspace_data_delete','succeeded','{}'::jsonb,1)`, newID(), opID, projectID, wsID); err != nil {
		t.Fatalf("seed completed effect: %v", err)
	}
	before := readWorkflowRelease(t, store, runID)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		o := tx.one("SELECT * FROM operations WHERE id=$1", opID)
		advance(tx, &ControlRequest{Body: Object{"epoch": int64(1), "version": o.N("version")}}, o)
		return Object{}
	}); err != nil {
		t.Fatalf("advance delete to done: %v", err)
	}

	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleaseReleased || release.reason != "" || !release.releasedAt.Valid {
		t.Fatalf("release after the delete completed = %+v; want released with an instant and no error", release)
	}
	var observed string
	var deletedAt sql.NullTime
	if err := store.Pool.QueryRow(`SELECT observed_state, deleted_at FROM workspaces WHERE id=$1`, wsID).Scan(&observed, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if observed != "deleted" || !deletedAt.Valid {
		t.Fatalf("workspace observed=%q deleted_at=%v; want the delete committed in the same transaction", observed, deletedAt)
	}

	// A replayed hook invocation finds `released` and writes nothing: the fact is already committed.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		if err := store.settleRunWorkspaceDeleted(tx, runID); err != nil {
			panic(databaseFailure{err})
		}
		return Object{}
	}); err != nil {
		t.Fatalf("replayed delete settle: %v", err)
	}
	after := readWorkflowRelease(t, store, runID)
	if after.state != workflowReleaseReleased || after.version != release.version {
		t.Fatalf("a replayed delete settle changed %+v to %+v", release, after)
	}
	if release.version != before.version+1 {
		t.Fatalf("the release took %d version bumps; want exactly 1", release.version-before.version)
	}
}

// A delete completion for a run that is still executing cannot be justified: no terminal transition
// declared that delete, so the hook refuses and the caller's transaction rolls back. Accepting it
// would record a Workspace release for a run whose nodes are still executing against that Workspace.
func TestWorkflowRunDeleteCompletionRefusesANonTerminalRun(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	before := readWorkflowRelease(t, store, runID)
	err := func() error {
		_, err := store.transact(context.Background(), func(tx *transaction) Object {
			if e := store.settleRunWorkspaceDeleted(tx, runID); e != nil {
				panic(databaseFailure{e})
			}
			return Object{}
		})
		return err
	}()
	if err == nil {
		t.Fatal("a delete completion for a running run must be refused")
	}
	if after := readWorkflowRelease(t, store, runID); after != before {
		t.Fatalf("a refused delete completion wrote %+v; want %+v", after, before)
	}
}

// A run whose graph never needed a Workspace records `not_required` at settlement. That is a
// different fact from `released`, and the two must never be conflated: nothing was ever released.
func TestWorkflowRunWithoutWorkspaceRecordsNotRequired(t *testing.T) {
	store := dispatcherDB(t)
	runID := seedWorkflowRunScene(t, store, wfGraph(
		[]Object{wfNode("s", "start", ""), wfNode("o", "output", "")},
		[][3]string{{"s", "o", ""}},
	))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleaseNotRequired || release.reason != "" || release.releasedAt.Valid {
		t.Fatalf("release = %+v; want not_required with no release instant", release)
	}
}

// D8's bound, for a WorkflowRun: a terminal run whose Workspace still holds a sandbox whose Node has
// been silent past the configured window is abandoned once. The unfinished delete operation is failed
// with the published unreachable-Node code, the Workspace row and its binding stay readable, and the
// run claims no release instant. A later pass does not re-declare the delete or re-enter the release.
func TestWorkflowRunGiveUpAbandonsAnUnreachableWorkspace(t *testing.T) {
	store := dispatcherDB(t)
	store.DeliveryUnreachableAfter = 30 * time.Minute
	runID, wsID := stageProvisionedWorkflowRun(t, store)
	if _, err := store.Pool.Exec(`UPDATE node_instances SET last_seen_at=clock_timestamp()-interval '2 hours' WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatalf("age the Node heartbeat: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance with the Node gone: %v", err)
	}

	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleaseAbandoned || release.reason != workflowReleaseAbandonedReason || release.releasedAt.Valid {
		t.Fatalf("release = %+v; want abandoned/%s with no release instant", release, workflowReleaseAbandonedReason)
	}
	var opState, opError string
	if err := store.Pool.QueryRow(`SELECT state, COALESCE(error_code,'') FROM operations
		WHERE workspace_id=$1 AND kind='delete_workspace' ORDER BY created_at DESC, id DESC LIMIT 1`, wsID).
		Scan(&opState, &opError); err != nil {
		t.Fatalf("read delete operation: %v", err)
	}
	if opState != "failed" || opError != "node_unavailable" {
		t.Fatalf("delete operation = %s/%q; want failed/node_unavailable", opState, opError)
	}
	var deletedAt sql.NullTime
	var binding sql.NullString
	if err := store.Pool.QueryRow(`SELECT deleted_at, workflow_run_id FROM workspaces WHERE id=$1`, wsID).Scan(&deletedAt, &binding); err != nil {
		t.Fatal(err)
	}
	if deletedAt.Valid || binding.String != runID {
		t.Fatalf("the abandoned Workspace must stay live and bound (deleted_at=%v run=%q)", deletedAt, binding.String)
	}

	// A second pass neither re-declares another delete nor moves the release: the abandoned run is out
	// of both scans.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if after := readWorkflowRelease(t, store, runID); after.state != workflowReleaseAbandoned || after.reason != release.reason {
		t.Fatalf("a second pass changed %+v to %+v", release, after)
	}
	if n := countWorkflowDeleteOperations(t, store, runID); n != 1 {
		t.Fatalf("a second pass declared %d delete operations; want only the failed one", n)
	}
}

// The give-up window is a bound on silence, not a reaction to any silence: a Node heard from inside
// the window holds the release, and an unconfigured window fails closed — the deployment that never
// set the limit loses only the give-up, never a Workspace it was still waiting for.
func TestWorkflowRunReleaseHoldsWithinTheWindowAndWithoutOne(t *testing.T) {
	for _, tc := range []struct {
		name      string
		window    time.Duration
		heartbeat time.Duration
	}{
		{name: "heartbeat inside the window", window: 30 * time.Minute, heartbeat: 5 * time.Minute},
		{name: "unconfigured window", window: 0, heartbeat: 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := dispatcherDB(t)
			store.DeliveryUnreachableAfter = tc.window
			runID, wsID := stageProvisionedWorkflowRun(t, store)
			if _, err := store.Pool.Exec(`UPDATE node_instances SET last_seen_at=clock_timestamp()-make_interval(secs => $2) WHERE workspace_id=$1`, wsID, tc.heartbeat.Seconds()); err != nil {
				t.Fatalf("age the heartbeat: %v", err)
			}
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Fatalf("advance: %v", err)
			}
			release := readWorkflowRelease(t, store, runID)
			if release.state != workflowReleaseReleasing || release.reason != "" {
				t.Fatalf("release = %+v; want still releasing", release)
			}
			if n := countWorkflowDeleteOperations(t, store, runID); n != 1 {
				t.Fatalf("%d delete operations; want the declared one still outstanding", n)
			}
		})
	}
}

// A Workspace whose sandbox is already terminated is not an abandoned release: no Node is left to be
// unreachable, and the remaining cleanup runs against the Substrate without one. Abandoning it would
// report a release as failed while a live Controller was still completing it.
func TestWorkflowRunReleaseDoesNotAbandonATerminatedSandbox(t *testing.T) {
	store := dispatcherDB(t)
	store.DeliveryUnreachableAfter = 30 * time.Minute
	runID, wsID := stageProvisionedWorkflowRun(t, store)
	if _, err := store.Pool.Exec(`UPDATE sandbox_instances SET terminated_at=clock_timestamp(), observed_state='terminated' WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(`UPDATE node_instances SET ended_at=clock_timestamp(), connection_state='ended', version=version+1 WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if release := readWorkflowRelease(t, store, runID); release.state != workflowReleaseReleasing {
		t.Fatalf("release = %+v; want still releasing while the delete can proceed without a Node", release)
	}
}

// A Workspace deleted by another path — a project deletion deletes every Workspace inline and never
// fires the per-run delete hook — is healed to `released` by the reconciliation pass, with the
// Workspace's own deletion instant as the release fact. The state is not left `releasing` beside a
// Workspace that no longer exists, and it is not claimed without evidence.
func TestWorkflowRunReleaseHealsAWorkspaceDeletedByAnotherPath(t *testing.T) {
	store := dispatcherDB(t)
	runID, wsID := stageProvisionedWorkflowRun(t, store)
	if _, err := store.Pool.Exec(`UPDATE workspaces SET observed_state='deleted', deleted_at=clock_timestamp(), version=version+1 WHERE id=$1`, wsID); err != nil {
		t.Fatal(err)
	}
	var deletedAt time.Time
	if err := store.Pool.QueryRow(`SELECT deleted_at FROM workspaces WHERE id=$1`, wsID).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleaseReleased || !release.releasedAt.Valid {
		t.Fatalf("release = %+v; want released with the Workspace's deletion instant", release)
	}
	if !release.releasedAt.Time.Equal(deletedAt) {
		t.Fatalf("release instant = %v; want the Workspace's own deletion instant %v", release.releasedAt.Time, deletedAt)
	}
}
