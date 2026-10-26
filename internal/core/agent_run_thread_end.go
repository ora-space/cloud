package core

import (
	"context"
	"fmt"
)

// Thread ending (Thread D4, IssueRun D4/D6).
//
// All three of D4's ending triggers live here: the idle window expiring, a cancellation request, and
// the user ending the conversation from the UI (`POST .../thread/end`). So does the writer of the
// cancellation request itself (`POST .../runs/{rid}/cancel`, cancelAgentRunByUser): the request and
// the reaction to it are one story, and keeping them in one file is what keeps D6's "cancellation is
// a request" visible from both ends — the API records and returns, the pass moves the Thread.
//
// Everything in this file stops at `thread_state = 'ending'`. Nothing here writes `ended`, marks a
// queued turn `discarded`, settles a Revision, moves the run to `delivering`/`releasing`/`done`, or
// deletes a Workspace: registered execution settles through `SessionEnded`, and an undispatched
// intent is reconciled by the startup loop. An `EndSession` command is a
// *request* — IssueRun D6 says so in as many words — so ending a Thread leaves the run's own
// `phase`/`status` exactly as they were and the session shutdown, delivery and Revision lifecycle
// proceed on their own evidence.

// agentThreadEndBatchSize bounds one scan pass so a single tick ends at most this many Threads.
// Ordering is deterministic (idle_since then id for the idle scan, created_at then id for the
// cancel scan) so every tick drains the oldest first; work beyond the bound waits for the next tick.
const agentThreadEndBatchSize = 100

// endAgentThread moves one authoritative run from a live Thread state to `ending` and releases
// exactly one EndSession command carrying reason, inside the caller's transaction.
//
// The CAS is exact rather than tolerant: the caller reached here only after
// re-reading a row this trigger applies to, every writer shares the one advisory lock, and the CAS
// repeats the state predicate the re-read approved — so zero affected rows contradicts the read
// itself. That is invariant corruption, and the only honest outcome is to roll the transaction back
// rather than to skip a transition a receipted request depends on.
//
// `idle_since` is cleared with the state (Thread D4): the instant is only meaningful while
// the Thread is idle, and leaving it behind would let a later reader — a second tick, or anything
// consulting the partial index — treat an already-ending Thread as an expired idle one.
// The command write cannot fail on anything the caller sent (its body is built and validated here),
// so there is no seam error to report: a database failure inside `enqueueThreadCommand` panics and
// rolls the whole caller's transaction back, which is what makes the retry under the same key a clean
// first request. What this function still refuses to tolerate is a CAS that moved no row — that is
// invariant corruption no caller can act on.
func (s *Store) endAgentThread(t *transaction, o Object, reason string) {
	runID := o.S("id")
	if moved := t.execRows(`
		UPDATE issue_runs
		SET thread_state='ending', idle_since=NULL, version=version+1, updated_at=now()
		WHERE id=$1 AND thread_state IN ('pending','active','idle')`, runID); moved != 1 {
		panic(databaseFailure{fmt.Errorf("end agent thread: run %s was not moved to ending (rows affected %d)", runID, moved)})
	}
	if o.S("phase") == "starting" {
		s.freezeAgentEnding(t, runID)
	}
	// The command is released through the control plane's own in-transaction seam
	// (controller-integration D6): the business layer never writes thread_commands, and a failure
	// rolls the `ending` write back with it, so a Thread can never be `ending` without the request
	// that makes it so. The commit that follows publishes ThreadCommandAvailable, never this call.
	enqueueThreadCommand(t, runID, "end_session", Object{"reason": reason})
	// `ending` is a state-only change — no entry, so no appended hint — but the Thread's REST
	// representation now reports a different threadState and a cleared idleSince, so A4's generalized
	// hint is exactly the one that covers it.
	threadChanged(t, o)
}

