package integration

// Acceptance for the public Thread end (Thread D4).
//
// The endpoint is the third of D4's three ending triggers and the only client-facing one. What it
// has to hold is narrower than the other two, and the tests below are shaped by that:
//
//   - It ends, it does not settle. The response is `202 {"threadState": "ending"}` and the run's own
//     phase/status are untouched: `ended`, `discarded`, Revision settlement and the Workspace delete
//     are settled elsewhere and stay out of this path.
//   - It is exactly one transition. One `EndSession{reason: user_ended}` command, one commit, and no
//     new Thread entry: ending is not something the conversation said, so it allocates no `seq`.
//   - It is idempotent under the caller's key, through the same generic mechanism every other public
//     POST uses — the precheck runs before the lifecycle is even read, so a replay is answered from
//     the recorded response rather than by re-deciding.
//
// Every case goes through real HTTP and real PostgreSQL, because those are the obligations: the
// idempotency record, the command and the state change have to be one commit, and the races below are
// races between real transactions sharing the real advisory lock.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/core"
)

func threadEndPath(tenant, issue, run string) string {
	return "/api/v1/tenants/" + tenant + "/issues/" + issue + "/runs/" + run + "/thread/end"
}

// endThread issues the production end request with the one legal body, asserting the exact status and
// — for the refusals — the stable public code the client branches on.
func (f *fixture) endThread(scene threadScene, key string, want int, code string) core.Object {
	f.t.Helper()
	return f.endThreadRaw(scene, key, `{}`, want, code)
}

// endThreadRaw is endThread with the body spelled out, so a case can send one the endpoint must
// refuse: `{}` is the whole accepted body, and every other shape is the strict decoder's business.
func (f *fixture) endThreadRaw(scene threadScene, key, body string, want int, code string) core.Object {
	f.t.Helper()
	status, out, e := f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), body, key)
	must(f.t, e)
	if status != want {
		f.t.Fatalf("POST thread/end: want %d got %d %v", want, status, out)
	}
	if code != "" && out.S("code") != code {
		f.t.Fatalf("POST thread/end: want code %q got %v", code, out)
	}
	return out
}

// endSessionReasons lists the `reason` of every end_session command the run holds, oldest first. It is
// the durable shape of "the session was asked to shut down, and why" — the run's own phase is not,
// because ending a Thread deliberately leaves it alone.
func (f *fixture) endSessionReasons(runID string) []string {
	f.t.Helper()
	rows, e := f.store.Pool.Query(`SELECT body->>'reason' FROM thread_commands WHERE run_id=$1 AND kind='end_session' ORDER BY created_at, id`, runID)
	must(f.t, e)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var reason string
		must(f.t, rows.Scan(&reason))
		out = append(out, reason)
	}
	must(f.t, rows.Err())
	return out
}

// threadRunState is the run's durable (phase, status, Thread state) triple.
func (f *fixture) threadRunState(runID string) (phase, status, threadState string) {
	f.t.Helper()
	must(f.t, f.store.Pool.QueryRow(`SELECT phase, status, thread_state FROM issue_runs WHERE id=$1`, runID).Scan(&phase, &status, &threadState))
	return phase, status, threadState
}

