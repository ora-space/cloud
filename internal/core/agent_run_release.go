package core

import (
	"fmt"
	"time"
)

// The B-owned release half of an Agent IssueRun: `delivering → releasing` with D4's final status,
// and `releasing → done` once the run Workspace's delete has been taken over — or once D8's single
// give-up condition says it never will be (IssueRun D3/D4/D5/D8, controller-integration D6
// deliverRevision/RunWorkspaceDeleted).
//
// Two transitions, one shape: each is a CAS on the phase the caller re-read, and each declares or
// completes the run Workspace's delete in the same transaction that commits the phase write, so the
// run can never be `releasing` without a durable delete intent, nor `done` without a settled delete
// operation. Neither transition writes anything else about the run: `releasing` sets `status` exactly
// once from the session end reason (D4), and `done` deliberately changes no business outcome —
// "终态不变" in D3's own words, which is why `done` carries no separate status of its own and why
// `done` is never a synonym for `completed`.
//
// `done` therefore has two entry conditions (D3's table, as amended by D8), and they are not the same
// statement: a `succeeded` delete means the Workspace is gone, while D8's expiry means Cloud stopped
// waiting and the Workspace is still there. Only the first may ever be reported to a user as a
// released Workspace.

// runSessionExecution resolves the run's settled agent_session execution: the execution whose durable
// result carries the session end reason D4 derives the final status from, and the one the delivery
// spec names as `session_execution_id`. There is at most one per run — one `agent_session` work item
// gives at most one execution — and `result IS NOT NULL` is what makes it "settled" rather
// than in flight. nil means the run never had a session terminal fact, which no caller of this file
// can legitimately be looking at.
func runSessionExecution(t *transaction, runID string) Object {
	return t.one(`
		SELECT e.execution_id, e.node_id, e.result FROM node_executions e
		JOIN execution_work w ON w.id = e.work_id
		WHERE w.run_id=$1 AND w.kind='agent_session' AND e.result IS NOT NULL
		ORDER BY e.created_at DESC, e.execution_id DESC LIMIT 1`, runID)
}

// runReleaseStatus maps the session end reason to the IssueRun `status` D4 derives from it. The three
// groups are exactly D4's table: a conversation the user ended or that went idle is `completed`; a
// cancelled run is `cancelled`; an Agent failure, a Node interruption and an unknown session outcome
// are all `failed`. Anything outside the closed set is a caller error — the reasons are validated
// before this is reached, and guessing a status for an unknown reason would silently invent a
// business outcome.
func runReleaseStatus(reason string) string {
	switch reason {
	case "user_ended", "idle_timeout":
		return "completed"
	case "cancelled":
		return "cancelled"
	case "agent_failed", "interrupted":
		return "failed"
	default:
		return ""
	}
}

// releaseAfterDelivery moves a `delivering` run to `releasing` with the given deliveryState — and,
// when the delivery succeeded, the Revision it registered — and declares the run Workspace's delete
// in the same transaction (IssueRun D3/D5, operation D4; Cloud Revision D4).
//
// It is the only writer of `releasing` on the delivery path, and it is reached from exactly three
// approved places in DeliverySettled: a verified delivery (`saved`/`unchanged`, with the Revision id
// the control plane committed just before the hook ran), D5's give-up on a delivery that kept failing
// (`failed`, with no Revision), and a delivery whose declared objects Cloud could not confirm, which
// settles as `failed` under the same D5 rules. The `status` it writes is derived from the session's
// own end reason, so the business outcome ("how did the Agent run end") stays independent of the
// delivery outcome ("was the Revision saved"), which IssueRun invariant 4 requires: a `completed` run
// may legitimately carry `deliveryState=failed`.
//
// What is deliberately NOT here: the Agent-authored reply comment D4 asks for on `completed`. D4 says
// Cloud writes "Thread 中 Agent 的最后一条消息" as a reply comment, but that text lives inside a
// `SessionUpdate` record — a business payload owned by desktop `ora-history` — and Thread D2 forbids
// Cloud from parsing business fields ("Cloud 不解析业务字段") while Thread invariant 6 forbids
// rewriting Node record content. There is no approved field path to read the message text from, so
// writing a comment would mean guessing a foreign schema. The conflict is left as an open
// question for the ADR rather than resolved here by a silent choice.
func (s *Store) releaseAfterDelivery(t *transaction, o Object, state, revisionID string) error {
	recordDeliveryOutcome(t, o, state, revisionID)
	return s.finishRelease(t, o)
}

