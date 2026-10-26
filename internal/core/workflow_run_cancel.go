package core

import (
	"fmt"
	"time"
)

// WorkflowRun cancellation: the public request that records `workflow_runs.cancel_requested_at`
// and the scheduler pass that reacts to it (specs decisions/cloud/workflow
// 0-cloud-side-scheduler-owns-workflow-execution.md D4/D6). The split is the one IssueRun D6
// already makes: the writer records the request and writes nothing else, and the run's own
// scheduler — which already recomputes every decision from committed state — settles the run on a
// later pass. A request is therefore never lost between the click and the settlement, and a caller
// that repeats it gets the instant the first one recorded.
//
// The terminal rule is D4's: once a request is recorded while the run is non-terminal, the run's
// terminal status is `cancelled`. A committed node result keeps its own state — it is what actually
// ran — but it does not keep the run alive, and a result that arrives after the run is terminal is
// kept on the execution row without rewriting the run.

// cancelWorkflowRunByUser is the public cancellation request behind
// `POST .../workflows/:wfid/runs/:rid/cancel`. It runs on the caller's transaction, so the request
// and the caller's idempotency record share one commit.
//
// It records the request and writes no other column. D4 makes cancellation a request: the run keeps
// its status, node states, session and Workspace until the scheduler reacts on its next pass, so the
// reaction is deliberately not inlined here — reacting twice, from the endpoint and from the
// scheduler, would put one transition in two places and let a caller's transaction do the work the
// pass is retried for.
//
// The window mirrors the IssueRun request's agent-execution window: `pending` (never claimed) and
// `running` (dispatching or in flight) are cancellable; every terminal status answers 409
// run_not_cancellable. `awaiting_input` is admitted by the column's CHECK but never written, so a
// cancellation there has no defined reaction and is refused like a terminal state rather than
// reported as corruption. A status outside the column's closed set is corruption.
//
// Authorization follows the binding rule the personal-model decision gave IssueRuns: on a run
// carrying frozen model bindings only the run's initiator (`created_by`) and the tenant's current
// administrators may cancel; a run with no binding keeps the member rule, which Public already
// established before dispatching here. Every workflow run created against a Project freezes at
// least the default slot's binding, so in practice this is the initiator-or-admin rule. The answer
// is checked before the window, so a caller who may not act on this run never learns whether it was
// still cancellable.
//
// Repeating the request is a no-op that returns the instant the first one recorded, whether or not
// the run has since settled: the recorded instant is the one the outcome is decided from, so the
// CAS writes only while `cancel_requested_at IS NULL`.
func cancelWorkflowRunByUser(t *transaction, r *PublicRequest, uid string) Object {
	workflow(t, r.TenantID, r.WorkflowID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT * FROM workflow_runs
		WHERE id=$1 AND tenant_id=$2 AND workflow_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, r.WorkflowID)
	require(run != nil, 404, "not_found")
	require(mayControlWorkflowRun(t, run, uid), 403, "workflow_run_cancel_forbidden")
	if run["cancelRequestedAt"] == nil {
		require(cancellableWorkflowRun(run), 409, "run_not_cancellable")
		// The CAS repeats the predicate the read approved under the caller's advisory lock: zero
		// affected rows contradicts that read, so it is invariant corruption rather than a race to
		// tolerate, exactly as in cancelAgentRunByUser.
		if moved := t.execRows(`
			UPDATE workflow_runs
			SET cancel_requested_at=now(), version=version+1, updated_at=now()
			WHERE id=$1 AND cancel_requested_at IS NULL`, run.S("id")); moved != 1 {
			panic(databaseFailure{fmt.Errorf("cancel workflow run: run %s was not marked cancelled (rows affected %d)", run.S("id"), moved)})
		}
		// Re-read rather than trust the application clock: the recorded instant is the database's, and
		// the response must be what a later repeat reads back.
		run = t.one(`SELECT cancel_requested_at FROM workflow_runs WHERE id=$1`, run.S("id"))
	}
	return Object{"cancelRequestedAt": run["cancelRequestedAt"]}
}