// endIdleAgentThread ends one run whose idle window has expired, in its own short transaction
// (Thread D4). The window and the comparison are entirely the database's: the caller
// passes a duration, the SQL compares `idle_since` against `now()`, and no Go process clock — let
// alone a Controller's or a Node's — can decide that a Thread is idle (Thread D4 invariant 5).
//
// The re-read repeats the scan's whole predicate, so a run that an answer, a cancel or another tick
// already moved is a deterministic no-op and never an error — that is what makes a second tick
// produce no second EndSession. `cancel_requested_at IS NULL` is part of the
// predicate so a pending cancellation always wins with `cancelled` rather than racing into
// `idle_timeout`; when both triggers do fire in the same instant, the CAS still lets exactly one of
// them commit.
func (s *Store) endIdleAgentThread(ctx context.Context, runID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT * FROM issue_runs
			WHERE id = $1
			  AND executor_type = 'agent'
			  AND deleted_at IS NULL
			  AND phase = 'running'
			  AND status = 'running'
			  AND thread_state = 'idle'
			  AND idle_since IS NOT NULL
			  AND cancel_requested_at IS NULL
			  AND (
				EXISTS(SELECT 1 FROM scheduled_task_occurrences o WHERE o.run_id=issue_runs.id)
				OR ($3 AND idle_since < now() - make_interval(secs => $2))
			  )`, runID, s.ThreadIdleTimeout.Seconds(), s.ThreadIdleTimeout > 0)
		if o == nil {
			return Object{}
		}
		s.endAgentThread(t, o, "idle_timeout")
		return Object{}
	})
	return err
}

// scanEndableIdleThreads returns the bounded, deterministically ordered ids of Threads whose idle
// window has expired. It is a read-only short scan outside the advisory lock — exactness lives in
// each endIdleAgentThread transaction, which re-reads the same predicate — mirroring the Phase 3A
// starting-retry scan. The `cancel_requested_at IS NULL` conjunct keeps a cancelled run out of this
// path so its ending carries the `cancelled` reason.
func (s *Store) scanEndableIdleThreads(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT id FROM issue_runs
		WHERE executor_type = 'agent'
		  AND deleted_at IS NULL
		  AND phase = 'running'
		  AND status = 'running'
		  AND thread_state = 'idle'
		  AND idle_since IS NOT NULL
		  AND cancel_requested_at IS NULL
		  AND (
			EXISTS(SELECT 1 FROM scheduled_task_occurrences o WHERE o.run_id=issue_runs.id)
			OR ($3 AND idle_since < now() - make_interval(secs => $2))
		  )
		ORDER BY idle_since, id
		LIMIT $1`, limit, s.ThreadIdleTimeout.Seconds(), s.ThreadIdleTimeout > 0)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// EndIdleAgentThreadsOnce is one bounded pass of the Thread idle-window scan: find the expired idle
// Threads (read-only) and end each in its own short transaction, with the sleep between ticks owned
// by the caller. Per-run failures are deliberately not surfaced — endIdleAgentThread already rolls
// back and leaves the run idle for a later tick, and the loop must not flood logs because one run is
// transiently stuck. Only a scan-level failure cancels the pass.
// A window that was never configured still leaves ordinary Threads alone. Scheduled occurrences
// are the sole approved exception: after their first successful turn they enter `idle` and must
// release EndSession{idle_timeout} immediately, independent of the deployment idle window.
func (s *Store) EndIdleAgentThreadsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanEndableIdleThreads(ctx, agentThreadEndBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.endIdleAgentThread(ctx, id)
	}
	return nil
}

// mayControlRun answers whether the caller may act on this run's live session — ending its Thread
// (Thread D4) or requesting its cancellation (IssueRun D6), the two run-level control actions the
// personal-model decision governs with one rule: on a run carrying a model Binding only that
// Binding's initiator and the tenant's current administrators may act, while an unbound run keeps
// Thread D3/D5's rule — any active tenant member, which Public already established before
// dispatching here. The rule is read from current PostgreSQL state, never from a caller-supplied
// field, and the caller's identity is resolved through the alias table so a renamed account keeps
// the right it earned.
//
// It answers rather than rejects so each caller names its own fault code: the two actions are
// distinct to a client ("you may not end this conversation" is not "you may not cancel this run"),
// and a shared helper that rejected would have to pick one of them.
func mayControlRun(t *transaction, run Object, r *PublicRequest) bool {
	binding := t.one("SELECT user_id FROM run_model_bindings WHERE run_id=$1", run.S("id"))
	if binding == nil {
		return true
	}
	uid := identityWithAlias(t, r.Identity).S("id")
	if uid == binding.S("userId") {
		return true
	}
	member := t.one("SELECT role FROM tenant_memberships WHERE tenant_id=$1 AND user_id=$2 AND status='active'", run.S("tenantId"), uid)
	return member != nil && member.S("role") == "admin"
}