// recordDeliveryOutcome writes the run's delivery conclusion into its result without leaving
// `delivering`.
//
// The conclusion and the phase are two facts, and a scheduled run needs them apart: its artifacts
// may still be uploading when the Revision has settled, so the run states what the Revision decided
// first and finishes releasing once the artifacts converge or their window expires. Writing it here
// keeps one authority over the run's outcome, and `finishRelease` reads it back instead of taking it
// from a caller that may have decided it in another transaction.
//
// `revisionId` is written beside `deliveryState` (IssueRun D4 spells the run's result as
// `{revisionId | null, deliveryState}`): an absent key would be indistinguishable from a Revision
// whose id Cloud failed to record, so a delivery that saved nothing writes an explicit JSON null.
// Both parameters are cast explicitly because `jsonb_build_object` is variadic `"any"`: the server
// cannot infer a bare parameter's type from it, so `$3` alone is rejected at parse time with
// SQLSTATE 42P18. The `revisionId` cast also makes the empty string mean NULL, which is what a
// failed delivery passes.
func recordDeliveryOutcome(t *transaction, o Object, state, revisionID string) {
	if state == deliveryStateFailed {
		settleDeliveryExecutions(t, o.S("id"), "deliver_revision", "revision_abandoned")
	}
	t.exec(`
		UPDATE issue_runs
		SET result = COALESCE(result, '{}'::jsonb) || jsonb_build_object('revisionId', NULLIF($3::text,'')::uuid, 'deliveryState', $2::text),
		    version=version+1, updated_at=now()
		WHERE id=$1`, o.S("id"), state, revisionID)
}

// deliveryOutcomeRecorded reports whether the run already states what its Revision delivery decided.
// It is the precondition for releasing a run that also awaits artifacts.
func deliveryOutcomeRecorded(t *transaction, runID string) bool {
	o := t.one("SELECT result->>'deliveryState' AS delivery_state FROM issue_runs WHERE id=$1", runID)
	return o != nil && o.S("deliveryState") != ""
}

// artifactDeliveryPending reports whether one run's artifact delivery is still inside its window.
func artifactDeliveryPending(t *transaction, runID string) bool {
	o := t.one("SELECT state FROM scheduled_run_artifact_deliveries WHERE issue_run_id=$1", runID)
	return o != nil && o.S("state") == "delivering"
}

// settleRevisionForRun records what the Revision delivery decided and releases the run only when no
// artifact delivery still holds it.
//
// A scheduled run keeps `delivering` until both converge (scheduled-task verdict/artifacts ADR D3):
// the artifact upload is registered in its own work item and settles independently, so recording the
// Revision outcome here and leaving the release to whichever side settles last is what keeps a
// released Workspace from cutting an upload off. A run without artifacts, and the two later calls
// (artifact settlement or window expiry) that find the outcome already recorded, release exactly as
// before.
func (s *Store) settleRevisionForRun(t *transaction, o Object, state, revisionID string) error {
	recordDeliveryOutcome(t, o, state, revisionID)
	if artifactDeliveryPending(t, o.S("id")) {
		return nil
	}
	return s.finishRelease(t, o)
}