// mayControlWorkflowRun answers whether uid may request this run's cancellation: the initiator or
// an active tenant administrator when the run carries frozen model bindings, and any active member
// (which Public already checked) when it carries none. The rule is read from current PostgreSQL
// state, never from a caller-supplied field, and uid is already alias-resolved by the dispatcher, so
// a renamed account keeps the right it earned.
func mayControlWorkflowRun(t *transaction, run Object, uid string) bool {
	if t.one("SELECT id FROM run_model_bindings WHERE workflow_run_id=$1 LIMIT 1", run.S("id")) == nil {
		return true
	}
	if uid == run.S("createdBy") {
		return true
	}
	member := t.one("SELECT role FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", run.S("tenantId"), uid)
	return member != nil && member.S("role") == "admin"
}

// cancellableWorkflowRun reports whether a cancellation request still has meaning for this run: the
// two states in which the scheduler either has not started the run or still owns a live session.
// The other statuses are named rather than folded into a default so that a status outside the
// column's closed set is reported as Cloud's broken invariant instead of being answered as a
// conflict — the same treatment cancellableAgentRun gives an unknown phase.
func cancellableWorkflowRun(run Object) bool {
	switch run.S("status") {
	case "pending", "running":
		return true
	case "awaiting_input", "succeeded", "failed", "cancelled":
		return false
	default:
		panic(databaseFailure{fmt.Errorf("cancel workflow run: run %s has status=%q, outside the closed set", run.S("id"), run.S("status"))})
	}
}

