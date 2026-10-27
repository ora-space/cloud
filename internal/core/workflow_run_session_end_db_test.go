package core

import (
	"context"
	"testing"
)

// A WorkflowRun session is single-turn: the initial turn's `turnEnded` receipt is the node's
// completion signal, and Cloud releases the existing Controller EndSession relay with
// `idle_timeout` (circus decision #292 option A). These tests drive the Cloud half: the receipt
// trigger, the command's owner and node scope, and the bounded wait.

// turnEndedRecord builds the `ora-history` record the Node writes when its turn ends. The record
// serializes variant fields in snake_case (`stop_reason`) even though the type tag is camelCase
// (`turnEnded`), as the real receipts from the acceptance stack do; `cancelled` is the one value
// that is not idle evidence (Thread D4).
func turnEndedRecord(stopReason string) Object {
	return Object{"at": "2026-10-19T10:00:00+00:00", "seq": int64(1), "type": "turnEnded", "stop_reason": stopReason}
}

// takeOverWorkflowEvents relays one Node history batch for a WorkflowRun session through the
// control plane's own takeover, exactly as the Controller does.
func takeOverWorkflowEvents(t *testing.T, store *Store, runID, execution string, events []Object) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		takeOverThreadEvents(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "events": events}})
		return Object{}
	}); err != nil {
		t.Fatalf("take over thread events: %v", err)
	}
}

// claimCommands reads the Controller's ordered command queue.
func claimCommands(t *testing.T, store *Store) []Object {
	t.Helper()
	var out []Object
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		out = objectsOf(claimThreadCommands(tx, 100)["commands"])
		return Object{}
	}); err != nil {
		t.Fatalf("claim thread commands: %v", err)
	}
	return out
}

// reportSessionEndAt reports the session's terminal event at an explicit receipt sequence: a
// session that already took over Thread records has consumed sequences one through the batch, and
// the terminal event must be the next one.
func reportSessionEndAt(t *testing.T, store *Store, runID, execution, nodeService, reason string, sequence int64) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		executionResult(tx, &ControlRequest{Body: Object{"operationId": runID, "executionId": execution, "result": Object{"outcome": "agent_session_ended", "reason": reason, "node": Object{"nodeId": nodeService}}, "sequence": sequence, "event": "ended"}}, true)
		return Object{}
	}); err != nil {
		t.Fatalf("report result: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after result: %v", err)
	}
}

