package core

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DB tests for the seam between the public cancellation request and the pass that reacts to it
// (IssueRun D6).
//
// D6 splits cancellation in two on purpose: the request is recorded and answered, and the reaction —
// ending the Thread, or releasing a run whose session was never declared — happens later, in the
// loop's own transaction. The two halves meet only on `issue_runs.cancel_requested_at`, and neither
// half can prove the seam alone: the reaction's tests stage the request directly through SQL
// (agent_run_thread_end_db_test.go), and the endpoint's acceptance proves only that it records and
// returns. What is proven here is that the request the endpoint writes is the request the pass
// consumes — the same column, the same two outcomes — so a writer that recorded somewhere else, or
// recorded something the pass ignores, fails these cases rather than passing both files separately.
//
// The writer is driven exactly as the router drives it (`Store.Public`, real transaction, real
// idempotency record); the pass is the shipped `ReactToCancelledAgentRunsOnce`.

// cancelCaller is the verified identity the cancellation endpoint is driven with: the scene's own
// tenant administrator, reached through a fresh `user_identities` binding rather than a second
// membership. Binding instead of inserting keeps the seed's "one active tenant, one active
// administrator" invariant untouched, and makes the request pass the same membership check the
// endpoint runs for every caller.
func cancelCaller(t *testing.T, store *Store, tenant string) *Claims {
	t.Helper()
	var userID string
	if err := store.Pool.QueryRow(`SELECT user_id FROM tenant_memberships WHERE tenant_id=$1 AND role='admin' AND status='active' ORDER BY user_id LIMIT 1`, tenant).Scan(&userID); err != nil {
		t.Fatalf("read the scene administrator: %v", err)
	}
	subject := "cancel-" + userID[:8]
	if _, err := store.Pool.Exec(`INSERT INTO user_identities(user_id,source,subject) VALUES($1,'test-cancel',$2)`, userID, subject); err != nil {
		t.Fatalf("bind the cancel caller: %v", err)
	}
	return &Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: subject}, Source: "test-cancel", Caller: "gateway", DisplayName: "Cancel Caller"}
}

// cancelRun drives the shipped writer the way the router does and returns the instant it recorded.
func cancelRun(t *testing.T, store *Store, seed dispSeed, runID, key string, caller *Claims) string {
	t.Helper()
	out, status, err := store.Public(context.Background(), &PublicRequest{
		Method: "POST", Key: key,
		Path:     "/api/v1/tenants/" + seed.tenant + "/issues/" + seed.issue + "/runs/" + runID + "/cancel",
		TenantID: seed.tenant, IssueID: seed.issue, RunID: runID,
		Body: Object{}, Identity: caller,
	})
	if err != nil {
		t.Fatalf("POST run/cancel: %v", err)
	}
	if status != 202 || out.S("cancelRequestedAt") == "" {
		t.Fatalf("POST run/cancel: want 202 carrying the recorded instant, got %d %v", status, out)
	}
	return out.S("cancelRequestedAt")
}