// The first request. One accepted end writes `ending`, releases exactly one
// `end_session` command with the user's reason, records the caller's idempotency response, and does
// nothing else: no entry, no `seq`, no change to the run's own phase/status, and no `ended`.
func TestThreadEndAcceptsTheFirstRequest(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	f.seedThreadEntry(scene.runID, "system", "user_turn", `{"content":"Begin this task."}`, nil, nil)

	phaseBefore, statusBefore, _ := f.threadRunState(scene.runID)
	out := f.endThread(scene, "end-1", 202, "")
	if got := out.S("threadState"); got != "ending" {
		t.Fatalf("the response must report the Thread ending, got %v", out)
	}
	// The response is the Thread state alone — the endpoint creates no resource to point at — and it
	// has to be the whole body, not a field of a larger one.
	if len(out) != 1 {
		t.Fatalf("the response must carry threadState and nothing else, got %v", out)
	}

	phase, status, threadState := f.threadRunState(scene.runID)
	if threadState != "ending" {
		t.Fatalf("the Thread must be ending, got %q", threadState)
	}
	// Ending is a request to the session, not a terminal state for the run: the shutdown, the
	// settlement and the Workspace delete all proceed on their own evidence (IssueRun D6).
	if phase != phaseBefore || status != statusBefore {
		t.Fatalf("ending a Thread must not move the run: %s/%s became %s/%s", phaseBefore, statusBefore, phase, status)
	}
	if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 || reasons[0] != "user_ended" {
		t.Fatalf("want exactly one end_session with reason user_ended, got %v", reasons)
	}
	// No new entry and no new seq: `ending` is not something the conversation said.
	if n := f.threadEntries(scene.runID); n != 1 {
		t.Fatalf("ending must append no entry, got %d", n)
	}
	if got := f.scalar(`SELECT COALESCE(MAX(seq),0) FROM thread_entries WHERE run_id=$1`, scene.runID); got != 1 {
		t.Fatalf("ending must allocate no seq, got %d", got)
	}
	f.assertEndRecorded(scene, "end-1", 202)
}

