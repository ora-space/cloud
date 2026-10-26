package core

// DB tests for circus#299 option A: a dispatched workflow agent session that produces neither a
// result nor a successful `turnEnded` within the configured turn window settles failed with
// Cloud's own `session_turn_deadline_exceeded`. The window is judged on the database clock against
// the execution row's `created_at`, so these tests age that column rather than sleeping, and they
// assert the ordering the owner fixed: a committed result wins, an unreachable Node is still
// reported as `node_unavailable`, a successful turn end keeps the end-confirmation path, and only a
// session with neither evidence nor reachability becomes the turn deadline.

import (
	"context"
	"testing"
	"time"
)

// ageWorkflowSessionExecution moves a registered session execution's registered instant into the
// past on the database clock — the same age a long-running or wedged session reaches in production.
func ageWorkflowSessionExecution(t *testing.T, store *Store, execution, age string) {
	t.Helper()
	if _, err := store.Pool.Exec(`UPDATE node_executions SET created_at=clock_timestamp()-make_interval(secs=>$2)
		WHERE execution_id=$1`, execution, age); err != nil {
		t.Fatalf("age execution %s: %v", execution, err)
	}
}

// dispatchedWorkflowSession stages one run with its agent node committed running and its session
// dispatched, which is the state the turn window judges.
func dispatchedWorkflowSession(t *testing.T) (store *Store, runID, nodeService, execution string) {
	t.Helper()
	store = dispatcherDB(t)
	runID, _, _, _, _ = seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution = dispatchSession(t, store, runID, "a", nodeService, spec)
	return store, runID, nodeService, execution
}

// A session registered longer than the window, with no result and no successful turn end, fails
// its node and the run under Cloud's own reason, and the terminal pass declares the Workspace
// delete. A repeated pass is a no-op: the run is already terminal and is never rescanned.
func TestWorkflowSessionTurnDeadlineFailsNode(t *testing.T) {
	store, runID, _, execution := dispatchedWorkflowSession(t)
	ageWorkflowSessionExecution(t, store, execution, "7260") // 2h1m, past the approved 2h default

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance past the turn window: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != workflowSessionTurnDeadlineExceeded {
		t.Fatalf("run settled %q error %q states=%v; want failed/%s", status, runErr, states, workflowSessionTurnDeadlineExceeded)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.S("error") != workflowSessionTurnDeadlineExceeded ||
		agent.O("output").S("reason") != workflowSessionTurnDeadlineExceeded {
		t.Fatalf("agent state = %v; want failed/%s", states["a"], workflowSessionTurnDeadlineExceeded)
	}
	assertRunWorkspaceReleased(t, store, runID)

	// A replay cannot advance a terminal run: status, states and version are unchanged.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if again, _, _, againVersion := readWorkflowRun(t, store, runID); again != "failed" || againVersion != version {
		t.Fatalf("a replay rewrote the terminal run: status=%s version=%d, want failed/%d", again, againVersion, version)
	}
}

// Inside the window the session is held: elapsed time alone is not evidence, so neither the node
// nor the run settles and no end request is released.
func TestWorkflowSessionTurnDeadlineHoldsWithinWindow(t *testing.T) {
	store, runID, _, execution := dispatchedWorkflowSession(t)
	ageWorkflowSessionExecution(t, store, execution, "3600") // 1h, inside the approved 2h default

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, store, runID)
	agent, _ := asWorkflowObject(states["a"])
	if status != "running" || agent == nil || agent.S("status") != "running" {
		t.Fatalf("a session inside its window settled: %q %v", status, states)
	}
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("a session with no turn end released commands: %v", got)
	}
}

// The window is the configured value, not a constant: a deployment that sets a shorter window gets
// it, through the Store field the server wires from configuration.
func TestWorkflowSessionTurnDeadlineUsesTheConfiguredWindow(t *testing.T) {
	store, runID, _, execution := dispatchedWorkflowSession(t)
	store.WorkflowSessionTurnTimeout = 30 * time.Minute
	ageWorkflowSessionExecution(t, store, execution, "1860") // 31m

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if status, runErr, _, _ := readWorkflowRun(t, store, runID); status != "failed" || runErr != workflowSessionTurnDeadlineExceeded {
		t.Fatalf("configured 30m window: run settled %q error %q", status, runErr)
	}
}

