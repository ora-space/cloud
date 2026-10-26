package core

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// DB tests for WorkflowRun cancellation (circus#347; specs ADR
// 0-cloud-side-scheduler-owns-workflow-execution.md D4/D6).
//
// D4 splits cancellation in two on purpose: the public request records one instant and answers, and
// the scheduler reacts to it on a later pass. The two halves meet only on
// `workflow_runs.cancel_requested_at`, so these tests drive the shipped writer exactly as the router
// does (`Store.Public`, real transaction, real idempotency record) and then the shipped pass
// (`AdvanceWorkflowRunsOnce`). A writer that recorded somewhere else, or a reaction that read
// something else, fails here rather than passing its half alone.

// workflowRunCancelCall drives the public cancellation request the way the router does.
func workflowRunCancelCall(t *testing.T, store *Store, tid, wid, runID, key string, caller *Claims) (Object, int, error) {
	t.Helper()
	return store.Public(context.Background(), &PublicRequest{
		Method: "POST", Key: key,
		Path:     "/api/v1/tenants/" + tid + "/workflows/" + wid + "/runs/" + runID + "/cancel",
		TenantID: tid, WorkflowID: wid, RunID: runID,
		Body: Object{}, Identity: caller,
	})
}

// mustCancelWorkflowRun records one cancellation request and returns the instant it reported.
func mustCancelWorkflowRun(t *testing.T, store *Store, tid, wid, runID, key string, caller *Claims) string {
	t.Helper()
	out, status, err := workflowRunCancelCall(t, store, tid, wid, runID, key, caller)
	if err != nil {
		t.Fatalf("POST workflow run cancel: %v", err)
	}
	if status != 202 || out.S("cancelRequestedAt") == "" {
		t.Fatalf("POST workflow run cancel: want 202 carrying the recorded instant, got %d %v", status, out)
	}
	return out.S("cancelRequestedAt")
}

// workflowRunWorkflowID reads the workflow a run belongs to, which the cancel route needs to address
// the run.
func workflowRunWorkflowID(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var wid string
	if err := store.Pool.QueryRow(`SELECT workflow_id FROM workflow_runs WHERE id=$1`, runID).Scan(&wid); err != nil {
		t.Fatalf("read run workflow id: %v", err)
	}
	return wid
}

// readCancelRequestedAt reads the request straight from the row: the column is the only seam between
// the writer and the reaction.
func readCancelRequestedAt(t *testing.T, store *Store, runID string) sql.NullTime {
	t.Helper()
	var at sql.NullTime
	if err := store.Pool.QueryRow(`SELECT cancel_requested_at FROM workflow_runs WHERE id=$1`, runID).Scan(&at); err != nil {
		t.Fatalf("read cancel_requested_at: %v", err)
	}
	return at
}

// The request records exactly one fact and returns the database's instant: the run is otherwise
// untouched (still pending, never started, no node states), and a repeat — even under a fresh
// idempotency key — returns the instant the first one recorded rather than replacing it.
func TestWorkflowRunCancelRequestRecordsTheInstant(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	caller := cancelCaller(t, store, tid)

	instant := mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-1", caller)
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	var started sql.NullTime
	if err := store.Pool.QueryRow(`SELECT started_at FROM workflow_runs WHERE id=$1`, runID).Scan(&started); err != nil {
		t.Fatalf("read started_at: %v", err)
	}
	if status != "pending" || runErr != "" || len(states) != 0 || started.Valid {
		t.Fatalf("recording the request moved the run: status=%q error=%q states=%v started=%v; want it untouched and unstarted", status, runErr, states, started)
	}
	if again := mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-2", caller); again != instant {
		t.Fatalf("a repeat must return the recorded instant %s, got %s", instant, again)
	}
	recorded := readCancelRequestedAt(t, store, runID)
	if !recorded.Valid {
		t.Fatal("the request was not recorded on the run row")
	}
	reported, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatalf("the endpoint must report an instant: %v", err)
	}
	if !reported.Equal(recorded.Time) {
		t.Fatalf("the row carries %v; want the reported instant %s", recorded.Time, reported)
	}
}

