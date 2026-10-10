package core

import (
	"fmt"
	"time"
)

// agentSessionEndedOutcome is the canonical outcome name of the session terminal result, kept in one
// place so the control plane's validation, the durable result object and this settlement cannot
// drift apart.
const agentSessionEndedOutcome = "agent_session_ended"

// settleSessionEnded is the B-owned session-terminal core behind the A→B hook OnSessionEnded
// (controller-integration D6 sessionEnded, Thread D4, IssueRun D3/D4; plan §5 Batch 1).
//
// It runs on the caller-owned *transaction of the session execution's terminal-event takeover,
// inside which all of the following must commit or roll back together:
//
//	Thread terminal:   thread_state='ended', idle_since=NULL — D4's last, unconditional row:
//	                   "会话执行终态被接管 → ended". `ending` is not a precondition: the row is
//	                   stated for the session execution's terminal event, and gating on `ending`
//	                   would make the reasons that reach it without a preceding end request
//	                   (`agent_failed`, `interrupted`) permanently un-representable while leaving
//	                   the terminal event un-acked, which the Controller would replay forever.
//	Discard:           every user turn still `queued` for this run becomes `discarded` (Thread D3:
//	                   turns the session never executed are marked in the transaction that takes its
//	                   terminal state over, and are never executed by a later session).
//	Delivering:        phase='delivering', status stays 'running' (IssueRun D3's `delivering` row,
//	                   whose entry condition is "会话执行有终态结果（任何结束原因）" and whose status
//	                   column is `running`).
//	Delivery release:  the Revision delivery work item is released in this same transaction
//	                   (IssueRun D3; controller-integration D6 spells the whole obligation as one
//	                   hook: "Thread `ended`、排队轮次 `discarded`、进入 `delivering` 并放出交付工作项").
//	Hint:              `issue_run.thread_changed` — the REST representation changed. No entry is
//	                   appended, so no `thread_appended` and no seq is allocated (Thread D5/A4).
//
// It never opens its own transaction, spawns a goroutine, or defers a post-commit effect. Identity
// is the two ids the control plane passes (run and execution); every business field is re-read here,
// and the execution must be this run's own registered agent_session execution, so a mis-addressed
// event can never be projected onto another run's Thread.
//
// What is deliberately NOT here: `releasing` and the `status` D4 derives from the end reason
// (written when the delivery settles), the Revision row, delivering on `DeliverRevision`, the
// Workspace delete and `done`. Those are Phase 5 Batch 2.
func (s *Store) settleSessionEnded(t *transaction, runID, executionID string, ended Object) error {
	if !validID(runID) {
		return fmt.Errorf("sessionEnded: invalid run id %q", runID)
	}
	if executionID == "" {
		return fmt.Errorf("sessionEnded: run %s has no execution identity", runID)
	}

	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		// No authoritative run to accept the terminal event: an invariant violation, not a replay, so
		// the event must not be acked.
		return fmt.Errorf("sessionEnded: issue_run %s not found", runID)
	}
	if o.S("executorType") != "agent" {
		return fmt.Errorf("sessionEnded: run %s is executor_type=%q, not agent", runID, o.S("executorType"))
	}
	e := t.one("SELECT * FROM node_executions WHERE execution_id=$1", executionID)
	if e == nil {
		return fmt.Errorf("sessionEnded: execution %s is not registered", executionID)
	}
	if e.S("kind") != "agent_session" {
		return fmt.Errorf("sessionEnded: execution %s is kind=%q, not agent_session", executionID, e.S("kind"))
	}
	work := t.one("SELECT * FROM execution_work WHERE id=$1", e.S("workId"))
	if work == nil || work.S("runId") != runID || work.S("kind") != "agent_session" {
		// Matching execution → work → run is what proves the terminal event really belongs to this
		// run and this session work; a mismatch is refused rather than resolved to "some other run".
		return fmt.Errorf("sessionEnded: execution %s does not belong to run %s agent_session work", executionID, runID)
	}
	// Re-validated here even though the control plane already refused a malformed result: the hook is
	// the last gate before a business transition, and an unknown reason would silently lose the
	// IssueRun status D4 derives from it. The closed set is the control plane's own sessionEndReasons
	// — one definition of what a session end reason is, not two.
	if ended.S("outcome") != agentSessionEndedOutcome {
		return fmt.Errorf("sessionEnded: run %s execution %s carries outcome %q, not %s", runID, executionID, ended.S("outcome"), agentSessionEndedOutcome)
	}
	if !sessionEndReasons[ended.S("reason")] {
		return fmt.Errorf("sessionEnded: run %s execution %s carries end reason %q, outside the closed set", runID, executionID, ended.S("reason"))
	}

	// One CAS for both run-level transitions, so they cannot even in principle happen apart: the
	// Thread reaches its terminal state and the run leaves the session phases in the same row write.
	// `ending` is included in the source set alongside the live states because D4 states the terminal
	// row for the session execution's end, not for a preceding request (IssueRun D6: an EndSession
	// command is a *request*).
	if moved := t.execRows(`
		UPDATE issue_runs
		SET phase='delivering', status='running', thread_state='ended', idle_since=NULL,
		    version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND deleted_at IS NULL
		  AND phase IN ('starting','running')
		  AND thread_state IN ('pending','active','idle','ending')`, runID); moved != 1 {
		// The row was re-read in this very transaction under the caller's advisory lock, so zero
		// affected rows contradicts that read. It is reported precisely rather than tolerated: a
		// terminal event must never be acked for a transition that did not happen (mandate §9/§13).
		now := t.one("SELECT phase, thread_state FROM issue_runs WHERE id=$1", runID)
		return fmt.Errorf("sessionEnded: run %s was not moved to ended/delivering (rows affected %d; phase=%q thread_state=%q)",
			runID, moved, now.S("phase"), now.S("threadState"))
	}

	// Discard, in the same transaction as the terminal state. The predicate is exactly D3's set: a
	// `user` turn still `queued` was never executed by the session, so it is marked `discarded` and
	// is never executed later. Nothing else about the row is touched — no content, no record, no
	// turn_id, no seq — and a `delivered` turn never regresses. system entries carry a NULL status
	// by the schema's own CHECK, so no predicate for them is needed or possible.
	t.execRows(`
		UPDATE thread_entries SET status='discarded'
		WHERE run_id=$1 AND source='user' AND status='queued'`, runID)

	// A restore that found the prior Revision's base commit gone from the remote refuses that Revision
	// for later runs in this same transaction (resume decision D3).
	refuseResumedRevision(t, executionID, ended)

	// The delivery work item is released in this transaction (IssueRun D3). A failure returns the
	// error, which the seam turns into a databaseFailure: the whole takeover rolls back, so the run
	// is never `delivering` without the delivery that phase exists to produce.
	//
	// The call is the control plane's own in-transaction enqueue, so the work item, its server-owned
	// object keys and the `delivery_already_settled` conflict guard are all A's. When storage is not
	// configured the enqueue settles this very delivery as `skipped` through the delivery hook
	// instead of declaring work, which is what releases the run.
	spec, err := s.sessionDeliverySpec(t, o, executionID)
	if err != nil {
		return err
	}
	enqueueExecutionWork(t, runID, "deliver_revision", spec, sessionStartTarget(t, o.S("workspaceId")), time.Time{})

	// Last, so lastSeq is the high-water mark this transaction is about to commit. `ended` is a
	// state-only change with a cleared idleSince, and the discards change per-turn status: both
	// change the Thread's REST representation, so A4's generalized hint is exactly the one that
	// covers them. No entry was appended, so no append hint and no seq are produced.
	threadChanged(t, o)
	return nil
}