// endThreadByUser is the public user-initiated end (Thread D4) behind
// `POST .../runs/{rid}/thread/end`. It runs on the caller's transaction, so the `ending` write, the
// EndSession command and the caller's idempotency record share one commit.
//
// Authorization is the Thread read/write path's own (Thread D3/D5: "same as comments"): the caller is
// an active tenant member — Public checked membership before dispatching here — and this resolves the
// Issue inside the caller's tenant, so a wrong tenant or Issue is a plain not-found, exactly like the
// Thread GET and POST. No new authorization model is invented, and a run outside the caller's scope
// stays indistinguishable from one that does not exist.
//
// The lifecycle check is the same closed set the other two triggers move out of: `pending | active |
// idle` end, `ending | ended` are already on their way and answer 409 thread_closed. A state outside
// D4's set is Cloud's own broken invariant and aborts as a 500 rather than being reported to the
// client as a conflict.
//
// It stops at `ending`. `ended` belongs to the session's terminal state (Thread invariant 4) and
// nothing here writes `discarded`, settles a Revision or deletes a Workspace — that is Phase 5.
func endThreadByUser(t *transaction, s *Store, r *PublicRequest) Object {
	i := issue(t, r.TenantID, r.IssueID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, i.S("id"))
	require(run != nil && run.S("executorType") == "agent", 404, "not_found")
	require(mayControlRun(t, run, r), 403, "model_run_end_forbidden")
	// A run whose session was never declared has no Thread to end: StartSession materializes `pending`,
	// so an empty state is a genuine absence, answered like every other
	// out-of-scope run rather than as a conflict.
	state := run.S("threadState")
	require(state != "", 404, "not_found")
	switch state {
	case "pending", "active", "idle":
	case "ending", "ended":
		reject(409, "thread_closed")
	default:
		panic(databaseFailure{fmt.Errorf("end thread by user: run %s has thread_state=%q, outside the closed set", run.S("id"), state)})
	}
	s.endAgentThread(t, run, "user_ended")
	return Object{"threadState": "ending"}
}

// cancelAgentRunByUser is the public cancellation request (IssueRun D6) behind
// `POST .../runs/{rid}/cancel`: the writer of the `issue_runs.cancel_requested_at` column that every
// consumer of a cancellation already reads. It runs on the caller's transaction, so the request and
// the caller's idempotency record share one commit.
//
// It records the request and writes nothing else. D6 makes cancellation a *request* — the run keeps
// its phase, status, Thread and Workspace, and the session shuts down on its own evidence — so the
// reaction is deliberately not inlined here: ending the Thread (or releasing a run whose session was
// never declared) belongs to reactToAgentRunCancel, which re-reads the recorded column under the same
// advisory lock on its next tick and is where a failed reaction is retried. Reacting here as well
// would put one transition in two places, and the second one would be reached by a caller whose
// request was already accepted.
//
// The window is the run's agent-execution window: a `queued` run (no phase yet) is cancellable,
// because the request outlives the dispatch it precedes — the dispatcher does not filter on it, and
// the run it provisions settles cancelled through settleRunWorkspace — and `provisioning`,
// `starting` and `running` are the three live phases. Everything past them answers 409
// run_not_cancellable: the session is over, and what remains is delivery and release, which D5's
// give-up limits own rather than the user.
//
// Authorization is mayControlRun's, the same rule as ending the Thread, and it is checked before the
// window so a caller who may not act on this run never learns whether it was still cancellable. An
// unbound run keeps the member rule; a bound one answers 403 model_run_cancel_forbidden.
//
// Repeating the request is a no-op that returns the instant the first one recorded, so the CAS writes
// only while `cancel_requested_at IS NULL`. That is also what makes the endpoint idempotent beyond
// its Idempotency-Key: a second click, a retry under a fresh key, and a cancel that arrives after the
// reaction already ran all leave the recorded instant — the one the run's outcome was decided from —
// untouched.
func cancelAgentRunByUser(t *transaction, r *PublicRequest) Object {
	i := issue(t, r.TenantID, r.IssueID)
	require(validID(r.RunID), 404, "not_found")
	run := t.one(`SELECT * FROM issue_runs WHERE id=$1 AND tenant_id=$2 AND issue_id=$3 AND deleted_at IS NULL`, r.RunID, r.TenantID, i.S("id"))
	require(run != nil && run.S("executorType") == "agent", 404, "not_found")
	require(mayControlRun(t, run, r), 403, "model_run_cancel_forbidden")
	if run.S("cancelRequestedAt") == "" {
		require(cancellableAgentRun(run), 409, "run_not_cancellable")
		// The CAS repeats the predicate the re-read approved, under the caller's advisory lock: zero
		// affected rows contradicts that read, so it is invariant corruption rather than a race to
		// tolerate, exactly as in endAgentThread.
		if moved := t.execRows(`
			UPDATE issue_runs
			SET cancel_requested_at=now(), version=version+1, updated_at=now()
			WHERE id=$1 AND cancel_requested_at IS NULL`, run.S("id")); moved != 1 {
			panic(databaseFailure{fmt.Errorf("cancel agent run: run %s was not marked cancelled (rows affected %d)", run.S("id"), moved)})
		}
		// Re-read rather than trust the application clock: the recorded instant is the database's, and
		// the response must be the one a later repeat reads back.
		run = t.one(`SELECT cancel_requested_at FROM issue_runs WHERE id=$1`, run.S("id"))
	}
	return Object{"cancelRequestedAt": run["cancelRequestedAt"]}
}

