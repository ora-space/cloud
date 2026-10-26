package core

// Ordering evidence for workflow-run cancellation (circus#75 acceptance items 2 and 6, G-026;
// specs decisions/cloud/workflow 0-cloud-side-scheduler-owns-workflow-execution.md D4/D6). The
// neighboring workflow_run_cancel_db_test.go cases drive one writer at a time; these drive two at
// once and assert the complete set of permitted final rows, not the common one.
//
// Every writer and the scheduler pass share the store's one advisory transaction lock, so the
// interleavings are serialized rather than true simultaneous commits — which is exactly the property
// the ordering evidence is about: whichever commits first decides, the second observes it in
// committed state, and no combination leaves the run, the node and the session disagreeing.

import (
	"context"
	"database/sql"
	"sync"
	"testing"
)

// commitSessionResult stores one session's terminal result the way the Node's callback does: the
// result transaction alone, without advancing. It returns its error instead of failing the test so a
// caller can race it against another writer; callers in the test goroutine turn the error back into
// a failure.
func commitSessionResult(store *Store, runID, execution, nodeService, reason string) error {
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "result": Object{"outcome": "agent_session_ended", "reason": reason, "node": Object{"nodeId": nodeService}}, "sequence": int64(1), "event": "ended"}}, true)
		return Object{}
	})
	return err
}

// assertWorkflowRunReleaseDeclared proves S5's half of the terminal settlement: the transaction that
// settled the run also recorded the release intent for its live Workspace, and nothing claims the
// Workspace is gone before Cloud observed the delete complete.
func assertWorkflowRunReleaseDeclared(t *testing.T, store *Store, runID string) {
	t.Helper()
	release := readWorkflowRelease(t, store, runID)
	if release.state != workflowReleasePending || release.reason != "" || release.releasedAt.Valid {
		t.Fatalf("unsaved terminal release = state %q reason %q released_at %v; want pending with no released_at",
			release.state, release.reason, release.releasedAt)
	}
}

// prepareRunningCancelScene advances a seeded run into a live, dispatched session and returns the
// facts the ordering cases race over.
func prepareRunningCancelScene(t *testing.T, store *Store) (runID, tid, wid, nodeService, execution string) {
	t.Helper()
	runID, tid, _, _, _ = seedWorkflowSessionScene(t, store)
	wid = workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance to claim the run: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution = dispatchSession(t, store, runID, "a", nodeService, spec)
	return runID, tid, wid, nodeService, execution
}