// assertEndRecorded proves the caller's key produced a durable idempotency record whose stored
// response is the 202 the caller saw — the thing that makes a replay a replay rather than a second
// decision.
func (f *fixture) assertEndRecorded(scene threadScene, key string, want int) {
	f.t.Helper()
	var stored int
	var response string
	must(f.t, f.store.Pool.QueryRow(`SELECT status, response::text FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, scene.tenantID, f.uid, key).Scan(&stored, &response))
	if stored != want {
		f.t.Fatalf("the recorded status must be the %d the caller saw, got %d", want, stored)
	}
	// jsonb's own text form is what comes back, so the recorded body is compared as a decoded value
	// rather than as a byte string: the response is the contract, not PostgreSQL's spacing.
	var recorded core.Object
	must(f.t, json.Unmarshal([]byte(response), &recorded))
	if fmt.Sprint(recorded) != fmt.Sprint(core.Object{"threadState": "ending"}) {
		f.t.Fatalf("the recorded response must be the one the caller saw, got %s", response)
	}
}

// One key, one transition. A replay is answered from the recorded response and
// must not release a second command, move the state again or write a second record; the boundary
// cases around it are the generic idempotency mechanism's, reused rather than reimplemented.
func TestThreadEndIsIdempotentUnderTheSameKey(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.endThread(scene, "end-replay", 202, "")

	// The replay. It is answered before the lifecycle is consulted — which is what the last case below
	// proves — so it returns the original body whether or not the Thread is still `ending`.
	replay := f.endThread(scene, "end-replay", 202, "")
	if fmt.Sprint(replay) != fmt.Sprint(first) {
		t.Fatalf("a replay must return the recorded response: %v then %v", first, replay)
	}
	if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 {
		t.Fatalf("a replay must not release a second command, got %v", reasons)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key='end-replay'`, scene.tenantID, f.uid); n != 1 {
		t.Fatalf("a replay must not write a second record, got %d", n)
	}

	// A new key against a Thread that is already on its way is the client-actionable conflict, not a
	// replay: the caller is telling Cloud to start an ending that has already started.
	f.endThread(scene, "end-second-key", 409, "thread_closed")
	// The same is true once the lifecycle has moved past ending. The state is written directly because
	// `ended` belongs to the session's terminal path (Phase 5), which this phase deliberately does not
	// implement.
	_, e := f.store.Pool.Exec(`UPDATE issue_runs SET thread_state='ended' WHERE id=$1`, scene.runID)
	must(t, e)
	f.endThread(scene, "end-after-ended", 409, "thread_closed")

	// The mechanism's own boundaries, which the four 4C endpoints share: no key at all is refused by
	// the transport contract, and the same key aimed at a different run is a conflict because the
	// recorded request is a different request.
	f.endThreadRaw(scene, "", `{}`, 400, "idempotency_key_required")
	other := scene.moreRun(t, f, "pending")
	f.endThread(other, "end-replay", 409, "idempotency_conflict")
	// A conflicting key must not have touched the run it named: the refusal happens before the
	// lifecycle read, so there is nothing to roll back and nothing to leak.
	if _, _, state := f.threadRunState(other.runID); state != "pending" {
		t.Fatalf("a conflicting key must leave the other run pending, got %q", state)
	}
	if reasons := f.endSessionReasons(other.runID); len(reasons) != 0 {
		t.Fatalf("a conflicting key must not release a command on the other run, got %v", reasons)
	}

	// The body is `{}` and nothing else, so the only "same key, different body" case the contract can
	// express is a body with a field in it — refused by the strict decoder before idempotency is even
	// consulted. That ordering is deliberate: a request Cloud cannot parse is not a request it may
	// replay.
	f.endThreadRaw(scene, "end-replay", `{"reason":"mine"}`, 400, "unknown_field")
	f.endThreadRaw(scene, "end-replay", ``, 400, "invalid_json")
	f.endThreadRaw(scene, "end-replay", `{} {}`, 400, "invalid_json")
}

// The replay's independence from the lifecycle, isolated: after the first success the Thread is moved
// all the way to `ended` and then re-read, and the *same* key still answers the original 202 while a
// new key answers 409. If the precheck ran after the lifecycle check, this is the case that would
// turn red — the recorded response would be shadowed by a conflict the caller already resolved.
func TestThreadEndReplayOutlivesTheStateItRecorded(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	first := f.endThread(scene, "end-replay", 202, "")
	_, e := f.store.Pool.Exec(`UPDATE issue_runs SET thread_state='ended' WHERE id=$1`, scene.runID)
	must(t, e)

	again := f.endThread(scene, "end-replay", 202, "")
	if fmt.Sprint(again) != fmt.Sprint(first) {
		t.Fatalf("a replay must return the recorded response whatever the state now is: %v then %v", first, again)
	}
	if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 {
		t.Fatalf("a replay after a state change must still not release a second command, got %v", reasons)
	}
}

// The concurrent replay. Two callers present the same key at the same instant;
// the advisory lock serializes them, so one commits the transition and the other replays it. Both see
// the same 202 and exactly one command exists. The start is synchronized with a channel and the
// assertion is on the complete set of permitted outcomes, never on a sleep.
func TestThreadEndConcurrentSameKeyMakesOneTransition(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)

	const callers = 4
	start := make(chan struct{})
	bodies := make([]core.Object, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, bodies[i], errs[i] = f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, "end-concurrent")
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range bodies {
		must(t, errs[i])
		if got := bodies[i].S("threadState"); got != "ending" {
			t.Fatalf("caller %d: want the recorded 202 body, got %v", i, bodies[i])
		}
	}
	if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 || reasons[0] != "user_ended" {
		t.Fatalf("concurrent replays must produce exactly one end_session, got %v", reasons)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key='end-concurrent'`, scene.tenantID, f.uid); n != 1 {
		t.Fatalf("concurrent replays must produce exactly one record, got %d", n)
	}
	if _, _, state := f.threadRunState(scene.runID); state != "ending" {
		t.Fatalf("concurrent replays must leave the Thread ending, got %q", state)
	}
}

// Authorization is the Thread read/write path's own, so this endpoint adds no
// permission model and no oracle. A caller with no membership is refused by the shared check exactly
// as the comments path refuses them; a member who names a run outside this tenant and Issue gets the
// same not-found they get from the Thread GET, so the endpoint cannot be used to probe which runs
// exist. Every refusal writes nothing.
func TestThreadEndAuthorizesLikeTheThreadRead(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	otherTenant := f.moreTenant()

	stranger, _ := f.addUser(t, "stranger", "Stranger")
	status, out, e := f.threadRequestAs(stranger, "POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, "end-stranger")
	must(t, e)
	if status != 403 || out.S("code") != "membership_required" {
		t.Fatalf("a non-member must be refused by the membership check, got %d %v", status, out)
	}

	for _, tc := range []struct{ name, tenant, issue, run string }{
		{"cross tenant", otherTenant, scene.issueID, scene.runID},
		{"cross issue", scene.tenantID, f.moreIssue(scene), scene.runID},
		{"unknown run", scene.tenantID, scene.issueID, newTestID()},
		{"run id is not an id", scene.tenantID, scene.issueID, "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, out, e := f.threadRequest("POST", threadEndPath(tc.tenant, tc.issue, tc.run), `{}`, "end-hidden")
			must(t, e)
			if status != 404 || out.S("code") != "not_found" {
				t.Fatalf("%s: want 404 not_found got %d %v", tc.name, status, out)
			}
		})
	}

	t.Run("a run with no declared session has no Thread to end", func(t *testing.T) {
		// Same answer the read gives: StartSession materializes `pending`, so an empty
		// state is a genuine absence rather than a conflict.
		runID := seedDeclaredAgentRun(t, f.store.Pool, scene.seed, "provisioning", "queued", false, false)
		status, out, e := f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, runID), `{}`, "end-undeclared")
		must(t, e)
		if status != 404 || out.S("code") != "not_found" {
			t.Fatalf("undeclared session: want 404 not_found got %d %v", status, out)
		}
	})

	t.Run("a human run has no Thread to end", func(t *testing.T) {
		runID := newTestID()
		_, e := f.store.Pool.Exec(`INSERT INTO issue_runs(id, tenant_id, issue_id, executor_type, executor_id) VALUES($1,$2,$3,'team',$4)`, runID, scene.tenantID, scene.issueID, newTestID())
		must(t, e)
		status, out, e := f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, runID), `{}`, "end-human")
		must(t, e)
		if status != 404 || out.S("code") != "not_found" {
			t.Fatalf("non-agent run: want 404 not_found got %d %v", status, out)
		}
	})

	// None of the refusals above may have written anything, on the scene's run or on the other one.
	if _, _, state := f.threadRunState(scene.runID); state != "pending" {
		t.Fatalf("a refused end must leave the Thread alone, got %q", state)
	}
	if total := f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.runID); total != 0 {
		t.Fatalf("a refused end must release no command, got %d", total)
	}
}

// The request's transaction is refused, so nothing about it survives: no
// `ending`, no command, and no idempotency record. The retry under the same key once the cause is gone
// is therefore a clean first request rather than a replay of a half-written end — which is exactly
// what the missing record proves.
//
// The refusal is induced at the database boundary: the merged design has no seam that can return an
// error here, because `enqueueThreadCommand` is in-transaction SQL whose body this path builds and
// validates itself. The command write is the transaction's last statement, so the `ending` CAS has
// already succeeded when the constraint refuses it.
func TestThreadEndWriteFailureRollsBackEverything(t *testing.T) {
	f := setup(t)
	f.bindBusinessHooks()
	scene := seedThreadScene(t, f)
	before := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID)
	_, e := f.store.Pool.Exec(`ALTER TABLE thread_commands ADD CONSTRAINT thread_commands_end_probe CHECK (kind <> 'end_session')`)
	must(t, e)

	f.endThread(scene, "end-503", 500, "internal_error")

	f.assertEndRolledBack(scene, before)
	_, e = f.store.Pool.Exec(`ALTER TABLE thread_commands DROP CONSTRAINT thread_commands_end_probe`)
	must(t, e)
	f.endThread(scene, "end-503", 202, "")
	if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 || reasons[0] != "user_ended" {
		t.Fatalf("the retry must release exactly the one command, got %v", reasons)
	}
}

// assertEndRolledBack is the durable half of "the whole request rolls back": the Thread state and the
// row version are where they were (only the ending CAS bumps the version), no command exists, and the
// caller's key left no record.
func (f *fixture) assertEndRolledBack(scene threadScene, versionBefore int) {
	f.t.Helper()
	_, _, state := f.threadRunState(scene.runID)
	if state != "pending" {
		f.t.Fatalf("a rolled-back end must leave the Thread pending, got %q", state)
	}
	if got := f.scalar(`SELECT version FROM issue_runs WHERE id=$1`, scene.runID); got != versionBefore {
		f.t.Fatalf("a rolled-back end must not bump the version: %d became %d", versionBefore, got)
	}
	if n := f.threadCommands(scene.runID); n != 0 {
		f.t.Fatalf("a rolled-back end must leave no command, got %d", n)
	}
	if n := f.scalar(`SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key='end-503'`, scene.tenantID, f.uid); n != 0 {
		f.t.Fatalf("a rolled-back end must leave no idempotency record, got %d", n)
	}
}

// The ending races. Each trigger (the user's request, the idle window, a
// cancellation request, a new user turn) may run first, and any interleaving must end in a legal
// serial outcome: exactly one transition to `ending` and exactly one EndSession command. The races are
// started from a barrier and the assertions accept the complete permitted set, never the common case —
// a test that only accepted one order would pass while the other lost a transition.
func TestThreadEndRacesProduceExactlyOneTransition(t *testing.T) {
	t.Run("against the idle window", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedThreadScene(t, f)
		f.store.ThreadIdleTimeout = 15 * time.Minute
		// An idle Thread whose window expired long ago, written at the storage boundary: the window is
		// judged by the database clock, so the instant is what makes the scan pick this run up.
		must(t, f.setThreadState(scene.runID, "running", "idle"))
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET idle_since=now() - interval '1 hour' WHERE id=$1`, scene.runID)
		must(t, e)
		before := f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.runID)

		var endStatus int
		var endBody core.Object
		var endErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			endStatus, endBody, endErr = f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, "end-race-idle")
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = f.store.EndIdleAgentThreadsOnce(context.Background())
		}()
		close(start)
		wg.Wait()
		must(t, endErr)

		// Either the user's request won (202, reason user_ended) or the window did (409 thread_closed,
		// reason idle_timeout). Both are legal; a third answer is not.
		reasons := f.endSessionReasons(scene.runID)
		if len(reasons) != 1 {
			t.Fatalf("the race must end the Thread exactly once, got %v", reasons)
		}
		switch reasons[0] {
		case "user_ended":
			if endStatus != 202 || endBody.S("threadState") != "ending" {
				t.Fatalf("an end that won the race must answer 202 ending, got %d %v", endStatus, endBody)
			}
		case "idle_timeout":
			if endStatus != 409 || endBody.S("code") != "thread_closed" {
				t.Fatalf("an end that lost to the window must answer 409 thread_closed, got %d %v", endStatus, endBody)
			}
		default:
			t.Fatalf("the race produced reason %q, which is neither trigger", reasons[0])
		}
		if _, _, state := f.threadRunState(scene.runID); state != "ending" {
			t.Fatalf("the race must leave the Thread ending, got %q", state)
		}
		if after := f.scalar(`SELECT count(*) FROM thread_commands WHERE run_id=$1`, scene.runID); after != before+1 {
			t.Fatalf("the race must release exactly one command: %d became %d", before, after)
		}
	})

	t.Run("against a cancellation request", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedThreadScene(t, f)
		_, e := f.store.Pool.Exec(`UPDATE issue_runs SET cancel_requested_at=now() WHERE id=$1`, scene.runID)
		must(t, e)

		var endStatus int
		var endBody core.Object
		var endErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			endStatus, endBody, endErr = f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, "end-race-cancel")
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = f.store.ReactToCancelledAgentRunsOnce(context.Background())
		}()
		close(start)
		wg.Wait()
		must(t, endErr)

		reasons := f.endSessionReasons(scene.runID)
		if len(reasons) != 1 {
			t.Fatalf("the race must end the Thread exactly once, got %v", reasons)
		}
		switch reasons[0] {
		case "user_ended":
			if endStatus != 202 {
				t.Fatalf("an end that won the race must answer 202, got %d %v", endStatus, endBody)
			}
		case "cancelled":
			if endStatus != 409 || endBody.S("code") != "thread_closed" {
				t.Fatalf("an end that lost to the cancel reaction must answer 409 thread_closed, got %d %v", endStatus, endBody)
			}
		default:
			t.Fatalf("the race produced reason %q, which is neither trigger", reasons[0])
		}
		if _, _, state := f.threadRunState(scene.runID); state != "ending" {
			t.Fatalf("the race must leave the Thread ending, got %q", state)
		}
	})

	t.Run("against a new user turn", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedThreadScene(t, f)

		var endStatus, postStatus int
		var endBody, postBody core.Object
		var endErr, postErr error
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			endStatus, endBody, endErr = f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, "end-race-post")
		}()
		go func() {
			defer wg.Done()
			<-start
			postStatus, postBody, postErr = f.threadRequest("POST", threadMessagesPath(scene.tenantID, scene.issueID, scene.runID), threadBody("one last thing"), "post-race-end")
		}()
		close(start)
		wg.Wait()
		must(t, endErr)
		must(t, postErr)

		// The end request never loses: `ending` is reachable from every accepting state, so whichever
		// order the two commits take, the user's request is either the first ending or a replay of it.
		if endStatus != 202 || endBody.S("threadState") != "ending" {
			t.Fatalf("the end must be accepted in either order, got %d %v", endStatus, endBody)
		}
		if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 || reasons[0] != "user_ended" {
			t.Fatalf("the race must release exactly one end_session, got %v", reasons)
		}
		// A turn accepted before the end is a real turn: it is in the log, it has its command, and it
		// was accepted into an open Thread. A turn that lost is refused exactly as a turn after any
		// other ending would be. There is no third answer.
		switch postStatus {
		case 201:
			if n := f.threadEntries(scene.runID); n != 1 {
				t.Fatalf("an accepted turn must be in the log, got %d entries", n)
			}
			if turns := f.commandTurns(scene.runID); len(turns) != 1 {
				t.Fatalf("an accepted turn must have its command, got %v", turns)
			}
		case 409:
			if postBody.S("code") != "thread_closed" {
				t.Fatalf("a turn that lost the race must be refused as thread_closed, got %d %v", postStatus, postBody)
			}
			if n := f.threadEntries(scene.runID); n != 0 {
				t.Fatalf("a refused turn must leave no entry, got %d", n)
			}
		default:
			t.Fatalf("a turn racing an end answered %d %v, which is neither outcome", postStatus, postBody)
		}
	})

	t.Run("twice from two keys", func(t *testing.T) {
		f := setup(t)
		f.bindBusinessHooks()
		scene := seedThreadScene(t, f)

		statuses := make([]int, 2)
		bodies := make([]core.Object, 2)
		errs := make([]error, 2)
		keys := []string{"end-two-keys-a", "end-two-keys-b"}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range statuses {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				statuses[i], bodies[i], errs[i] = f.threadRequest("POST", threadEndPath(scene.tenantID, scene.issueID, scene.runID), `{}`, keys[i])
			}(i)
		}
		close(start)
		wg.Wait()
		for i := range statuses {
			must(t, errs[i])
		}

		// Two different keys are two different requests, so exactly one may start the ending and the
		// other must be told the Thread is closed. Both being accepted would mean two transitions; both
		// being refused would mean the ending never started.
		accepted, refused := 0, 0
		for i := range statuses {
			switch statuses[i] {
			case 202:
				accepted++
				if bodies[i].S("threadState") != "ending" {
					t.Fatalf("the accepted caller must see the ending, got %v", bodies[i])
				}
			case 409:
				refused++
				if bodies[i].S("code") != "thread_closed" {
					t.Fatalf("the refused caller must see thread_closed, got %v", bodies[i])
				}
			default:
				t.Fatalf("a racing end answered %d %v, which is neither outcome", statuses[i], bodies[i])
			}
		}
		if accepted != 1 || refused != 1 {
			t.Fatalf("two keys must produce one ending and one conflict, got %d/%d", accepted, refused)
		}
		if reasons := f.endSessionReasons(scene.runID); len(reasons) != 1 {
			t.Fatalf("two keys must release exactly one end_session, got %v", reasons)
		}
	})
}