// A successful initial turn releases exactly one end request scoped to that node's session, and the
// node settles only from the session's own terminal result — the receipt itself is a trigger, not
// terminal evidence.
func TestWorkflowSessionEndsAfterSuccessfulTurn(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)

	// Nothing is released before the turn ends.
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("commands before the turn ended: %v", got)
	}
	takeOverWorkflowEvents(t, store, runID, execution, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after turn end: %v", err)
	}
	// The trigger does not settle the node: only the session's terminal result does (D4).
	if status, _, states, _ := readWorkflowRun(t, store, runID); status != "running" {
		t.Fatalf("run settled %q on a receipt alone; states=%v", status, states)
	}

	commands := claimCommands(t, store)
	if len(commands) != 1 {
		t.Fatalf("released %d commands after one turn end; want 1: %v", len(commands), commands)
	}
	cmd := commands[0]
	if cmd.S("kind") != "end_session" || cmd.O("body").S("reason") != "idle_timeout" {
		t.Fatalf("command = %v; want end_session/idle_timeout", cmd)
	}
	// The command names the WorkflowRun as its operation id — what the Controller matches its
	// control binding against — and the node's own session execution as its destination.
	if cmd.S("runId") != runID || cmd.S("executionId") != execution || cmd.S("nodeId") != nodeService {
		t.Fatalf("command destination = run %q execution %q node %q; want %q/%q/%q",
			cmd.S("runId"), cmd.S("executionId"), cmd.S("nodeId"), runID, execution, nodeService)
	}
	// Recomputing while the command is outstanding is a replay: still one row, still undelivered.
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance while ending: %v", err)
	}
	if again := claimCommands(t, store); len(again) != 1 || again[0].S("commandId") != cmd.S("commandId") {
		t.Fatalf("a re-tick released another command: %v", again)
	}

	// The Node accepts the command and reports the session's terminal result, which is the node's
	// evidence and carries the reason the command asked for.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		recordThreadCommandDelivered(tx, &ControlRequest{Body: Object{"commandId": cmd.S("commandId"), "executionId": execution}})
		return Object{}
	}); err != nil {
		t.Fatalf("record delivered: %v", err)
	}
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("delivered command still claimable: %v", got)
	}
	reportSessionEndAt(t, store, runID, execution, nodeService, "idle_timeout", 2)

	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "succeeded" || runErr != "" {
		t.Fatalf("run settled %q error %q states=%v; want succeeded", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "succeeded" || agent.S("sessionEndReason") != "idle_timeout" {
		t.Fatalf("agent state = %v; want succeeded/idle_timeout", states["a"])
	}
	assertRunWorkspaceReleased(t, store, runID)
}

// A cancelled turn is not idle evidence (Thread D4): the session's authoritative end settles it, so
// no end request is released from it.
func TestWorkflowSessionCancelledTurnDoesNotRequestEnd(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec := startSessionNode(t, store, runID, wsID, "a")
	execution := dispatchSession(t, store, runID, "a", nodeService, spec)
	takeOverWorkflowEvents(t, store, runID, execution, []Object{threadEvent(1, turnEndedRecord("cancelled"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after cancelled turn: %v", err)
	}
	if got := claimCommands(t, store); len(got) != 0 {
		t.Fatalf("a cancelled turn released %v; want no end request", got)
	}
}

// Every agent node gets its own end request: the run executes its agent nodes serially through one
// Workspace (D4), so the command is scoped to the graph node whose session it addresses, and a
// delivery naming another node's execution is refused.
func TestWorkflowSessionEndRequestsAreScopedPerNode(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionSceneGraph(t, store, workflowTwoAgentGraph())
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	wsID := settleSessionWorkspace(t, store, runID)
	nodeService, spec1 := startSessionNode(t, store, runID, wsID, "a1")
	execution1 := dispatchSession(t, store, runID, "a1", nodeService, spec1)
	takeOverWorkflowEvents(t, store, runID, execution1, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after a1 turn end: %v", err)
	}
	first := claimCommands(t, store)
	if len(first) != 1 || first[0].S("executionId") != execution1 {
		t.Fatalf("a1 commands = %v; want one addressed to %s", first, execution1)
	}
	// The Node ends a1; the next pass settles it and starts a2.
	reportSessionEndAt(t, store, runID, execution1, nodeService, "idle_timeout", 2)
	spec2 := sessionWorkSpec(t, store, runID, "a2")
	execution2 := dispatchSession(t, store, runID, "a2", nodeService, spec2)
	// a1's request was never acknowledged by the Node in this replay, but it still addresses a1's
	// session only: naming a2's execution is refused rather than delivering into another node.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		recordThreadCommandDelivered(tx, &ControlRequest{Body: Object{"commandId": first[0].S("commandId"), "executionId": execution2}})
		return Object{}
	}); err == nil || ErrorCode(err).Code != "dispatch_conflict" {
		t.Fatalf("cross-node delivery: err=%v; want dispatch_conflict", err)
	}

	takeOverWorkflowEvents(t, store, runID, execution2, []Object{threadEvent(1, turnEndedRecord("end_turn"), "")})
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance after a2 turn end: %v", err)
	}
	// a1's undelivered request is no longer claimable once its session has a result; a2's request is
	// the only live one, and it is a different row.
	second := claimCommands(t, store)
	if len(second) != 1 || second[0].S("executionId") != execution2 || second[0].S("commandId") == first[0].S("commandId") {
		t.Fatalf("a2 commands = %v; want a new request addressed to %s", second, execution2)
	}
}

// The end request's confirmation is bounded (D7): once the command has been outstanding longer than
// the window with no session terminal, the node fails under a named reason and the run finishes,
// which declares the Workspace deletion. The run never stays `running` forever.
func TestWorkflowSessionEndDeadlineFailsNode(t *testing.T) {
	store := dispatcherDB(t)
	runID, _, _, _, _ := seedWorkflowSessionScene(t, store)
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
	if got := claimCommands(t, store); len(got) != 1 {
		t.Fatalf("commands = %v; want the one end request", got)
	}
	// A process restart (or a Node that never claims the command) leaves the request outstanding;
	// age it past the confirmation window on the database clock.
	if _, err := store.Pool.Exec(`UPDATE thread_commands SET created_at=clock_timestamp()-interval '6 minutes'
		WHERE workflow_run_id=$1`, runID); err != nil {
		t.Fatalf("age the end request: %v", err)
	}
	if err := store.AdvanceWorkflowRunsOnce(context.Background()); err != nil {
		t.Fatalf("advance past the window: %v", err)
	}
	status, runErr, states, _ := readWorkflowRun(t, store, runID)
	if status != "failed" || runErr != "session end deadline exceeded" {
		t.Fatalf("run settled %q error %q states=%v; want the end deadline failure", status, runErr, states)
	}
	agent, _ := asWorkflowObject(states["a"])
	if agent == nil || agent.S("status") != "failed" || agent.S("sessionEndReason") != "session_end_deadline_exceeded" {
		t.Fatalf("agent state = %v; want the end deadline failure", states["a"])
	}
	if got := agent.S("error"); got != "session_end_deadline_exceeded" {
		t.Fatalf("failed agent error = %q; want session_end_deadline_exceeded", got)
	}
	assertRunWorkspaceReleased(t, store, runID)
}