// The cancellation writer and the scheduler pass run concurrently. Whichever wins, the pass that
// observes the request releases exactly one end request for the live session, and the run stays
// running until that session reports — the request never settles a live execution from under its
// node. The session's own terminal result then settles the final row: run cancelled, node
// cancelled, Workspace deletion declared, request retained.
func TestWorkflowRunCancelRacesTheSchedulerPass(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, wid, nodeService, execution := prepareRunningCancelScene(t, store)
	caller := cancelCaller(t, store, tid)

	var cancelErr error
	var cancelStatus int
	var instant string
	start := make(chan struct{})
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		<-start
		out, status, err := workflowRunCancelCall(t, store, tid, wid, runID, "wf-cancel-pass-race", caller)
		cancelErr, cancelStatus, instant = err, status, out.S("cancelRequestedAt")
	}()
	go func() {
		defer group.Done()
		<-start
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			// The pass swallows per-run failures by design; a scan failure is the only one that
			// reaches here, and it must not be hidden.
			t.Errorf("scheduler pass: %v", err)
		}
	}()
	close(start)
	group.Wait()

	if cancelErr != nil || cancelStatus != 202 || instant == "" {
		t.Fatalf("the request: status=%d instant=%q err=%v; want 202 carrying the recorded instant", cancelStatus, instant, cancelErr)
	}
	// If the pass ran first, the request is the only side of the race that happened; the next pass
	// must react to it. If the pass ran second, this is a replay that releases nothing new.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the race: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "running" || runErr != "" {
		t.Fatalf("after the race the run is %q error %q states=%v; want the live session held", status, runErr, states)
	}
	commands := claimCommands(t, store)
	if len(commands) != 1 {
		t.Fatalf("the race released %d commands; want exactly the one end request: %v", len(commands), commands)
	}
	cmd := commands[0]
	if cmd.S("kind") != "end_session" || cmd.O("body").S("reason") != "cancelled" ||
		cmd.S("runId") != runID || cmd.S("executionId") != execution || cmd.S("nodeId") != nodeService {
		t.Fatalf("command = %v; want end_session/cancelled for %s/%s/%s", cmd, runID, execution, nodeService)
	}

	reportSessionEnd(t, store, runID, execution, nodeService, "cancelled")
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("final run = %q error %q; want cancelled", status, runErr)
	}
	if agent == nil || agent.S("status") != "cancelled" || agent.S("error") != "" || agent.O("output").S("reason") != "cancelled" {
		t.Fatalf("final node a = %v; want cancelled with reason cancelled", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
	assertWorkflowRunReleaseDeclared(t, store, runID)
	if recorded := readCancelRequestedAt(t, store, runID); !recorded.Valid {
		t.Fatal("the final state lost the recorded request")
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the settlement: %v", err)
	}
	if _, _, _, again := readWorkflowRun(t, store, runID); again != version {
		t.Fatalf("a pass after settlement bumped the final row's version from %d to %d", version, again)
	}
}

// A session's own terminal callback and the cancellation request race. Both orders are permitted,
// and each has one consistent final row: if the request commits while the run is still running, the
// run is cancelled and the node keeps the result that actually ran; if the callback's pass settles
// the run first, the request is refused as no-longer-cancellable and nothing is recorded. A request
// is never recorded on a succeeded run, and a cancelled run never loses the result.
func TestWorkflowRunCancelAndTheSessionCallbackRace(t *testing.T) {
	// Each attempt gets its own schema: the scene seeds one personal-model default per schema, and the
	// outcome branch is read from whichever ordering the shared advisory lock produced, so repeating
	// covers both without assuming a particular winner.
	for attempt := 0; attempt < 4; attempt++ {
		store := dispatcherDB(t)
		runID, tid, wid, nodeService, execution := prepareRunningCancelScene(t, store)
		caller := cancelCaller(t, store, tid)

		var cancelOut Object
		var cancelErr error
		var callbackErr error
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			cancelOut, _, cancelErr = workflowRunCancelCall(t, store, tid, wid, runID, "wf-cancel-callback-race", caller)
		}()
		go func() {
			defer group.Done()
			<-start
			callbackErr = commitSessionResult(store, runID, execution, nodeService, "idle_timeout")
			if callbackErr == nil {
				callbackErr = store.AdvanceWorkflowRunsOnce(context.Background())
			}
		}()
		close(start)
		group.Wait()

		if callbackErr != nil {
			t.Fatalf("attempt %d: the session callback: %v", attempt, callbackErr)
		}
		status, runErr, states, _ := readWorkflowRun(t, store, runID)
		agent, _ := asWorkflowObject(states["a"])
		if runErr != "" || agent == nil || agent.S("status") != "succeeded" || agent.O("output").S("reason") != "idle_timeout" {
			t.Fatalf("attempt %d: final state run=%q error=%q states=%v; want the idle_timeout result kept", attempt, status, runErr, states)
		}
		recorded := readCancelRequestedAt(t, store, runID)
		if cancelErr == nil {
			// Accepted: the request was recorded while the run was non-terminal, so it decides the run.
			if cancelOut.S("cancelRequestedAt") == "" || !recorded.Valid {
				t.Fatalf("attempt %d: an accepted request reported %q but recorded %v", attempt, cancelOut.S("cancelRequestedAt"), recorded)
			}
			if status != "cancelled" {
				t.Fatalf("attempt %d: the accepted request left the run %q; want cancelled", attempt, status)
			}
			t.Logf("attempt %d: the request committed first; the callback's result settled the node and the run is cancelled", attempt)
		} else {
			// Refused: the callback had already settled the run, and a refusal records nothing.
			if code := ErrorCode(cancelErr).Code; code != "run_not_cancellable" {
				t.Fatalf("attempt %d: the request failed with %q; want run_not_cancellable", attempt, code)
			}
			if recorded.Valid {
				t.Fatalf("attempt %d: a refused request recorded %v", attempt, recorded.Time)
			}
			if status != "succeeded" {
				t.Fatalf("attempt %d: a refused request left the run %q; want succeeded", attempt, status)
			}
			t.Logf("attempt %d: the callback's pass settled the run first; the request was refused", attempt)
		}
		assertRunWorkspaceReleased(t, store, runID)
		assertWorkflowRunReleaseDeclared(t, store, runID)
		if status == "cancelled" {
			// The recorded instant is the seam the outcome is read from; a settled run keeps it.
			if again, _, err := workflowRunCancelCall(t, store, tid, wid, runID, "wf-cancel-callback-replay", caller); err != nil || again.S("cancelRequestedAt") != cancelOut.S("cancelRequestedAt") {
				t.Fatalf("attempt %d: a repeat after settlement: instant=%q err=%v; want the recorded %q", attempt, again.S("cancelRequestedAt"), err, cancelOut.S("cancelRequestedAt"))
			}
		}
	}
}