// A successful `turnEnded` is the later trigger and wins over the turn window: the session is asked
// to end through the end-confirmation path instead of being declared over-window, even when the
// execution is old enough for the deadline. The ordering is what keeps a session that did finish
// its work from being reported as hung.
func TestWorkflowSessionTurnDeadlineDoesNotOverrideSuccessfulTurn(t *testing.T) {
	store, runID, _, execution := dispatchedWorkflowSession(t)
	takeOverWorkflowEvents(t, store, runID, execution, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	ageWorkflowSessionExecution(t, store, execution, "7260")

	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if status, _, states, _ := readWorkflowRun(t, store, runID); status != "running" {
		t.Fatalf("a successful turn was settled by the turn deadline: %q %v", status, states)
	}
	commands := claimCommands(t, store)
	if len(commands) != 1 || commands[0].O("body").S("reason") != "idle_timeout" || commands[0].S("executionId") != execution {
		t.Fatalf("commands = %v; want the one end request for %s", commands, execution)
	}
}

// A committed result is terminal evidence and wins over the deadline: a result that commits just
// before the pass ages the execution settles the node from its own reason, not as over-window,
// which is the ordering the owner fixed (result first, then reachability, then the window).
func TestWorkflowSessionTurnDeadlineDoesNotOverrideAResult(t *testing.T) {
	store, runID, nodeService, execution := dispatchedWorkflowSession(t)
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Body: Object{
			"operationId": runID, "executionId": execution,
			"result":   Object{"outcome": "agent_session_ended", "reason": "idle_timeout", "node": Object{"nodeId": nodeService}},
			"sequence": int64(1), "event": "ended",
		}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("commit result: %v", err)
	}
	ageWorkflowSessionExecution(t, store, execution, "7260")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if status, runErr, _, _ := readWorkflowRun(t, store, runID); status != "succeeded" || runErr != "" {
		t.Fatalf("a committed result lost to the turn deadline: %s/%q", status, runErr)
	}
}

// A result that arrives after the deadline settled the run is kept on the execution row and changes
// nothing else, exactly as a late result after `node_unavailable` is kept: the terminal run is not
// rewritten, and the execution row is the only record of what actually ran.
func TestWorkflowSessionTurnDeadlineResultAfterTheNodeSettledIsKept(t *testing.T) {
	store, runID, nodeService, execution := dispatchedWorkflowSession(t)
	ageWorkflowSessionExecution(t, store, execution, "7260")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance past the turn window: %v", err)
	}
	status, runErr, states, version := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != workflowSessionTurnDeadlineExceeded {
		t.Fatalf("run settled %q error %q; want failed/%s", status, runErr, workflowSessionTurnDeadlineExceeded)
	}

	// The Node was only slow, and reports the session's real end after Cloud stopped waiting.
	reportSessionEnd(t, store, runID, execution, nodeService, "idle_timeout")
	afterStatus, afterErr, afterStates, afterVersion := readWorkflowRun(t, store, runID)
	if afterStatus != status || afterErr != runErr {
		t.Fatalf("the late result moved the run to %q error %q; want %q error %q", afterStatus, afterErr, status, runErr)
	}
	if afterVersion != version+1 {
		t.Fatalf("the late stop evidence settles the independent skipped delivery once: version=%d want=%d", afterVersion, version+1)
	}
	if jsonText(afterStates) != jsonText(states) {
		t.Fatalf("the late result rewrote the node states:\n got %v\nwant %v", afterStates, states)
	}
	var outcome, reason string
	if err := store.Pool.QueryRow(`SELECT result->>'outcome', result->>'reason' FROM node_executions WHERE execution_id=$1`, execution).Scan(&outcome, &reason); err != nil {
		t.Fatalf("read the execution result: %v", err)
	}
	if outcome != "agent_session_ended" || reason != "idle_timeout" {
		t.Fatalf("execution result = %s/%s; want the reported session end kept as evidence", outcome, reason)
	}
}

// The window belongs to the execution row, not the run: after a first node settles and a second
// node dispatches, only the new execution's own instant is judged. An old first-node execution
// cannot make the second node over-window, which is also what makes a retry's new row re-time.
func TestWorkflowSessionTurnDeadlineIsPerExecution(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowTwoAgentGraph())
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a1")
	execution1 := dispatchSession(t, store, runID, "a1", nodeService, spec1)
	reportSessionEndAt(t, store, runID, execution1, nodeService, "idle_timeout", 1)
	spec2 := sessionWorkSpec(t, store, runID, "a2")
	execution2 := dispatchSession(t, store, runID, "a2", nodeService, spec2)

	ageWorkflowSessionExecution(t, store, execution1, "7260")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	status, _, states, _ := readWorkflowRun(t, store, runID)
	a2, _ := asWorkflowObject(states["a2"])
	if status != "running" || a2 == nil || a2.S("status") != "running" {
		t.Fatalf("an old execution's age settled a fresh node: %q %v", status, states)
	}

	ageWorkflowSessionExecution(t, store, execution2, "7260")
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if status, runErr, _, _ := readWorkflowRun(t, store, runID); status != "failed" || runErr != workflowSessionTurnDeadlineExceeded {
		t.Fatalf("the second execution's own window: run settled %q error %q", status, runErr)
	}
}
