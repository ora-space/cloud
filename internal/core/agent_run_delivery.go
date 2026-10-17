package core

import (
	"fmt"
	"time"
)

// The delivery half of an Agent IssueRun: the B-owned settlement behind the control plane's
// OnDeliverySettled hook (controller-integration D6 deliverySettled, IssueRun D3/D4/D5).
//
// The control plane owns the whole takeover this reacts to. It acknowledges the delivery execution's
// terminal Node event with a receipt, writes the durable result, runs the object-store check that the
// repository forbids holding a transaction across, records its verdict in `revision_verifications`,
// registers the `revisions` row, and only then calls this hook — all in one transaction, so a
// settlement that fails rolls the Revision and the receipt back with it and the Node replays. What
// arrives here is therefore a *verdict*, not a Node claim: `saved` and `unchanged` carry the id of a
// Revision this transaction has already committed, `failed` carries either a reason from the wire's
// closed set or Cloud's own `verification_failed`, and `skipped` says the deployment has no object
// store and no delivery will ever happen for this run.
//
// Nothing here writes execution evidence. The two things this file owns are the run's own business
// conclusion (D4's `releasing` with its deliveryState) and, when a failure has not yet exhausted
// D5's limits, the release of the next delivery attempt through the control plane's enqueue.

// Delivery outcome spellings, shared by the control plane's settled object and the run's stored
// `result.deliveryState`: one vocabulary for one fact, so the hook contract and the durable run
// result cannot drift apart.
const (
	deliveryStateSaved     = "saved"
	deliveryStateUnchanged = "unchanged"
	deliveryStateFailed    = "failed"
	deliveryStateSkipped   = "skipped"
)

// Delivery retry policy (IssueRun D5). The ADR fixes both numbers as first-version defaults, so they
// are named constants rather than invented values: the backoff starts at 30 s and doubles per
// attempt, capped at 10 minutes.
const (
	deliveryRetryBase = 30 * time.Second
	deliveryRetryCap  = 10 * time.Minute
)

// deliveryRetryBackoff is D5's exponential backoff for the attempt after `attempted` failed delivery
// executions: 30s * 2^(attempted-1), capped at 10 minutes. `attempted` is the number of delivery
// executions already registered for the run, so the first retry (one failure) waits 30 seconds.
//
// The doubling is bounded at 16 before the shift rather than trusted: the cap is reached at 5, and
// bounding the exponent is what keeps the arithmetic in range for any caller value.
func deliveryRetryBackoff(attempted int) time.Duration {
	if attempted < 1 {
		attempted = 1
	}
	if attempted > 16 {
		return deliveryRetryCap
	}
	if d := deliveryRetryBase << (attempted - 1); d > 0 && d < deliveryRetryCap {
		return d
	}
	return deliveryRetryCap
}