// On a run carrying frozen executor bindings only the initiator and the tenant's administrators may
// cancel; an ordinary active member is refused before learning whether the run was cancellable.
func TestWorkflowRunCancelRequiresTheInitiatorOrAnAdmin(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	member, memberID := modelUser(t, store, "wf-cancel-member")
	if _, err := store.Pool.Exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')`, tid, memberID); err != nil {
		t.Fatalf("join the member to the tenant: %v", err)
	}

	_, _, err := workflowRunCancelCall(t, store, tid, wid, runID, "wf-cancel-member-1", member)
	if err == nil || ErrorCode(err).Code != "workflow_run_cancel_forbidden" {
		t.Fatalf("a non-initiator member's request: err=%v; want workflow_run_cancel_forbidden", err)
	}
	if recorded := readCancelRequestedAt(t, store, runID); recorded.Valid {
		t.Fatalf("a refused request recorded %v", recorded.Time)
	}
	// The run's initiator, reached through a fresh identity binding, may cancel.
	if instant := mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-initiator-1", cancelCaller(t, store, tid)); instant == "" {
		t.Fatal("the initiator's request recorded nothing")
	}
}

// Cancel-before-start is the whole cancellation: the run never claims, so it stamps no started_at,
// validates no graph, provisions no Workspace and leaves no unregistered work. The request survives
// the settlement, and later passes do not touch the terminal run.
func TestWorkflowRunCancelBeforeStartNeverStarts(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-before-start", cancelCaller(t, store, tid))

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the request: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	var started, finished sql.NullTime
	if err := store.Pool.QueryRow(`SELECT started_at, finished_at FROM workflow_runs WHERE id=$1`, runID).Scan(&started, &finished); err != nil {
		t.Fatalf("read run timestamps: %v", err)
	}
	if status != "cancelled" || runErr != "" || len(states) != 0 {
		t.Fatalf("run settled %q error %q states=%v; want cancelled with no node states", status, runErr, states)
	}
	if started.Valid {
		t.Fatalf("a run cancelled before its claim stamped started_at %v", started.Time)
	}
	if !finished.Valid {
		t.Fatal("a cancelled run carries no finished_at")
	}
	var workspaces, work, deletes int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM workspaces WHERE workflow_run_id=$1`, runID).Scan(&workspaces); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM execution_work WHERE workflow_run_id=$1`, runID).Scan(&work); err != nil {
		t.Fatalf("count execution work: %v", err)
	}
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations o JOIN workspaces w ON w.id=o.workspace_id
		WHERE w.workflow_run_id=$1 AND o.kind='delete_workspace'`, runID).Scan(&deletes); err != nil {
		t.Fatalf("count delete operations: %v", err)
	}
	if workspaces != 0 || work != 0 || deletes != 0 {
		t.Fatalf("a cancelled-before-start run left workspaces=%d work=%d deletes=%d; want none", workspaces, work, deletes)
	}
	if !readCancelRequestedAt(t, store, runID).Valid {
		t.Fatal("the settlement cleared the recorded request")
	}
	// The run is terminal and out of the scan: a later pass changes nothing.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after terminal: %v", err)
	}
	if _, _, _, again := readWorkflowRun(t, store, runID); again != version {
		t.Fatalf("a later pass bumped the terminal run's version from %d to %d", version, again)
	}
}

// A running run with an in-flight session is stopped through the existing EndSession relay: the
// request is scoped to that node's session, a repeat is a replay rather than a second request, and
// the node settles from the session's own terminal result while the run settles cancelled. The
// recorded instant outlives the settlement, so a repeat after the outcome still returns it.
func TestWorkflowRunCancelStopsTheRunningSession(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	caller := cancelCaller(t, store, tid)

	instant := mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-running-1", caller)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the request: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "running" || runErr != "" || agent == nil || agent.S("status") != "running" {
		t.Fatalf("after the reaction: run %q error %q states=%v; want it held with a running session", status, runErr, states)
	}
	commands := claimCommands(t, store)
	if len(commands) != 1 {
		t.Fatalf("released %d commands; want the one end request: %v", len(commands), commands)
	}
	cmd := commands[0]
	if cmd.S("kind") != "end_session" || cmd.O("body").S("reason") != "cancelled" {
		t.Fatalf("command = %v; want end_session/cancelled", cmd)
	}
	if cmd.S("runId") != runID || cmd.S("executionId") != execution || cmd.S("nodeId") != nodeService {
		t.Fatalf("command destination = run %q execution %q node %q; want %q/%q/%q",
			cmd.S("runId"), cmd.S("executionId"), cmd.S("nodeId"), runID, execution, nodeService)
	}
	// Recomputing while the request is outstanding is a replay: still one request.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance while ending: %v", err)
	}
	if again := claimCommands(t, store); len(again) != 1 || again[0].S("commandId") != cmd.S("commandId") {
		t.Fatalf("a re-tick released another command: %v", again)
	}

	reportSessionEnd(t, store, runID, execution, nodeService, "cancelled")
	status, runErr, states, _ = readWorkflowRun(t, store, runID)
	agent, _ = asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled", status, runErr, states)
	}
	if agent == nil || agent.S("status") != "cancelled" || agent.S("error") != "" || agent.O("output").S("reason") != "cancelled" {
		t.Fatalf("node a = %v; want cancelled with no error and reason cancelled", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
	if !readCancelRequestedAt(t, store, runID).Valid {
		t.Fatal("the settlement cleared the recorded request")
	}
	if again := mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-running-2", caller); again != instant {
		t.Fatalf("a repeat after the settlement must return %s, got %s", instant, again)
	}
}

// A result the session committed before the reaction keeps the node's own outcome — that result is
// what actually ran — while the run is still cancelled, because the request was recorded while the
// run was non-terminal.
func TestWorkflowRunCancelKeepsACommittedSessionResult(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-result", cancelCaller(t, store, tid))
	// The real end commits before the next pass reads the request.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "result": Object{"outcome": "agent_session_ended", "reason": "idle_timeout", "node": Object{"nodeId": nodeService}}, "sequence": int64(1), "event": "ended"}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("report result: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the result: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled", status, runErr, states)
	}
	if agent == nil || agent.S("status") != "succeeded" || agent.O("output").S("reason") != "idle_timeout" {
		t.Fatalf("node a = %v; want the committed idle_timeout result kept", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// The end request's confirmation is bounded exactly as it is for an idle session: once the command
// has been outstanding past the window with no session terminal, the node settles cancelled and the
// run finishes. A result arriving after that is kept on the execution row and rewrites nothing.
func TestWorkflowRunCancelEndWindowSettlesCancelled(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-window", cancelCaller(t, store, tid))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the request: %v", err)
	}
	if got := claimCommands(t, store); len(got) != 1 {
		t.Fatalf("commands = %v; want the one end request", got)
	}
	// A Node that never claims the command leaves it outstanding; age it past the window on the
	// database clock.
	if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes'
		WHERE workflow_run_id=$1`, runID); err != nil {
		t.Fatalf("age the end request: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance past the window: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled", status, runErr, states)
	}
	if agent == nil || agent.S("status") != "cancelled" {
		t.Fatalf("node a = %v; want cancelled", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
	// The Node comes back after the run is terminal: the result is kept on the execution row and
	// the terminal run is not rewritten.
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	afterStatus, afterErr, afterStates, afterVersion := readWorkflowRun(t, store, runID)
	if afterStatus != status || afterErr != runErr || afterVersion != version {
		t.Fatalf("the late result moved the run to %q error %q version %d; want %q error %q version %d",
			afterStatus, afterErr, afterVersion, status, runErr, version)
	}
	if afterAgent, _ := asWorkflowObject(afterStates["a"]); afterAgent == nil || afterAgent.S("status") != "cancelled" {
		t.Fatalf("the late result rewrote node a to %v; want cancelled", afterStates["a"])
	}
	var result sql.NullString
	if err := store.Pool.QueryRow(`SELECT result FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id='a' AND kind='agent_session'`, runID).Scan(&result); err != nil {
		t.Fatalf("read the execution result: %v", err)
	}
	if !result.Valid {
		t.Fatal("the late result was discarded; want it kept on the execution row")
	}
}

// A Node that can no longer report cannot confirm an end request, so the cancellation wins over the
// unreachable settlement: the node and the run settle cancelled rather than failed, and no end
// request is released to a Node that is gone.
func TestWorkflowRunCancelWinsOverAnUnreachableNode(t *testing.T) {
	store := dispatcherDB(t)
	store.DeliveryUnreachableAfter = 30 * time.Minute
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	dispatchSession(t, store, runID, "a", nodeService, spec)
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-unreachable", cancelCaller(t, store, tid))
	if _, err := store.Pool.Exec(`UPDATE node_instances SET last_seen_at=clock_timestamp()-interval '2 hours' WHERE workspace_id=$1`, wsID); err != nil {
		t.Fatalf("age the Node heartbeat: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance with the Node gone: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled rather than node_unavailable", status, runErr, states)
	}
	if agent == nil || agent.S("status") != "cancelled" {
		t.Fatalf("node a = %v; want cancelled", states["a"])
	}
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("released %v to a Node that is gone; want no end request", got)
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// The scheduler reuses the end request the node already has instead of upgrading it: its reason may
// already have been acted on, and rewriting a released request would change it after the fact. The
// run still settles cancelled and the node keeps the result the session actually reported.
func TestWorkflowRunCancelReusesTheExistingEndRequest(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	takeOverWorkflowEvents(t, store, runID, execution, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after turn end: %v", err)
	}
	first := claimCommands(t, store)
	if len(first) != 1 || first[0].O("body").S("reason") != "idle_timeout" {
		t.Fatalf("commands = %v; want the one idle_timeout end request", first)
	}
	mustCancelWorkflowRun(t, store, tid, wid, runID, "wf-cancel-reuse", cancelCaller(t, store, tid))
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after the request: %v", err)
	}
	again := claimCommands(t, store)
	if len(again) != 1 || again[0].S("commandId") != first[0].S("commandId") || again[0].O("body").S("reason") != "idle_timeout" {
		t.Fatalf("commands after the request = %v; want the existing idle_timeout request unchanged", again)
	}
	// The receipt batch already consumed sequence 1, so the session's terminal event is next.
	reportSessionEndAt(t, store, runID, execution, nodeService, "idle_timeout", 2)
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "cancelled" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want cancelled", status, runErr, states)
	}
	if agent == nil || agent.S("status") != "succeeded" || agent.O("output").S("reason") != "idle_timeout" {
		t.Fatalf("node a = %v; want the session's own succeeded result kept", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// A run that already settled on its own cannot be cancelled: there is no live execution to stop, so
// the request is refused rather than recorded as if it had decided anything.
func TestWorkflowRunCancelAfterTheRunSettledIsRefused(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	if status, _, _, _ := readWorkflowRun(t, store, runID); status != "succeeded" {
		t.Fatalf("precondition: run settled %q; want succeeded", status)
	}

	_, _, err := workflowRunCancelCall(t, store, tid, wid, runID, "wf-cancel-terminal", cancelCaller(t, store, tid))
	if err == nil || ErrorCode(err).Code != "run_not_cancellable" {
		t.Fatalf("a terminal run's request: err=%v; want run_not_cancellable", err)
	}
	if recorded := readCancelRequestedAt(t, store, runID); recorded.Valid {
		t.Fatalf("a refused request recorded %v", recorded.Time)
	}
}

// Two callers racing to cancel the same run still record one request: the transactions serialize on
// the store's advisory lock, whichever runs second reads the instant the first recorded, and both
// answers carry it. The row keeps one instant and one request-caused version bump, so a race cannot
// rewrite the outcome the run is settled from.
func TestWorkflowRunCancelRacesToOneRequest(t *testing.T) {
	store := dispatcherDB(t)
	runID, tid, _, _, _ := seedWorkflowSessionScene(t, store)
	wid := workflowRunWorkflowID(t, store, runID)
	caller := cancelCaller(t, store, tid)
	_, _, _, versionBefore := readWorkflowRun(t, store, runID)

	type answer struct {
		instant string
		status  int
		err     error
	}
	answers := make([]answer, 2)
	start := make(chan struct{})
	var group sync.WaitGroup
	for i, key := range []string{"wf-cancel-race-a", "wf-cancel-race-b"} {
		group.Add(1)
		go func(i int, key string) {
			defer group.Done()
			<-start
			out, status, err := workflowRunCancelCall(t, store, tid, wid, runID, key, caller)
			answers[i] = answer{out.S("cancelRequestedAt"), status, err}
		}(i, key)
	}
	close(start)
	group.Wait()

	for i, got := range answers {
		if got.err != nil || got.status != 202 || got.instant == "" {
			t.Fatalf("request %d: status=%d instant=%q err=%v; want 202 carrying the recorded instant", i, got.status, got.instant, got.err)
		}
	}
	if answers[0].instant != answers[1].instant {
		t.Fatalf("the race returned two instants %s and %s; want the one the first request recorded", answers[0].instant, answers[1].instant)
	}
	reported, err := time.Parse(time.RFC3339, answers[0].instant)
	if err != nil {
		t.Fatalf("the endpoint must report an instant: %v", err)
	}
	if recorded := readCancelRequestedAt(t, store, runID); !recorded.Valid || !recorded.Time.Equal(reported) {
		t.Fatalf("the row carries %v; want the instant both answers reported (%s)", recorded, reported)
	}
	status, runErr, states, versionAfter := readWorkflowRun(t, store, runID)
	if status != "pending" || runErr != "" || len(states) != 0 {
		t.Fatalf("the race moved the run: status=%q error=%q states=%v; want it untouched", status, runErr, states)
	}
	if versionAfter != versionBefore+1 {
		t.Fatalf("the request bumped the run's version from %d to %d; want exactly one request-caused bump", versionBefore, versionAfter)
	}
}
