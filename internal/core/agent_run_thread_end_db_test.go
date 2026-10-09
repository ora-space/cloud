package core

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

// Phase 4C S7 DB tests (T4C-31, T4C-32, T4C-33, T4C-34): the two ending triggers Cloud can reach
// without a new public API, and the Phase 5 boundary they must stop at.
//
// T4C-35 (the user-end endpoint) does not exist: G-018 is still an unapproved ADR supplement, so no
// endpoint implements `user_ended` and this file tests only `idle_timeout` and `cancelled`.
//
// The tests drive the shipped loops (EndIdleAgentThreadsOnce / ReactToCancelledAgentRunsOnce) and the
// real control-plane seam on a real isolated PostgreSQL schema, exactly as cmd/server wires them. The
// idle window is always staged by backdating `idle_since` through SQL, so nothing here waits on, or
// asserts against, a Go process clock — the comparison under test is the database's own.

// runningThreadScene brings a seeded scene to the state every ending trigger starts from: a live run
// whose Thread is `active` because a real Node record was taken over.
func runningThreadScene(t *testing.T, store *Store) takeoverScene {
	t.Helper()
	scene := seedTakeoverScene(t, store)
	if _, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(1, threadRecord("update", 0, "a"), "")}); err != nil {
		t.Fatalf("make the run running: %v", err)
	}
	return scene
}

// stageIdleThread puts a live run into the idle Thread state a scan reacts to, with `idle_since`
// backdated by age on the database's clock. Staging it rather than waiting for a real turn end keeps
// the test deterministic and keeps the window's arithmetic entirely inside PostgreSQL.
func stageIdleThread(t *testing.T, store *Store, runID string, age time.Duration) {
	t.Helper()
	if _, err := store.Pool.Exec(`
		UPDATE issue_runs
		SET thread_state='idle', idle_since=now()-make_interval(secs => $2), version=version+1, updated_at=now()
		WHERE id=$1`, runID, age.Seconds()); err != nil {
		t.Fatalf("stage idle Thread: %v", err)
	}
}