// finishRelease performs the `delivering → releasing` transition from the outcome the run already
// recorded.
func (s *Store) finishRelease(t *transaction, o Object) error {
	runID := o.S("id")
	recorded := t.one("SELECT result->>'deliveryState' AS delivery_state FROM issue_runs WHERE id=$1", runID)
	state := recorded.S("deliveryState")
	if state == "" {
		return fmt.Errorf("release agent run: run %s has no recorded delivery outcome", runID)
	}
	sess := runSessionExecution(t, runID)
	if sess == nil {
		return fmt.Errorf("release agent run: run %s has no settled agent_session execution", runID)
	}
	reason := sess.O("result").S("reason")
	status := runReleaseStatus(reason)
	if reason == "cancelled" && o.O("result").O("startup").B("policyCancellation") {
		status = "failed"
	}
	if status == "" {
		return fmt.Errorf("release agent run: run %s session execution %s carries end reason %q, outside the closed set", runID, sess.S("executionId"), reason)
	}
	// One CAS for the phase and D4's status together, so they cannot in principle happen apart. The
	// source phase is `delivering` and nothing else: the other two ways into `releasing` (the
	// provisioning failure and the no-session cancel) already ran their own write, and a second
	// entry from here would be a second authority over the same transition.
	if t.execRows(`
		UPDATE issue_runs
		SET phase='releasing', status=$2, completed_at=now(), version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND deleted_at IS NULL AND phase='delivering'`,
		runID, status) != 1 {
		now := t.one("SELECT phase, status FROM issue_runs WHERE id=$1", runID)
		return fmt.Errorf("release agent run: run %s was not moved to releasing (phase=%q status=%q)", runID, now.S("phase"), now.S("status"))
	}
	// D4's Timeline activity, in the same transaction as the status it reports. The details carry the
	// two independent outcomes so the timeline can explain a `completed` run whose Revision was not
	// saved, which is the pairing D5's give-up produces.
	appendActivity(t, o.S("tenantId"), o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run."+status,
		runActivityDetails(runID, o.S("executorType"), o.S("executorId"),
			Object{"sessionEndReason": reason, "deliveryState": state}))
	// The delete declaration goes through the A seam in this transaction: a seam failure rolls the
	// releasing transition back, so a run is never releasing without its delete intent.
	return s.declareDelete(t, o)
}

// deliveryGivenUp reports whether D5's give-up limits have been exceeded for a run still in
// `delivering`, which is what turns an unending delivery into a released run:
//
//	continuous failure:  more than `DeliveryGiveUpAfter` (D5's first-version default 2 h) has passed
//	                     since the run's FIRST delivery work item was declared. While the run is
//	                     `delivering` every attempt has failed — a success would have left the phase —
//	                     so that instant is the start of the unbroken failure run D5 measures.
//	unreachable Workspace: the run's Workspace has no live sandbox carrying a Node last reachable
//	                     within `DeliveryUnreachableAfter` (D5's default 30 min). A Workspace whose
//	                     Node's state has been unknown that long cannot deliver, so waiting out the
//	                     full failure window would only hold the sandbox longer.
//
// Both comparisons are made on database time, and both limits are judged by the caller's configured
// values. A limit that was never configured (zero or negative) fails closed: it is treated as "no
// give-up on this condition" rather than as an elapsed zero-length window, which would abandon every
// delivery the moment it was declared. The caller is a background pass with no client to tell, so
// giving up nothing is the safe answer.
func (s *Store) deliveryGivenUp(t *transaction, o Object) bool {
	runID, wid := o.S("id"), o.S("workspaceId")
	if s.DeliveryGiveUpAfter > 0 && t.one(`
		SELECT id FROM execution_work
		WHERE run_id=$1 AND kind='deliver_revision' AND created_at < now() - make_interval(secs => $2)
		LIMIT 1`, runID, s.DeliveryGiveUpAfter.Seconds()) != nil {
		return true
	}
	return runWorkspaceNodeUnknown(t, runID, wid, s.DeliveryUnreachableAfter)
}