// advanceCancelledWorkflowRun is the scheduler's reaction to a recorded cancellation request. It
// settles the run `cancelled` without dispatching another node, and it keeps whatever evidence the
// run already committed:
//
//   - A node waiting to retry: the wait is not a session, so there is nothing to end. The node
//     settles cancelled, the run settles cancelled and the Workspace is released — the request does
//     not wait for the committed due instant and does not start the attempt that was waiting for it
//     (retry decision D5, invariant 6).
//   - No in-flight agent session: nothing to stop, so the request settles the run now. This is also
//     the whole story of a run cancelled while `pending`: it never claims, never stamps started_at,
//     never validates its graph and never provisions a Workspace — the unregistered work, if any,
//     is deleted by the terminal cleanup.
//   - A committed session result: the node keeps its own outcome (succeeded, failed or cancelled by
//     the reason the session reported), because that result is what actually ran, and the run is
//     still cancelled — a request recorded while the run was non-terminal decides the run.
//   - A session dispatched but not reported: the scheduler reuses that node's existing end request
//     when one exists and otherwise releases `end_session{cancelled}`. 0049 makes the request one row
//     per node, so a repeat is a replay rather than a second request. An existing request is not
//     upgraded: its reason may already have been acted on by the Node, and rewriting it would
//     change a request after the fact. The wait is bounded by the confirmation window measured from
//     the command's own created_at on the database clock, and expiry settles the node cancelled —
//     not failed — because the run was cancelled by request.
//   - A Node that can no longer report: a Node that cannot confirm an end request would otherwise
//     hold the run until the window expires, so the cancel wins over the unreachable settlement and
//     the node/run settle cancelled. The judgement and window are the shared
//     runWorkspaceNodeUnknown rule the rest of the session path uses: "the Node is gone" has one
//     definition across run kinds.
//
// Every terminal path goes through finishWorkflowRun, so a cancelled run deletes its unregistered
// work and declares its Workspace deletion like any other terminal run (D7).
func advanceCancelledWorkflowRun(t *transaction, run Object, version int64, states map[string]workflowNodeState, unreachableAfter time.Duration) {
	runID := run.S("id")
	// The retrying branch comes first: a waiting retry starts no session, so cancellation must not
	// wait for its due instant, and the request wins even when retryAt has already passed (D5).
	if nodeID, retrying := workflowRetryingAgentNode(states); retrying {
		cancelWorkflowAgentNode(states, nodeID)
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	nodeID, running := workflowRunningAgentNode(states)
	if !running {
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	// Evidence is scoped to the attempt in flight: a cancelled node may have an earlier attempt's
	// result committed on its own execution row, and reading it here would settle the cancel with
	// that stale outcome instead of stopping the current session (retry decision D4).
	nodeState := states[nodeID]
	attempt := nodeState.attemptNumber()
	// The session's own terminal evidence is the node's state before the request is: it is what
	// actually ran. Reading it first also ends the session cleanly, without a pointless end request.
	if e := t.one(`SELECT result FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='agent_session' AND result IS NOT NULL`, runID, nodeID, attempt); e != nil {
		applyWorkflowAgentTerminal(states, nodeID, e.O("result"))
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	exec := t.one(`SELECT execution_id FROM node_executions
		WHERE operation_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='agent_session'`, runID, nodeID, attempt)
	if exec == nil {
		// Committed running but never dispatched: there is no session to stop, and waiting for the
		// startup deadline would hold a run the user already asked to stop.
		cancelWorkflowAgentNode(states, nodeID)
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	if w := t.one("SELECT id FROM workspaces WHERE workflow_run_id=$1", runID); w != nil &&
		runWorkspaceNodeUnknown(t, runID, w.S("id"), unreachableAfter) {
		cancelWorkflowAgentNode(states, nodeID)
		finishWorkflowRun(t, runID, version, "cancelled", "", states)
		return
	}
	cmd := t.one("SELECT * FROM thread_commands WHERE workflow_run_id=$1 AND workflow_node_id=$2 AND workflow_node_attempt=$3 AND kind='end_session'", runID, nodeID, attempt)
	if cmd == nil {
		cmd = enqueueWorkflowThreadCommand(t, runID, nodeID, attempt, "end_session", Object{"reason": "cancelled"})
	}
	window := startupDuration(t.store.AgentEndConfirmTimeout, 5*time.Minute)
	if t.one(`SELECT id FROM thread_commands WHERE id=$1
		AND created_at<=clock_timestamp()-make_interval(secs=>$2)`, cmd.S("id"), window.Seconds()) == nil {
		return
	}
	cancelWorkflowAgentNode(states, nodeID)
	finishWorkflowRun(t, runID, version, "cancelled", "", states)
}

// applyWorkflowAgentTerminal applies a session execution's result document to a node's committed
// state. The result's reason is the node's evidence (mappings decision D3): agent_failed and
// interrupted fail the node, cancelled cancels it, and the natural session ends succeed it. Only a
// failed node carries an error — a cancelled node was cancelled, not broken. The display name and
// start instant survive: they describe the execution the result belongs to.
func applyWorkflowAgentTerminal(states map[string]workflowNodeState, nodeID string, result Object) {
	reason := workflowAgentTerminalReason(result)
	state := states[nodeID]
	state.Status = workflowSessionEndReason(result.S("reason"))
	state.Attempt = state.attemptNumber()
	state.RetryAt = ""
	state.FinishedAt = nowRFC3339()
	state.Output = Object{"reason": reason}
	if state.Status == "failed" {
		state.Error = reason
	}
	states[nodeID] = state
}

// workflowAgentTerminalReason maps a session result onto the node/run reason the public side reads.
// The Node reports a failed plugin start as the generic `agent_failed` with
// `detail=agent_plugin_unavailable`; a WorkflowRun has no Thread, so the node and run rows are the
// only place its cause can surface. Other reasons pass through unchanged — only the bounded,
// user-actionable plugin code that IssueRun threads already expose is lifted out of the detail.
func workflowAgentTerminalReason(result Object) string {
	if result.S("reason") == "agent_failed" && result.S("detail") == "agent_plugin_unavailable" {
		return "agent_plugin_unavailable"
	}
	return result.S("reason")
}

// cancelWorkflowAgentNode commits one in-flight agent node cancelled. It is Cloud's own verdict for
// a session the user asked to stop (or whose end could not be confirmed), not a Node-reported reason,
// so the node carries no error: nothing broke.
func cancelWorkflowAgentNode(states map[string]workflowNodeState, nodeID string) {
	state := states[nodeID]
	state.Status = "cancelled"
	state.Attempt = state.attemptNumber()
	// A cancelled wait has no next attempt, so the committed due instant must not survive into the
	// terminal state: a later pass reading it would see a cancelled node that still looks scheduled.
	state.RetryAt = ""
	state.FinishedAt = nowRFC3339()
	state.Error = ""
	state.Output = Object{"reason": "cancelled"}
	states[nodeID] = state
}