// requestCancel records the cancellation request the ending reaction consumes. No Cloud API writes
// this column yet (G-026), so a test states the request directly; the reaction under test is the
// shipped one either way.
func requestCancel(t *testing.T, store *Store, runID string) {
	t.Helper()
	if _, err := store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now(), version=version+1, updated_at=now() WHERE id=$1`, runID); err != nil {
		t.Fatalf("request cancel: %v", err)
	}
}

// endSessionRequests lists the reasons of every EndSession command the run holds, in creation order.
// Counting the *reasons* rather than the rows is what makes "exactly one EndSession, carrying the
// right reason" one assertion.
func endSessionRequests(t *testing.T, store *Store, runID string) []string {
	t.Helper()
	rows, err := store.Pool.Query(`SELECT body->>'reason' FROM thread_commands WHERE run_id=$1 AND kind='end_session' ORDER BY created_at, id`, runID)
	if err != nil {
		t.Fatalf("list EndSession commands: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var reason string
		if err := rows.Scan(&reason); err != nil {
			t.Fatalf("scan EndSession reason: %v", err)
		}
		out = append(out, reason)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate EndSession commands: %v", err)
	}
	return out
}

// idlePass runs one shipped idle-window pass.
func idlePass(t *testing.T, store *Store) {
	t.Helper()
	if err := store.EndIdleAgentThreadsOnce(context.Background()); err != nil {
		t.Fatalf("idle Thread pass: %v", err)
	}
}

// cancelPass runs one shipped cancellation-reaction pass.
func cancelPass(t *testing.T, store *Store) {
	t.Helper()
	if err := store.ReactToCancelledAgentRunsOnce(context.Background()); err != nil {
		t.Fatalf("cancel reaction pass: %v", err)
	}
}

// runIssueID reads the issue a run belongs to, so an assertion about the issue timeline does not have
// to reach through the scene's nesting.
func runIssueID(t *testing.T, store *Store, runID string) string {
	t.Helper()
	var issueID string
	if err := store.Pool.QueryRow(`SELECT issue_id FROM issue_runs WHERE id=$1`, runID).Scan(&issueID); err != nil {
		t.Fatalf("read run issue: %v", err)
	}
	return issueID
}

// deliveryState reads result.deliveryState, the column D6's no-session cancel must set to `skipped`.
func deliveryState(t *testing.T, store *Store, runID string) sql.NullString {
	t.Helper()
	var state sql.NullString
	if err := store.Pool.QueryRow(`SELECT result->>'deliveryState' FROM issue_runs WHERE id=$1`, runID).Scan(&state); err != nil {
		t.Fatalf("read deliveryState: %v", err)
	}
	return state
}

// threadHistory freezes the Thread exactly as stored. Comparing the whole slice before and after is
// how a test proves the 4C ending transitions left the history byte-for-byte alone: no `record`
// rewritten, no `seq` renumbered, no `turn_id` regenerated (Thread D1 invariant 4).
func threadHistory(t *testing.T, store *Store, runID string) []string {
	t.Helper()
	rows, err := store.Pool.Query(`
		SELECT seq::text||'|'||source||'|'||kind||'|'||record::text||'|'||COALESCE(turn_id::text,'')||'|'||created_at::text
		FROM thread_entries WHERE run_id=$1 ORDER BY seq`, runID)
	if err != nil {
		t.Fatalf("freeze Thread history: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan Thread history: %v", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate Thread history: %v", err)
	}
	return out
}

// TestIdleScanEndsAnExpiredThreadOnce (T4C-31, D-4C-10): a Thread whose idle window has expired is
// moved to `ending` with exactly one EndSession{idle_timeout} in its own transaction, a repeated tick
// emits no second request, and the run's own phase/status are untouched — an EndSession is a request,
// not a terminal state (IssueRun D6).
func TestIdleScanEndsAnExpiredThreadOnce(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := runningThreadScene(t, store)

	// Inside the window: an idle Thread is not yet endable, however long the process has been up.
	stageIdleThread(t, store, scene.run, 1*time.Minute)
	idlePass(t, store)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("an unexpired idle window must not end the Thread, got %v", state)
	}
	if got := endSessionRequests(t, store, scene.run); len(got) != 0 {
		t.Fatalf("an unexpired idle window must emit no EndSession, got %v", got)
	}

	// Past the window: exactly one EndSession, and the idle instant is cleared with the state so no
	// later reader can mistake an ending Thread for an expired idle one.
	stageIdleThread(t, store, scene.run, 20*time.Minute)
	idlePass(t, store)
	state, since := runThreadState(t, store, scene.run), idleAgeOnly(t, store, scene.run)
	if !state.Valid || state.String != "ending" {
		t.Fatalf("an expired idle window must move the Thread to ending, got %v", state)
	}
	if since.Valid {
		t.Fatalf("ending a Thread must clear idle_since, got %v", since)
	}
	reasons := endSessionRequests(t, store, scene.run)
	if len(reasons) != 1 || reasons[0] != "idle_timeout" {
		t.Fatalf("exactly one EndSession{idle_timeout} is expected, got %v", reasons)
	}
	// The request is durable and undelivered: nothing has claimed it yet, and the reason travels in
	// the body the Node will be handed.
	if n := countCommands(t, store, scene.run); n != 1 {
		t.Fatalf("ending must release exactly one command, got %d", n)
	}
	phase, status, _, _, _, _ := runFields(t, store, scene.run)
	if phase.String != "running" || status != "running" {
		t.Fatalf("ending a Thread must not move the run itself: %s/%s", phase.String, status)
	}

	// A second tick (and a third, for a scanner that somehow raced itself) finds nothing: the scan
	// predicate and the ending CAS both refuse a Thread that is already ending.
	idlePass(t, store)
	idlePass(t, store)
	if got := endSessionRequests(t, store, scene.run); len(got) != 1 {
		t.Fatalf("a repeated idle tick must emit no second EndSession, got %v", got)
	}
}

// TestIdleScanLeavesUnconfiguredAndUnfinishedWindowsAlone (T4C-31, D-4C-10): an unset window ends
// nothing at all — a zero-length window would shut down every live session the instant it went idle —
// and a Thread the POST already returned to `active` is not ended by a scan that ran afterwards.
func TestIdleScanLeavesUnconfiguredAndUnfinishedWindowsAlone(t *testing.T) {
	store := dispatcherDB(t)
	scene := runningThreadScene(t, store)

	// No configured window: the pass is a no-op even for a Thread that has been idle for hours.
	stageIdleThread(t, store, scene.run, 6*time.Hour)
	store.ThreadIdleTimeout = 0
	idlePass(t, store)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("an unconfigured window must end nothing, got %v", state)
	}
	if got := endSessionRequests(t, store, scene.run); len(got) != 0 {
		t.Fatalf("an unconfigured window must emit no EndSession, got %v", got)
	}

	// The same Thread, now configured and legitimately expired, ends — which proves the run above was
	// genuinely endable and only the missing window held it back.
	store.ThreadIdleTimeout = 15 * time.Minute
	idlePass(t, store)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
		t.Fatalf("the configured window must end the expired Thread, got %v", state)
	}

	// A POST that arrives inside the window returns the Thread to active and clears idle_since; the
	// scan that runs afterwards re-reads the same predicate and finds nothing (scenario 5 of §4C.14,
	// POST-first order).
	other := dispatcherDB(t)
	other.ThreadIdleTimeout = 15 * time.Minute
	second := runningThreadScene(t, other)
	stageIdleThread(t, other, second.run, 20*time.Minute)
	seedQueuedUserTurn(t, other, second.run, "still here")
	idlePass(t, other)
	if state := runThreadState(t, other, second.run); !state.Valid || state.String != "active" {
		t.Fatalf("a turn accepted inside the window must leave the Thread active, got %v", state)
	}
	if got := endSessionRequests(t, other, second.run); len(got) != 0 {
		t.Fatalf("a Thread activated by a POST must emit no EndSession, got %v", got)
	}
}

// idleAgeOnly reports whether idle_since is set, for assertions that only care about the column being
// cleared.
func idleAgeOnly(t *testing.T, store *Store, runID string) sql.NullString {
	t.Helper()
	since, _ := idleAge(t, store, runID)
	return since
}

// TestCancelWithoutASessionReleasesTheRun (T4C-32, IssueRun D6): a cancel that arrives before any
// session was declared takes D6's other outcome — the run releases straight from `starting` with
// deliveryState=skipped, its Workspace delete declared — and gets no `ending` state and no EndSession,
// because there is no session to shut down. The two cancel outcomes are distinct transitions.
func TestCancelWithoutASessionReleasesTheRun(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	if state := runThreadState(t, store, scene.seed.run); state.Valid {
		t.Fatalf("precondition: a run whose session was never declared has no thread_state, got %v", state)
	}

	requestCancel(t, store, scene.seed.run)
	cancelPass(t, store)

	phase, status, _, result, _, cancelAt := runFields(t, store, scene.seed.run)
	if phase.String != "releasing" || status != "cancelled" {
		t.Fatalf("a no-session cancel must release the run, got %s/%s", phase.String, status)
	}
	if !cancelAt.Valid {
		t.Fatalf("the cancellation request must stay recorded, got %v", cancelAt)
	}
	if state := deliveryState(t, store, scene.seed.run); !state.Valid || state.String != "skipped" {
		t.Fatalf("a no-session cancel delivers nothing, got deliveryState=%v", state)
	}
	if !result.Valid {
		t.Fatalf("the released run must keep its result object, got %v", result)
	}
	if state := runThreadState(t, store, scene.seed.run); state.Valid {
		t.Fatalf("the no-session cancel must not invent a Thread state, got %v", state)
	}
	if got := endSessionRequests(t, store, scene.seed.run); len(got) != 0 {
		t.Fatalf("the no-session cancel must emit no EndSession, got %v", got)
	}
	// The Workspace delete is declared in the same transaction as the releasing transition: a run is
	// never releasing while the delete intent did not commit (IssueRun invariant 3).
	if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
		t.Fatalf("exactly one delete declaration is expected, got %d", n)
	}
	if n := countRunActivities(t, store, runIssueID(t, store, scene.seed.run), "run.cancelled"); n != 1 {
		t.Fatalf("the cancel must be recorded once on the issue timeline, got %d", n)
	}
}

// TestCancelWithASessionEndsTheThread (T4C-32, IssueRun D6): once a session exists, the cancel ends
// the Thread instead — one transaction, one EndSession{cancelled} — for a Thread in any live state,
// including a `starting` run whose Thread is still `pending`.
//
// The `pending` case is the discriminator between D6's two outcomes: the plan names
// `provisioning|starting` for the releasing outcome, but D6 qualifies it with "（尚无会话）", and only
// that qualifier is consistent with IssueRun invariant 3. A `starting` run that has declared its
// session has a session to shut down, so releasing it — and deleting the Workspace its Agent has
// already written Thread content into — would be wrong.
func TestCancelWithASessionEndsTheThread(t *testing.T) {
	store := commandStore(t)

	// `starting` with the session already declared (thread_state='pending', written by StartSession).
	pending := seedTakeoverScene(t, store)
	if state := runThreadState(t, store, pending.run); !state.Valid || state.String != "pending" {
		t.Fatalf("precondition: the seeded scene declares its session, got %v", state)
	}
	requestCancel(t, store, pending.run)
	cancelPass(t, store)
	phase, status, _, _, _, _ := runFields(t, store, pending.run)
	if state := runThreadState(t, store, pending.run); !state.Valid || state.String != "ending" {
		t.Fatalf("a cancel with a declared session must end the Thread, got %v", state)
	}
	if reasons := endSessionRequests(t, store, pending.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("exactly one EndSession{cancelled} is expected, got %v", reasons)
	}
	if phase.String != "starting" || status != "dispatched" {
		t.Fatalf("ending a Thread must not release the run: %s/%s", phase.String, status)
	}
	if n := countRunActivities(t, store, runIssueID(t, store, pending.run), "run.cancelled"); n != 0 {
		t.Fatalf("the Thread ending must not write run.cancelled, got %d", n)
	}

	// A running Thread in each of the other two live states behaves identically.
	for _, tc := range []struct {
		name  string
		state string
		age   time.Duration
	}{
		{name: "active", state: "active"},
		{name: "idle", state: "idle", age: 1 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := commandStore(t)
			other.ThreadIdleTimeout = 15 * time.Minute
			scene := runningThreadScene(t, other)
			if tc.state == "idle" {
				stageIdleThread(t, other, scene.run, tc.age)
			}
			requestCancel(t, other, scene.run)
			cancelPass(t, other)
			if state := runThreadState(t, other, scene.run); !state.Valid || state.String != "ending" {
				t.Fatalf("a %s Thread must end on cancel, got %v", tc.state, state)
			}
			if since := idleAgeOnly(t, other, scene.run); since.Valid {
				t.Fatalf("ending a %s Thread must clear idle_since, got %v", tc.state, since)
			}
			if reasons := endSessionRequests(t, other, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
				t.Fatalf("exactly one EndSession{cancelled} is expected, got %v", reasons)
			}
			if phase, status, _, _, _, _ := runFields(t, other, scene.run); phase.String != "running" || status != "running" {
				t.Fatalf("ending a %s Thread must not move the run: %s/%s", tc.state, phase.String, status)
			}
		})
	}
}

// TestCancelAfterEndingEmitsNoSecondRequest (T4C-32, plan §4C.11): a cancel that arrives after the
// Thread is already `ending` records the request and nothing else — no second EndSession, no second
// reaction, no terminal state. The Thread ending path has exactly one request in it.
func TestCancelAfterEndingEmitsNoSecondRequest(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := runningThreadScene(t, store)

	// The idle window ends it first, carrying the idle reason.
	stageIdleThread(t, store, scene.run, 20*time.Minute)
	idlePass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "idle_timeout" {
		t.Fatalf("precondition: the idle window must end the Thread, got %v", reasons)
	}

	requestCancel(t, store, scene.run)
	cancelPass(t, store)
	cancelPass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "idle_timeout" {
		t.Fatalf("a cancel after the Thread ended must emit no second request, got %v", reasons)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
		t.Fatalf("the Thread stays ending, got %v", state)
	}
	// D6: the request is still recorded — the reaction is what is idempotent, not the record.
	if _, _, _, _, _, cancelAt := runFields(t, store, scene.run); !cancelAt.Valid {
		t.Fatal("the cancellation request must stay recorded")
	}
}

// TestCancelDuringProvisioningIsLeftToSettlement (T4C-32, IssueRun D3/D6): a cancel while the run is
// still `provisioning` belongs to the create_workspace operation's own terminal transaction, which
// already releases it. The Thread ending scan must not touch it — reacting here would declare a
// Workspace delete while the operation is still building that Workspace.
func TestCancelDuringProvisioningIsLeftToSettlement(t *testing.T) {
	store := dispatcherDB(t)
	// The scene is a run that owns a real run Workspace whose create_workspace operation has already
	// succeeded (that is what makes it `starting`), so a delete declaration from here would commit: the
	// scan's silence below is a decision, not a busy Project.
	starting := seedStartingRun(t, store, sessionStartInput())
	seed := starting.seed
	stageSettleState(t, store, seed.run, "provisioning", "dispatched", false)
	requestCancel(t, store, seed.run)

	cancelPass(t, store)

	phase, status, _, result, _, _ := runFields(t, store, seed.run)
	if phase.String != "provisioning" || status != "dispatched" {
		t.Fatalf("a provisioning cancel is settled by the workspace operation, not here: %s/%s", phase.String, status)
	}
	if state := runThreadState(t, store, seed.run); state.Valid {
		t.Fatalf("the Thread scan must not write thread_state here, got %v", state)
	}
	if state := deliveryState(t, store, seed.run); state.Valid {
		t.Fatalf("the Thread scan must not settle deliveryState here, got %v", state)
	}
	if result.Valid {
		t.Fatalf("the Thread scan must not write a result here, got %v", result)
	}
	if n := countDeleteOperations(t, store, seed.run); n != 0 {
		t.Fatalf("the Thread scan must not declare a delete, got %d", n)
	}
	if got := endSessionRequests(t, store, seed.run); len(got) != 0 {
		t.Fatalf("a provisioning cancel emits no EndSession, got %v", got)
	}
}

// TestPendingCancelWinsOverIdleTimeout (T4C-31/T4C-32, §4C.14 #5): a Thread that is both idle and
// carrying a cancellation request ends with `cancelled` and never with `idle_timeout` — the idle
// scan's predicate excludes a requested cancel, so the reason on the wire always identifies the
// trigger that actually fired.
func TestPendingCancelWinsOverIdleTimeout(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := runningThreadScene(t, store)

	stageIdleThread(t, store, scene.run, 20*time.Minute)
	requestCancel(t, store, scene.run)
	idlePass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 0 {
		t.Fatalf("the idle scan must not end a Thread whose cancel is pending, got %v", reasons)
	}

	cancelPass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("the pending cancel must be the trigger, got %v", reasons)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
		t.Fatalf("the Thread ends, got %v", state)
	}
}

// TestThreadEndingConcurrencyMatrix (T4C-33, §4C.14): the ending triggers raced against each other
// and against the takeover, driven concurrently on one real PostgreSQL schema with a channel barrier
// — no sleeps, no timing assumptions. Every scenario asserts the complete set of permitted serial
// outcomes, which is what the global advisory lock plus each exact CAS is supposed to produce: the
// losers observe the winner's committed state and become deterministic no-ops.
func TestThreadEndingConcurrencyMatrix(t *testing.T) {
	// A pair of racing operations, started from the same instant by a closed barrier.
	race := func(run, run2 func() error) []error {
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			errs[0] = run()
		}()
		go func() {
			defer wg.Done()
			<-start
			errs[1] = run2()
		}()
		close(start)
		wg.Wait()
		return errs
	}

	t.Run("two idle ticks emit one request", func(t *testing.T) {
		store := commandStore(t)
		store.ThreadIdleTimeout = 15 * time.Minute
		scene := runningThreadScene(t, store)
		stageIdleThread(t, store, scene.run, 20*time.Minute)

		for _, err := range race(
			func() error { return store.EndIdleAgentThreadsOnce(context.Background()) },
			func() error { return store.EndIdleAgentThreadsOnce(context.Background()) },
		) {
			if err != nil {
				t.Fatalf("concurrent idle pass failed: %v", err)
			}
		}
		if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "idle_timeout" {
			t.Fatalf("two racing ticks must emit exactly one EndSession{idle_timeout}, got %v", reasons)
		}
		if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
			t.Fatalf("the Thread ends, got %v", state)
		}
	})

	t.Run("idle tick and cancel race emit one request", func(t *testing.T) {
		store := commandStore(t)
		store.ThreadIdleTimeout = 15 * time.Minute
		scene := runningThreadScene(t, store)
		stageIdleThread(t, store, scene.run, 20*time.Minute)
		// The request is durable before either pass starts, so the idle scan's `cancel_requested_at IS
		// NULL` conjunct already excludes this Thread: whichever order the two passes serialize in,
		// the cancel is the trigger and the scan has nothing to do. Both orders therefore converge on
		// one identical outcome, which is what makes the assertion exact rather than a permitted set.
		requestCancel(t, store, scene.run)

		for _, err := range race(
			func() error { return store.EndIdleAgentThreadsOnce(context.Background()) },
			func() error { return store.ReactToCancelledAgentRunsOnce(context.Background()) },
		) {
			if err != nil {
				t.Fatalf("racing ending pass failed: %v", err)
			}
		}
		reasons := endSessionRequests(t, store, scene.run)
		if len(reasons) != 1 || reasons[0] != "cancelled" {
			t.Fatalf("a pending cancel must be the only trigger, got %v", reasons)
		}
		if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
			t.Fatalf("the Thread ends, got %v", state)
		}
		if n := countCommands(t, store, scene.run); n != 1 {
			t.Fatalf("ending releases exactly one command, got %d", n)
		}
	})

	t.Run("idle tick and takeover race", func(t *testing.T) {
		store := commandStore(t)
		store.ThreadIdleTimeout = 15 * time.Minute
		scene := runningThreadScene(t, store)
		stageIdleThread(t, store, scene.run, 20*time.Minute)

		// The takeover is legal after a POST: it appends the record either way, and only the loser of
		// the race has a lifecycle write to make. The permitted outcomes are exactly:
		//   scan first  → ending + one EndSession; the takeover appends its entry and finds no live
		//                 state to move, so it writes none.
		//   takeover first → idle→active with idle_since cleared; the scan re-reads and ends nothing.
		before := len(threadHistory(t, store, scene.run))
		errs := race(
			func() error { return store.EndIdleAgentThreadsOnce(context.Background()) },
			func() error {
				_, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(2, threadRecord("update", 1, "b"), "")})
				return err
			},
		)
		for _, err := range errs {
			if err != nil {
				t.Fatalf("racing takeover failed: %v", err)
			}
		}
		// The record is appended in both orders — an acked event is never dropped.
		if got := len(threadHistory(t, store, scene.run)); got != before+1 {
			t.Fatalf("the takeover must append its record whichever order won: had %d, now %d", before, got)
		}
		reasons := endSessionRequests(t, store, scene.run)
		state := runThreadState(t, store, scene.run)
		switch {
		case state.Valid && state.String == "ending":
			if len(reasons) != 1 || reasons[0] != "idle_timeout" {
				t.Fatalf("scan-first must leave exactly one EndSession{idle_timeout}, got %v", reasons)
			}
			if since := idleAgeOnly(t, store, scene.run); since.Valid {
				t.Fatalf("ending must clear idle_since, got %v", since)
			}
		case state.Valid && state.String == "active":
			if len(reasons) != 0 {
				t.Fatalf("takeover-first must emit no EndSession, got %v", reasons)
			}
			if since := idleAgeOnly(t, store, scene.run); since.Valid {
				t.Fatalf("returning to active must clear idle_since, got %v", since)
			}
		default:
			t.Fatalf("the race must land on ending or active, got %v (reasons %v)", state, reasons)
		}

		// The other half of the permitted set, driven deterministically instead of hoped for: when the
		// takeover commits first, the scan that follows must find an active Thread and end nothing.
		deterministic := commandStore(t)
		deterministic.ThreadIdleTimeout = 15 * time.Minute
		scene2 := runningThreadScene(t, deterministic)
		stageIdleThread(t, deterministic, scene2.run, 20*time.Minute)
		if _, err := takeOver(t, deterministic, scene2, scene2.execution, []Object{threadEvent(2, threadRecord("update", 1, "b"), "")}); err != nil {
			t.Fatalf("takeover before the scan: %v", err)
		}
		idlePass(t, deterministic)
		if state := runThreadState(t, deterministic, scene2.run); !state.Valid || state.String != "active" {
			t.Fatalf("a taken-over record must leave the Thread active, got %v", state)
		}
		if since := idleAgeOnly(t, deterministic, scene2.run); since.Valid {
			t.Fatalf("returning to active must clear idle_since, got %v", since)
		}
		if reasons := endSessionRequests(t, deterministic, scene2.run); len(reasons) != 0 {
			t.Fatalf("a Thread reactivated by the takeover must emit no EndSession, got %v", reasons)
		}
	})

	t.Run("cancel and takeover race", func(t *testing.T) {
		store := commandStore(t)
		scene := runningThreadScene(t, store)
		requestCancel(t, store, scene.run)
		before := len(threadHistory(t, store, scene.run))

		// Both orders converge: the takeover of a continuation record on an `active` Thread has no
		// lifecycle write to make either way, and the cancel ends the Thread exactly once. So the
		// outcome is deterministic — ending, one EndSession{cancelled}, every record appended.
		for _, err := range race(
			func() error { return store.ReactToCancelledAgentRunsOnce(context.Background()) },
			func() error {
				_, err := takeOver(t, store, scene, scene.execution, []Object{threadEvent(2, threadRecord("update", 1, "b"), "")})
				return err
			},
		) {
			if err != nil {
				t.Fatalf("racing cancel and takeover failed: %v", err)
			}
		}
		if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
			t.Fatalf("the cancel must end the Thread, got %v", state)
		}
		reasons := endSessionRequests(t, store, scene.run)
		if len(reasons) != 1 || reasons[0] != "cancelled" {
			t.Fatalf("exactly one EndSession{cancelled} is expected, got %v", reasons)
		}
		if got := len(threadHistory(t, store, scene.run)); got != before+1 {
			t.Fatalf("the takeover must still append its record: had %d, now %d", before, got)
		}
		if n := countCommands(t, store, scene.run); n != 1 {
			t.Fatalf("ending releases exactly one command, got %d", n)
		}
	})
}