// cancellableAgentRun reports whether a cancellation request still has meaning for this run: D6's
// agent-execution window, which is `queued` (no phase yet) plus the three live phases. The remaining
// phases are named rather than folded into a default so that a phase outside the schema's closed set
// is reported as Cloud's broken invariant instead of being answered as a conflict — the same
// treatment endThreadByUser gives an unknown thread_state.
func cancellableAgentRun(run Object) bool {
	switch run.S("phase") {
	case "", "provisioning", "starting", "running":
		return true
	case "delivering", "releasing", "done":
		return false
	default:
		panic(databaseFailure{fmt.Errorf("cancel agent run: run %s has phase=%q, outside the closed set", run.S("id"), run.S("phase"))})
	}
}

// reactToAgentRunCancel is the B-owned reaction to a cancellation request (IssueRun D6).
// It runs in the caller's transaction and stops at `ending`, exactly like the idle path:
// the request is recorded and the session is asked to shut down, and nothing here makes the run
// terminal, releases it, or deletes its Workspace.
//
// Which of D6's two no-session outcomes applies is decided by `thread_state`, the durable record of
// whether a session was ever declared (StartSession materializes `pending`), and not
// by `phase` alone. The earlier design named `provisioning|starting` for the releasing outcome, but
// IssueRun D6 qualifies it — "取消发生在 provisioning/starting（尚无会话）时" — and only the
// qualifier is consistent with IssueRun invariant 3, which allows a `delete_workspace` declaration
// for the no-session cancel and for nothing else on this path. A `starting` run whose session was
// already declared therefore ends its Thread instead of being released: it has a session to shut
// down, and releasing it would delete a Workspace whose Agent has produced Thread content.
func (s *Store) reactToAgentRunCancel(ctx context.Context, runID string) error {
	_, err := s.transact(ctx, func(t *transaction) Object {
		o := t.one(`
			SELECT * FROM issue_runs
			WHERE id = $1
			  AND executor_type = 'agent'
			  AND deleted_at IS NULL
			  AND cancel_requested_at IS NOT NULL
			  AND phase IN ('starting','running')`, runID)
		if o == nil {
			// No pending cancel, or the run already settled past this path: a deterministic no-op.
			// A run whose cancel was already reacted to has thread_state='ending', which the ending
			// CAS below refuses — that is what makes the second tick emit no second EndSession.
			return Object{}
		}
		if o.S("threadState") == "" {
			// D6's cancel-before-session path: no session was ever declared, so there is nothing to
			// shut down and no Thread content a shutdown could preserve. The run releases
			// immediately with deliveryState=skipped. Deliberately no `ending` state and no
			// EndSession command — the two cancel outcomes are distinct transitions, not two
			// spellings of one.
			if err := s.releaseCancelledStartingRun(t, o); err != nil {
				panic(databaseFailure{err})
			}
			return Object{}
		}
		s.endAgentThread(t, o, "cancelled")
		return Object{}
	})
	return err
}