// sessionDeliverySpec builds the fixed DeliverRevision snapshot the delivery work item carries
// (proto DeliverRevisionSpec; Revision D2). Everything but the object keys is read from
// authoritative rows in the caller's transaction:
//
//	sessionExecutionId:  the ended session execution.
//	checkoutExecutionId: the clone execution that produced the run Workspace's recorded baseline.
//	baseCommit:          workspaces.base_commit_id, the commit the bundle is relative to.
//	priorRevision:       the Revision the session resumed, without bundle (resume decision D5).
//	kind:                the work item kind, which the control plane's enqueue re-checks.
//
// The three server-owned locations — the per-attempt `bundleKey`/`historyKey` and the
// `revisionRef` — are deliberately absent: the control plane fixes them while it inserts the work
// item, so a retried delivery is a new attempt with new keys by construction and no caller can name
// an object another attempt already wrote.
//
// A run Workspace with no recorded baseline, or one whose baseline names no successful clone
// execution, is an invariant violation: a session cannot have run in a Workspace that was never
// cloned, so failing closed (and rolling the whole takeover back) is the only honest outcome.
func (s *Store) sessionDeliverySpec(t *transaction, o Object, sessionExecutionID string) (Object, error) {
	runID, wid := o.S("id"), o.S("workspaceId")
	if wid == "" {
		return nil, fmt.Errorf("sessionEnded: run %s has no run workspace", runID)
	}
	w := t.one("SELECT base_commit_id FROM workspaces WHERE id=$1 AND issue_run_id=$2 AND deleted_at IS NULL", wid, runID)
	if w == nil {
		return nil, fmt.Errorf("sessionEnded: run %s has no live run workspace %s", runID, wid)
	}
	base := w.S("baseCommitId")
	if !commitID(base) {
		return nil, fmt.Errorf("sessionEnded: run %s workspace %s has no usable base commit (%q)", runID, wid, base)
	}
	checkout := t.one(`
		SELECT execution_id FROM clone_executions
		WHERE workspace_id=$1 AND result->>'outcome'='clone_ready' AND result->>'commit'=$2
		ORDER BY created_at DESC, execution_id DESC LIMIT 1`, wid, base)
	if checkout == nil {
		return nil, fmt.Errorf("sessionEnded: run %s workspace %s has no successful clone execution for base commit %s", runID, wid, base)
	}
	spec := Object{
		"kind":                "deliver_revision",
		"sessionExecutionId":  sessionExecutionID,
		"checkoutExecutionId": checkout.S("executionId"),
		"baseCommit":          base,
	}
	if prior := deliveryPriorRevision(t, sessionExecutionID); prior != nil {
		spec["priorRevision"] = prior
	}
	return spec, nil
}