// runWorkspaceNodeUnknown is the one authoritative judgement that a run Workspace's Node state is
// unknown, and the only place the heartbeat evidence behind it is read: the Workspace is bound to this
// run and no live sandbox of it carries a Node last confirmed reachable inside window.
// IssueRun D5's delivery give-up, IssueRun D8's delete give-up and the workflow scheduler's in-flight
// session settlement (workflowAgentSessionOutcome) and cancellation (advanceCancelledWorkflowRun)
// all ask this one function, so none of them can drift into a second definition of
// "the Node is unreachable".
//
// The binding is whichever column ties the Workspace to its run — `issue_run_id` for an IssueRun,
// `workflow_run_id` for a WorkflowRun. The two are mutually exclusive (0039's XOR), so one comparison
// covers both kinds without the caller having to say which it holds, and a Workspace bound to a
// different run of either kind is still refused.
//
// The polarity is deliberately "unknown" rather than "reachable": a Workspace whose row is missing, is
// not bound to this run, or whose id is unusable is *not* unknown. Failing the other way round would
// turn a lookup problem into an abandoned run.
//
// window is the caller's configured limit and is judged on the database clock, so a Cloud process with
// a skewed clock cannot abandon a Workspace another replica would still be waiting for. A window that
// was never configured (zero or negative) fails closed as "not unknown".
//
// A Workspace with no live sandbox at all is unknown immediately, window or not: there is no Node left
// to report anything, which is the case force stop leaves behind when it terminates the sandbox and
// the executions it was hosting in one transaction.
func runWorkspaceNodeUnknown(t *transaction, runID, wid string, window time.Duration) bool {
	if window <= 0 || !validID(wid) {
		return false
	}
	return t.one(`
		SELECT w.id FROM workspaces w
		WHERE w.id=$1 AND (w.issue_run_id=$2 OR w.workflow_run_id=$2)
		  AND NOT EXISTS (
		    SELECT 1 FROM sandbox_instances sb
		    JOIN node_instances n ON n.sandbox_instance_id = sb.id
		    WHERE sb.workspace_id = w.id AND sb.terminated_at IS NULL
		      AND n.ended_at IS NULL
		      AND n.last_seen_at > clock_timestamp() - make_interval(secs => $3))`,
		wid, runID, window.Seconds()) != nil
}

// releaseGivenUp reports whether D8's single give-up condition holds for a run still in `releasing`.
// The judgement itself — the run Workspace still holds a sandbox and that sandbox's Node state has
// been unknown for longer than `DeliveryUnreachableAfter` (D5's window, reused verbatim; D8 introduces
// no second limit and no "delete failed N times" count) — lives in runWorkspaceReleaseGivenUp, which
// the workflow scheduler's release pass asks for the same fact. Neither run kind can drift into a
// second definition of "the release should be abandoned".
func (s *Store) releaseGivenUp(t *transaction, o Object) bool {
	return runWorkspaceReleaseGivenUp(t, o.S("id"), o.S("workspaceId"), s.DeliveryUnreachableAfter)
}

// giveUpRunWorkspaceRelease is D8's terminal transition, run on the caller-owned *transaction: an
// unreachable run Workspace stops being retried, and the run reaches `done` without Cloud ever
// claiming the Workspace was released (IssueRun D8, D3's `done` row, invariant 8).
//
// Three writes, and nothing else, are what the decision consists of:
//
//   - the unfinished `delete_workspace` operation becomes terminally failed. Zero rows is a legal
//     outcome, not an error: the run can be `releasing` with no operation in flight (a Project that
//     was busy when the intent was declared, or a refusal the reconciliation pass has not re-declared
//     yet), and D8's condition is about the Workspace, not about an operation existing.
//   - `releasing → done`, as one CAS on the phase the caller re-read. A CAS that matches nothing is an
//     invariant contradiction and an error, which rolls the operation's failure back with it: a
//     terminal-failed operation without the run moving would be exactly the "operation failed, run
//     stuck in releasing" state D8 exists to prevent.
//   - `failure_reason = workspace_unavailable`, the code D3 already uses for the same fact — Cloud
//     could not make the Workspace available-to-delete, so the release did not happen.
//
// What is deliberately NOT here:
//
//   - `status`. D8 keeps the delivery conclusion D4 derived; `done` is not a business outcome.
//   - anything about `workspaces`. The Workspace is still there, and pretending otherwise — clearing
//     `issue_run_id`, soft-deleting the row, writing a success-shaped operation result — is the
//     lie D8 forbids. The row is the record of a residual resource, and it must stay readable.
//   - a Timeline activity. D4 attaches `run.completed/failed/cancelled` to the `releasing` transition,
//     which is where the business outcome is decided; D8 decides nothing about the outcome, and
//     inventing a new `action` string for it would publish an event no approved decision defines. The
//     reason is on the run itself, as `failure_reason`, where a client already reads it.
//   - any cleanup. D8 explicitly adds no background task, no retention sweep and no retry API; the
//     residual Workspace stays unreachable through the public API and is an operator's problem.
func (s *Store) giveUpRunWorkspaceRelease(t *transaction, o Object) error {
	runID, wid := o.S("id"), o.S("workspaceId")
	// `node_unavailable` is the published operation failure code for this fact — OperationErrorCode's
	// own vocabulary, already carried by the OpenAPI enum — so D8 reuses it rather than minting a new
	// public error code. It is also the code the Controller itself uses to park a step on
	// an unreachable Node, which is the same diagnosis. The operation is left with its `step` and
	// `request` intact: they are the record of how far the delete got.
	t.exec(`UPDATE operations SET state='failed', error_code='node_unavailable', version=version+1, updated_at=now()
		WHERE workspace_id=$1 AND kind='delete_workspace' AND state IN ('queued','running','retry_wait','blocked')`, wid)
	if t.execRows(`
		UPDATE issue_runs
		SET phase='done', failure_reason=$2, version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND phase='releasing'`, runID, "workspace_unavailable") != 1 {
		now := t.one("SELECT phase, status FROM issue_runs WHERE id=$1", runID)
		return fmt.Errorf("give up run release: run %s was not moved to done (phase=%q status=%q)", runID, now.S("phase"), now.S("status"))
	}
	return nil
}