// TestThreadHistoryIsImmutableAcrossEnding (T4C-34, Thread D1 invariant 4 / plan §4C.11): from a
// taken-over Thread through both ending triggers and every repeated tick, no Thread entry's `record`
// is rewritten and no `seq` is renumbered — the only mutable column on the whole 4C path is `status`,
// and it moves one way only. The GET half of T4C-34 stays deferred with S2a, which this round does
// not implement; what is provable at the storage boundary is proven here.
//
// The same walk also holds the Phase 5 boundary: nothing on this path writes `ended`, marks a queued
// turn `discarded`, or releases the Workspace.
func TestThreadHistoryIsImmutableAcrossEnding(t *testing.T) {
	store := commandStore(t)
	store.ThreadIdleTimeout = 15 * time.Minute
	scene := runningThreadScene(t, store)
	turnID, turnSeq := seedQueuedUserTurn(t, store, scene.run, "a turn the agent has not taken yet")

	// Deliver the queued turn and end the agent's turn in one batch, so the Thread history holds a
	// delivered user turn next to Node records before anything ends the Thread.
	if _, err := takeOver(t, store, scene, scene.execution, []Object{
		threadEvent(2, userTurnRecord(1, "the user turn"), turnID),
		threadEvent(3, threadRecord("turnEnded", 2, ""), ""),
	}); err != nil {
		t.Fatalf("deliver the user turn and end the agent's turn: %v", err)
	}
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "idle" {
		t.Fatalf("precondition: the turn end idles the Thread, got %v", state)
	}
	before := threadHistory(t, store, scene.run)

	// End the Thread by the idle window, then by cancel, then re-run both passes.
	stageIdleThread(t, store, scene.run, 20*time.Minute)
	idlePass(t, store)
	idlePass(t, store)
	requestCancel(t, store, scene.run)
	cancelPass(t, store)
	cancelPass(t, store)

	after := threadHistory(t, store, scene.run)
	if len(after) != len(before) {
		t.Fatalf("ending a Thread must not add or remove history: had %d, now %d", len(before), len(after))
	}
	for i := range before {
		if after[i] != before[i] {
			t.Fatalf("Thread history must be byte-for-byte immutable:\n had %s\n now %s", before[i], after[i])
		}
	}
	// seq stays contiguous from 1 with no gaps and no duplicates — the ending path renumbers nothing.
	if seqs := threadSeqs(t, store, scene.run); len(seqs) != len(before) {
		t.Fatalf("seq must not be renumbered: %v over %d entries", seqs, len(before))
	} else {
		for i, seq := range seqs {
			if seq != int64(i+1) {
				t.Fatalf("seq must stay contiguous from 1, got %v", seqs)
			}
		}
	}
	// The delivered user turn is still delivered: ending a Thread is not a reason to rewrite a turn's
	// lifecycle, and `discarded` is Phase 5's (the session end that finds a turn still queued).
	if _, _, status, _ := threadEntryRow(t, store, scene.run, turnSeq); status != "delivered" {
		t.Fatalf("ending a Thread must not rewrite a delivered turn, got %q", status)
	}
	if n := countDiscardedTurns(t, store, scene.run); n != 0 {
		t.Fatalf("`discarded` is Phase 5's transition, got %d", n)
	}
	if n := countEndedThreads(t, store); n != 0 {
		t.Fatalf("`ended` is Phase 5's transition, got %d runs", n)
	}
}

// countDiscardedTurns reports how many Thread entries a run holds in the Phase 5 `discarded` state.
func countDiscardedTurns(t *testing.T, store *Store, runID string) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM thread_entries WHERE run_id=$1 AND status='discarded'`, runID).Scan(&n); err != nil {
		t.Fatalf("count discarded turns: %v", err)
	}
	return n
}

// countEndedThreads reports how many runs reached the Phase 5 terminal Thread state.
func countEndedThreads(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM issue_runs WHERE thread_state='ended'`).Scan(&n); err != nil {
		t.Fatalf("count ended Threads: %v", err)
	}
	return n
}