// The end confirmation window expiring and a late session callback race. The pass either settles the
// run cancelled and leaves the callback's result on the execution row, or reads the committed result
// first and keeps the node's own outcome; the run is cancelled either way. The node never goes back
// to running, the result is never discarded, and the terminal row is not rewritten afterwards.
func TestWorkflowRunCancelWindowAndALateCallbackRace(t *testing.T) {
	for attempt := 0; attempt < 4; attempt++ {
		store := dispatcherDB(t)
		runID, tid, wid, nodeService, execution := prepareRunningCancelScene(t, store)
		caller := cancelCaller(t, store, tid)
		mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-window-race", caller)
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatalf("attempt %d: advance after the request: %v", attempt, err)
		}
		var requests int
		if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_commands
			WHERE workflow_run_id=$1 AND kind='end_session'`, runID).Scan(&requests); err != nil {
			t.Fatalf("attempt %d: count the end request: %v", attempt, err)
		}
		if requests != 1 {
			t.Fatalf("attempt %d: %d end requests; want the one end request", attempt, requests)
		}
		// The Node never confirms; age the one end request past the confirmation window on the
		// database clock so the next pass is the cancellation's own settlement.
		if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes'
			WHERE workflow_run_id=$1`, runID); err != nil {
			t.Fatalf("attempt %d: age the end request: %v", attempt, err)
		}

		var callbackErr error
		start := make(chan struct{})
		var group sync.WaitGroup
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			callbackErr = commitSessionResult(store, runID, execution, nodeService, "idle_timeout")
		}()
		go func() {
			defer group.Done()
			<-start
			if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
				t.Errorf("attempt %d: scheduler pass: %v", attempt, err)
			}
		}()
		close(start)
		group.Wait()
		if callbackErr != nil {
			t.Fatalf("attempt %d: report the late callback: %v", attempt, callbackErr)
		}

		// Drain the independent save barrier once both racing facts are committed. Execution and
		// node conclusions stay immutable; late stop evidence may settle delivery from pending.
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		status, runErr, states, version := readWorkflowRun(t, store, runID)
		agent, _ := asWorkflowObject(states["a"])
		if status != "cancelled" || runErr != "" {
			t.Fatalf("attempt %d: final run = %q error %q states=%v; want cancelled", attempt, status, runErr, states)
		}
		if agent == nil {
			t.Fatalf("attempt %d: final node a is missing", attempt)
		}
		switch {
		case agent.S("status") == "succeeded" && agent.O("output").S("reason") == "idle_timeout":
			// The pass read the committed result: the node keeps what actually ran.
			t.Logf("attempt %d: the pass read the committed result; node a kept idle_timeout", attempt)
		case agent.S("status") == "cancelled" && agent.O("output").S("reason") == "cancelled" && agent.S("error") == "":
			// The pass settled first: the callback's result belongs to the execution row only.
			t.Logf("attempt %d: the pass settled first; the late result stayed on the execution row", attempt)
		default:
			t.Fatalf("attempt %d: final node a = %v; want either the idle_timeout result or cancelled", attempt, states["a"])
		}
		var result sql.NullString
		if err := store.Pool.QueryRow(`SELECT result FROM node_executions
			WHERE operation_id=$1 AND workflow_node_id='a' AND kind='agent_session'`, runID).Scan(&result); err != nil {
			t.Fatalf("attempt %d: read the execution result: %v", attempt, err)
		}
		if !result.Valid {
			t.Fatalf("attempt %d: the late result was discarded; want it kept on the execution row", attempt)
		}
		assertRunWorkspaceReleased(t, store, runID)
		assertWorkflowRunReleaseDeclared(t, store, runID)
		if !readCancelRequestedAt(t, store, runID).Valid {
			t.Fatalf("attempt %d: the final state lost the recorded request", attempt)
		}
		if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
			t.Fatalf("attempt %d: advance after settlement: %v", attempt, err)
		}
		if _, _, _, again := readWorkflowRun(t, store, runID); again != version {
			t.Fatalf("attempt %d: a pass after settlement bumped the final row's version from %d to %d", attempt, version, again)
		}
	}
}