// releaseCancelledStartingRun moves a `starting` run whose cancel arrived before any session was
// declared straight to releasing/cancelled with result.deliveryState=skipped (IssueRun D6)
// and declares the Workspace delete in the same transaction.
//
// It is the sibling of settleRunWorkspace's provisioning cancel branch rather than a reuse of it:
// that branch runs on the create_workspace operation's terminal transaction and is gated on
// phase='provisioning', while this one runs for a run whose Workspace already exists and whose
// session was never declared. Both produce D6's one no-session outcome; keeping them apart is what
// keeps the create_workspace terminal from releasing a run whose Workspace is still being built.
//
// The CAS repeats the whole predicate the caller re-read, so zero affected rows is the same
// invariant contradiction endAgentThread refuses, not a race to tolerate.
func (s *Store) releaseCancelledStartingRun(t *transaction, o Object) error {
	runID := o.S("id")
	if moved := t.execRows(`
		UPDATE issue_runs
		SET phase='releasing', status='cancelled', completed_at=now(),
		    result = COALESCE(result, '{}'::jsonb) || '{"deliveryState":"skipped"}'::jsonb,
		    version=version+1, updated_at=now()
		WHERE id=$1 AND executor_type='agent' AND phase='starting'
		  AND thread_state IS NULL AND cancel_requested_at IS NOT NULL`, runID); moved != 1 {
		return fmt.Errorf("cancel agent run: run %s was not released (rows affected %d)", runID, moved)
	}
	appendActivity(t, o.S("tenantId"), o.S("issueId"), activityActor(o.S("executorType")), o.S("executorId"), "run.cancelled",
		runActivityDetails(runID, o.S("executorType"), o.S("executorId"), nil))
	// The delete declaration goes through the A seam in this transaction (D6): a seam failure rolls
	// the releasing transition back, so a run is never releasing without its delete intent.
	return s.declareDelete(t, o)
}

// scanCancelledAgentRuns returns the bounded, deterministically ordered ids of runs carrying an
// unreacted cancellation request. `phase='provisioning'` is deliberately absent: a cancel during
// provisioning is settled by the create_workspace operation's own terminal transaction (IssueRun
// D3/D6), and reacting here would declare a Workspace delete while that operation is still building
// the Workspace. Terminal and delivering phases are absent for the same reason — 4C ends a Thread
// only while it is live.
func (s *Store) scanCancelledAgentRuns(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.Pool.QueryContext(ctx, `
		SELECT id FROM issue_runs
		WHERE executor_type = 'agent'
		  AND deleted_at IS NULL
		  AND cancel_requested_at IS NOT NULL
		  AND phase IN ('starting','running')
		  AND (thread_state IS NULL OR thread_state IN ('pending','active','idle'))
		ORDER BY created_at, id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ReactToCancelledAgentRunsOnce is one bounded pass of the cancellation reaction: find the runs
// whose cancellation request has not been reacted to yet (read-only) and react to each in its own
// short transaction. Per-run failures are not surfaced, for the same reason as the idle pass: the
// transaction rolled back, the run is unchanged, and the next tick retries it. Only a scan-level
// failure cancels the pass.
//
// The request is written by cancelAgentRunByUser (`POST .../runs/{rid}/cancel`); this pass is the
// reaction to it, and the two meet only on the column, so a request recorded while this pass is
// mid-tick is picked up by the next one.
func (s *Store) ReactToCancelledAgentRunsOnce(ctx context.Context) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	ids, err := s.scanCancelledAgentRuns(ctx, agentThreadEndBatchSize)
	if err != nil {
		return err
	}
	for _, id := range ids {
		_ = s.reactToAgentRunCancel(ctx, id)
	}
	return nil
}