// settleDelivery is the B-owned delivery-settlement core behind the A→B hook OnDeliverySettled
// (controller-integration D6 deliverySettled, IssueRun D3/D4/D5).
//
// It runs on the caller-owned *transaction of the delivery execution's terminal event, inside which
// everything it decides must commit or roll back with the receipt, the verification verdict, the
// Revision and the durable result. It never opens its own transaction, spawns a goroutine, or defers
// a post-commit effect.
//
// Contract (the rows are checked in this order, and the first one matches first):
//
//	phase != 'delivering'     → deterministic no-op: a replay or a late result decides nothing,
//	                            whether it reports a failure or a success.
//	saved / unchanged         → releasing with deliveryState=saved|unchanged and the registered
//	                            Revision id (releaseAfterDelivery). The id is part of the contract,
//	                            not an extra: the control plane writes the Revision before this hook
//	                            may see a success, so a success without one is an internal
//	                            contradiction rather than a run Cloud should release as saved.
//	failed, given up         → releasing with deliveryState=failed (releaseAfterDelivery).
//	failed, not given up     → the run STAYS `delivering` and a new delivery work item is released
//	                            with D5's backoff (a retry never creates a new logical delivery —
//	                            the run, its Revision target and its Workspace are unchanged).
//	skipped                  → releasing with deliveryState=skipped: the deployment has no object
//	                            store, so no attempt could ever succeed and retrying would hold the
//	                            Workspace against a limit that can never be reached.
//
// Identity is re-read, never taken from the caller: the run must be the one the registered delivery
// execution was dispatched for, through execution → execution_work → run, and the work item must be
// this run's `deliver_revision` work. A mismatch is an error that rolls the takeover back rather than
// a settlement projected onto some other run.
func (s *Store) settleDelivery(t *transaction, runID, executionID string, settled Object) error {
	if !validID(runID) {
		return fmt.Errorf("deliverySettled: invalid run id %q", runID)
	}
	o := t.one("SELECT * FROM issue_runs WHERE id=$1 AND deleted_at IS NULL", runID)
	if o == nil {
		return fmt.Errorf("deliverySettled: issue_run %s not found", runID)
	}
	if o.S("executorType") != "agent" {
		return fmt.Errorf("deliverySettled: run %s is executor_type=%q, not agent", runID, o.S("executorType"))
	}
	// The outcome kind is re-validated here even though the control plane reduced it: the hook is the
	// last gate before a business transition, and an unknown kind is an internal contradiction rather
	// than a decision any policy below can make.
	outcome := settled.S("outcome")
	switch outcome {
	case deliveryStateSaved, deliveryStateUnchanged, deliveryStateFailed, deliveryStateSkipped:
	default:
		return fmt.Errorf("deliverySettled: run %s carries unknown outcome %q", runID, outcome)
	}
	// A settlement that names an execution must name this run's own registered delivery execution.
	// The control plane's enqueue-time skip has no execution to name — it settles before any attempt
	// exists — which is why the check is conditional rather than unconditional.
	if executionID != "" {
		e := t.one("SELECT * FROM node_executions WHERE execution_id=$1", executionID)
		if e == nil {
			return fmt.Errorf("deliverySettled: execution %s is not registered", executionID)
		}
		if e.S("kind") != "deliver_revision" {
			return fmt.Errorf("deliverySettled: execution %s is kind=%q, not deliver_revision", executionID, e.S("kind"))
		}
		work := t.one("SELECT * FROM execution_work WHERE id=$1", e.S("workId"))
		if work == nil || work.S("runId") != runID || work.S("kind") != "deliver_revision" {
			// Matching execution → work → run is what proves this terminal result really belongs to
			// this run's delivery; a mismatch is refused rather than resolved to "some other run".
			return fmt.Errorf("deliverySettled: execution %s does not belong to run %s deliver_revision work", executionID, runID)
		}
	}

	// A settlement arriving for a run that already left `delivering` decides nothing, whatever it
	// carries — that is Cloud Revision invariant 9, and it is also what keeps this path live: the
	// control plane writes the receipt before this hook runs and rolls it back if the hook fails, so
	// refusing here would leave the Node's terminal event unacknowledged forever, the liveness hole
	// controller-integration D2's receipt design exists to prevent. Receipted as a plain fact about a
	// finished attempt, the same result lets the Node stop replaying once the run has moved on
	// (IssueRun D3: phases never move backwards).
	if o.S("phase") != "delivering" {
		return nil
	}

	switch outcome {
	case deliveryStateSaved, deliveryStateUnchanged:
		// The Revision was written by the control plane in this same transaction, before this hook ran
		// (revision.go), so its absence means the two layers disagree about what was committed, and
		// moving the run to `releasing` as saved would record a success no row backs (invariant 3).
		revisionID := settled.S("revisionId")
		if !validID(revisionID) {
			return fmt.Errorf("deliverySettled: run %s execution %s reports %s without a registered Revision", runID, executionID, outcome)
		}
		return s.releaseAfterDelivery(t, o, outcome, revisionID)
	case deliveryStateSkipped:
		// No object store is configured, so no delivery attempt was ever declared and none ever will
		// be: the control plane's enqueue recorded the skip instead of a work item. Release with
		// deliveryState=skipped, which is D4's own spelling for "no delivery happened", rather than
		// waiting on a give-up window that no attempt is running against.
		return s.releaseAfterDelivery(t, o, deliveryStateSkipped, "")
	}

	// A failure. The reason is either the wire's closed set or Cloud's own `verification_failed`
	// verdict, which the control plane writes when the declared objects could not be confirmed.
	// Accepting the second from the wire is what the control plane's revisionFailureReasons refuses,
	// and that refusal still stands — this branch sees the reason only after the control plane
	// decided it, never straight from a Node's payload.
	reason := settled.S("reason")
	if reason != revisionVerificationFailure && !revisionFailureReasons[reason] {
		return fmt.Errorf("deliverySettled: run %s execution %s carries failure reason %q, outside the closed set", runID, executionID, reason)
	}
	if s.deliveryGivenUp(t, o) {
		return s.releaseAfterDelivery(t, o, deliveryStateFailed, "")
	}
	// D5: the failure releases a new delivery execution and the run keeps `delivering` — the sandbox
	// stays running because the Revision is not registered yet. The retry is a fresh work item whose
	// object keys the control plane fixes at enqueue time, so it can never overwrite the objects of
	// the attempt that failed. The session execution is the run's settled `agent_session` one: the
	// delivery spec names it, and its durable result carries the end reason the final status is
	// derived from.
	sess := runSessionExecution(t, runID)
	if sess == nil {
		return fmt.Errorf("deliverySettled: run %s has no settled agent_session execution to retry the delivery of", runID)
	}
	spec, err := s.sessionDeliverySpec(t, o, sess.S("executionId"))
	if err != nil {
		return err
	}
	attempts := t.one(`
		SELECT count(*) AS n FROM node_executions e
		JOIN execution_work w ON w.id = e.work_id
		WHERE w.run_id=$1 AND w.kind='deliver_revision'`, runID).N("n")
	availableAt := time.Now().UTC().Add(deliveryRetryBackoff(int(attempts)))
	enqueueExecutionWork(t, runID, "deliver_revision", spec, sessionStartTarget(t, o.S("workspaceId")), "", availableAt)
	return nil
}