// TestCancelledRequestEndsTheDeclaredSession (IssueRun D6, Thread D4): a request written through the
// public endpoint reaches the pass as the `cancelled` trigger and ends the Thread once, with one
// EndSession{cancelled} and no release — the run has a session to shut down, so shutting it down is
// what cancellation means. Repeating the request before the pass, under a fresh key, adds nothing:
// the pass reacts to the request, not to the number of times it was asked for.
func TestCancelledRequestEndsTheDeclaredSession(t *testing.T) {
	store := commandStore(t)
	scene := runningThreadScene(t, store)
	caller := cancelCaller(t, store, scene.seed.seed.tenant)

	instant := cancelRun(t, store, scene.seed.seed, scene.run, "cancel-seam-1", caller)
	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "active" {
		t.Fatalf("recording the request must leave the Thread alone, got %v", state)
	}

	// The same request again, from a fresh key: the endpoint accepts it and returns the instant it
	// already recorded, so the pass below has exactly one request to react to.
	if again := cancelRun(t, store, scene.seed.seed, scene.run, "cancel-seam-2", caller); again != instant {
		t.Fatalf("a repeat must return the recorded instant %s, got %s", instant, again)
	}

	cancelPass(t, store)

	if state := runThreadState(t, store, scene.run); !state.Valid || state.String != "ending" {
		t.Fatalf("the recorded request must end the Thread, got %v", state)
	}
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 || reasons[0] != "cancelled" {
		t.Fatalf("exactly one EndSession{cancelled} is expected, got %v", reasons)
	}
	phase, status, _, _, _, cancelAt := runFields(t, store, scene.run)
	if phase.String != "running" || status != "running" {
		t.Fatalf("ending the Thread must not release the run: %s/%s", phase.String, status)
	}
	if !cancelAt.Valid {
		t.Fatalf("the request must stay recorded after the reaction, got %v", cancelAt)
	}
	// A second pass sees the Thread already ending and adds nothing: the request is consumed by the
	// reaction, not replayed by it.
	cancelPass(t, store)
	if reasons := endSessionRequests(t, store, scene.run); len(reasons) != 1 {
		t.Fatalf("a repeated pass must emit no second EndSession, got %v", reasons)
	}
}

// TestCancelledRequestReleasesTheUndeclaredSession (IssueRun D6): the same request against a run whose
// session was never declared takes D6's other outcome — the run releases with deliveryState=skipped,
// its Workspace delete declared and no EndSession, because there is no session to shut down. Both
// outcomes are reached from the one column the endpoint writes.
func TestCancelledRequestReleasesTheUndeclaredSession(t *testing.T) {
	store := commandStore(t)
	scene := seedStartingRun(t, store, sessionStartInput())
	if state := runThreadState(t, store, scene.seed.run); state.Valid {
		t.Fatalf("precondition: a run whose session was never declared has no thread_state, got %v", state)
	}
	caller := cancelCaller(t, store, scene.seed.tenant)

	instant := cancelRun(t, store, scene.seed, scene.seed.run, "cancel-seam-1", caller)
	cancelPass(t, store)

	phase, status, _, result, _, cancelAt := runFields(t, store, scene.seed.run)
	if phase.String != "releasing" || status != "cancelled" {
		t.Fatalf("a cancel with no session must release the run, got %s/%s", phase.String, status)
	}
	if !cancelAt.Valid {
		t.Fatalf("the request must stay recorded after the release, got %v", cancelAt)
	}
	if !result.Valid {
		t.Fatalf("the released run must keep its result object, got %v", result)
	}
	if state := deliveryState(t, store, scene.seed.run); !state.Valid || state.String != "skipped" {
		t.Fatalf("a no-session cancel delivers nothing, got deliveryState=%v", state)
	}
	if state := runThreadState(t, store, scene.seed.run); state.Valid {
		t.Fatalf("the no-session cancel must not invent a Thread state, got %v", state)
	}
	if reasons := endSessionRequests(t, store, scene.seed.run); len(reasons) != 0 {
		t.Fatalf("the no-session cancel must emit no EndSession, got %v", reasons)
	}
	if n := countDeleteOperations(t, store, scene.seed.run); n != 1 {
		t.Fatalf("exactly one delete declaration is expected, got %d", n)
	}
	if n := countRunActivities(t, store, runIssueID(t, store, scene.seed.run), "run.cancelled"); n != 1 {
		t.Fatalf("the release must record the cancellation once on the issue timeline, got %d", n)
	}
	// The instant the endpoint returned is the instant the outcome is decided from, so the release
	// must not have replaced or cleared it.
	reported, err := time.Parse(time.RFC3339, instant)
	if err != nil {
		t.Fatalf("the endpoint must report an instant: %v", err)
	}
	var recorded time.Time
	if err := store.Pool.QueryRow(`SELECT cancel_requested_at FROM issue_runs WHERE id=$1`, scene.seed.run).Scan(&recorded); err != nil {
		t.Fatalf("read the recorded instant: %v", err)
	}
	if !reported.Equal(recorded) {
		t.Fatalf("the released run must keep the reported instant %s, got %v", reported, recorded)
	}
}