// settleRunWorkspaceDeleted is the B-owned core behind the A→B hook OnRunWorkspaceDeleted
// (controller-integration D6 runWorkspaceDeleted, IssueRun D3).
//
// It runs on the caller-owned *transaction in which the run Workspace's delete_workspace operation
// was written `succeeded`, and it is the last transition of the run's lifecycle: `releasing → done`.
// Nothing else changes — D3's table gives `done` no status of its own ("终态不变"), so whatever D4
// derived when the run entered `releasing` stands, and a `done` run may be `completed`, `cancelled`
// or `failed` depending on how the Agent's session ended rather than on the delete succeeding.
//
// A run already `done` is a deterministic no-op: the operation's terminal state is a fact that may be
// replayed, and settling it twice must not bump the version or write a second business fact
// (IssueRun D3). Any other phase is an invariant contradiction — the delete is declared only from
// `releasing`, so no other phase can have a succeeded delete to settle.
//
// `done` is also reachable through D8's give-up, and that is what makes the no-op branch load-bearing
// rather than merely defensive: a run Cloud abandoned under D8 is `done` with its delete operation
// terminally failed, so a Controller that replays a late success for that operation is refused by the
// operation's own state check long before it gets here, and a replayed hook invocation finds the run
// `done` and writes nothing. Neither path can turn D8's give-up back into a reported deletion.
func (s *Store) settleRunWorkspaceDeleted(t *transaction, runID string) error {
	if !validID(runID) {
		return fmt.Errorf("runWorkspaceDeleted: invalid run id %q", runID)
	}
	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		// A run Workspace may belong to a WorkflowRun: the same delete_workspace terminal fires this
		// hook with the WorkflowRun's id, and the release fact it proves is the WorkflowRun's
		// (`settleWorkflowRunWorkspaceDeleted`, 0055). An id that is neither a live issue run nor a
		// live workflow run is still an invariant violation.
		if t.one("SELECT id FROM workflow_runs WHERE id=$1 AND deleted_at IS NULL", runID) != nil {
			return settleWorkflowRunWorkspaceDeleted(t, runID)
		}
		return fmt.Errorf("runWorkspaceDeleted: issue_run %s not found", runID)
	}
	if o.S("executorType") != "agent" {
		return fmt.Errorf("runWorkspaceDeleted: run %s is executor_type=%q, not agent", runID, o.S("executorType"))
	}
	switch o.S("phase") {
	case "done":
		return nil
	case "releasing":
		if t.execRows(`
			UPDATE issue_runs SET phase='done', version=version+1, updated_at=now()
			WHERE id=$1 AND executor_type='agent' AND phase='releasing'`, runID) != 1 {
			return fmt.Errorf("runWorkspaceDeleted: run %s was not moved to done from releasing", runID)
		}
		return nil
	default:
		return fmt.Errorf("runWorkspaceDeleted: run %s is phase=%q, not releasing or done", runID, o.S("phase"))
	}
}
