package core

import (
	"context"
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// This file covers the dispatch pass: the loop that carries out an approved write on the process's own
// context, the lease that bounds one attempt at it, and the generation that fences that attempt's answer.
//
// The property every case here rests on is the one the request-local dispatch could not have: the write
// belongs to the approval, not to the request that recorded it. An attempt may therefore be made, lost,
// and made again — and what makes that safe rather than a second write is not this file at all, it is the
// executor's compare-and-swap on the authorized commit pair. Cloud never infers from silence that a write
// did not happen; it re-attempts and records what the executor establishes.

// approveForDispatch records one approval through the real accepting function and returns its identifier
// with the executor left unasked.
//
// It is `approveWithoutDispatching`'s call, named for what these cases do with it: the window it opens is
// the state every case below starts from, and the pass is what closes it.
func approveForDispatch(t *testing.T, store *Store, scene pushScene, requestID string) string {
	t.Helper()
	return approveWithoutDispatching(t, store, scene, requestID)
}

// lapseLease moves an attempt's lease into the past, which is what a process that died mid-dispatch
// leaves behind: a deadline nobody will ever release, and a row no pass may claim until it lapses.
//
// Waiting the real lease out is not an option — it is over a minute — so the case moves the only clock
// the lease is measured against, which is the database's, on the row's own deadline.
func lapseLease(t *testing.T, store *Store, id string) {
	t.Helper()
	stagedExec(t, store, "UPDATE push_requests SET dispatch_lease_expires_at=now()-interval '1 second' WHERE id=$1", id)
}

// freezeCredential makes the credential unusable for a write without revoking the binding, which is the
// guard refusal an operator meets most often and the one whose code these cases read.
func freezeCredential(t *testing.T, store *Store, scene pushScene) {
	t.Helper()
	stagedExec(t, store, "UPDATE credential_refs SET availability='invalid',unavailable_reason='revoked_by_owner' WHERE id=$1", scene.credential)
}

// TestAnApprovedWriteIsCarriedOutByThePassAndNotByTheRequest is the correction itself, asserted where a
// regression would show: the accepting call returns with the executor untouched.
//
// The defect this replaces ran the dispatch inside the request, so the write and its settlement lived and
// died with the caller. A caller that hung up — or a process killed mid-write — left the approval
// `dispatching` with nothing anywhere reading that state. Here the request ends first and the write
// happens afterwards, on the loop's context, which is what makes the caller's `202` a promise the
// deployment keeps rather than a hope.
func TestAnApprovedWriteIsCarriedOutByThePassAndNotByTheRequest(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)

	out, status, err := scene.pushRequest(store, "key-pass", "req-pass", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("approve a push: %v", err)
	}
	if status != 202 || out.S("state") != pushDispatching {
		t.Fatalf("accepting a push must answer 202 with a dispatching plan, got %d %v/%v", status, out["state"], out["resultCode"])
	}
	if len(executor.calls) != 0 {
		t.Fatalf("the accepting request dispatched the write %d times, want none", len(executor.calls))
	}

	dispatchPushes(t, store)

	if len(executor.calls) != 1 {
		t.Fatalf("the pass dispatched the write %d times, want exactly 1", len(executor.calls))
	}
	if got := pushState(t, store, out.S("id")); got != pushSucceeded+"/"+pushApplied {
		t.Fatalf("the carried-out write settled as %q, want a success", got)
	}
	// The first attempt carries the authorization the approval was recorded under, so the identity the
	// approving caller was handed is the one the Store would record consumption under. Only a re-attempt
	// re-issues, and a first attempt that quietly re-issued would leave the caller holding a name for
	// nothing.
	if executor.calls[0].AuthorizationID != out.S("authorizationId") {
		t.Fatalf("the first attempt redeemed %q, want the approval's own %q", executor.calls[0].AuthorizationID, out.S("authorizationId"))
	}
	row := pushRow(t, store, out.S("id"))
	if got := row.N("dispatchGeneration"); got != 1 {
		t.Fatalf("the first attempt left generation %d, want 1", got)
	}
	// A settled approval has no attempt left to wait for, and the table's CHECK enforces the pair: a
	// settlement that forgot to release the lease would fail the write rather than leave a settled row
	// reading as one still being dispatched.
	if row["dispatchLeaseExpiresAt"] != nil {
		t.Fatalf("a settled approval kept the lease %v", row["dispatchLeaseExpiresAt"])
	}
}

// TestACleanupFailureIsRecordedAsUncertainAndNeverAsAConcludedFailure is the durable half of the
// cleanup-failure rule, and it is the defect the earlier shape produced: an executor that confirmed the
// write and then could not prove its own copy of the credential destroyed reported a refusal, so the
// approval read `failed` for a write the upstream had already applied and the commit the executor had
// observed never reached the audit.
//
// What the row must say instead is both facts. The external effect is not something Cloud will call
// completed — `uncertain`, under the executor's own code, because "the material may outlive the attempt"
// is a caveat about Cloud's certainty rather than evidence that the remote is unchanged — while the
// observation is kept, so a reader can still see what the remote held. `failed` is the one answer this
// case rejects in both columns: it is a claim about the remote, and the remote moved.
//
// It runs through the real pass, the real claim and the real settlement, so what is asserted is the row a
// deployment produces rather than the mapping in isolation.
func TestACleanupFailureIsRecordedAsUncertainAndNeverAsAConcludedFailure(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{
		Verdict:        PushConfirmed,
		ObservedCommit: pushNewCommit,
		CleanupFailure: cloneexec.ReasonCodeCleanupFailed,
	}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-cleanup")

	dispatchPushes(t, store)

	if len(executor.calls) != 1 {
		t.Fatalf("the pass dispatched the write %d times, want exactly 1", len(executor.calls))
	}
	want := pushUncertain + "/" + cloneexec.ReasonCodeCleanupFailed
	if got := pushState(t, store, id); got != want {
		t.Fatalf("a confirmed write whose executor could not clean up settled as %q, want %q", got, want)
	}
	row := pushRow(t, store, id)
	if row.S("state") != pushUncertain {
		t.Fatalf("the write settled in state %q, want %q: a write that landed is never recorded as a refusal", row.S("state"), pushUncertain)
	}
	// The observation survives the caveat. It is what the verdict was drawn from, and the cleanup failure
	// is a fact about the executor's disk rather than about the remote, so it may not suppress it.
	if got := pushObserved(t, store, id); got != pushNewCommit {
		t.Fatalf("observed commit = %q, want the commit the executor read back %q", got, pushNewCommit)
	}
	// And it is a settlement rather than a row left behind: an unsettled approval would read `dispatching`,
	// which is a claim that an attempt is still in flight.
	if row["settledAt"] == nil {
		t.Fatal("an uncertain settlement left the approval looking dispatched")
	}

	// Terminal: the authorization behind the attempt is spent, so an uncertain write is never re-attempted
	// and the caveat is not a reason to try again.
	dispatchPushes(t, store)
	if len(executor.calls) != 1 {
		t.Fatalf("an uncertain approval was dispatched %d times, want exactly 1", len(executor.calls))
	}
	if got := pushState(t, store, id); got != want {
		t.Fatalf("a later pass revised the settlement to %q", got)
	}
}

// TestALapsedAttemptIsReattemptedWithAFreshAuthorization is the reconciliation, asserted on the two
// things it must get right at once.
//
// A row whose attempt never came back is re-attempted rather than abandoned, because the remote can
// still be asked and the answer is worth having. And the re-attempt is issued a *new* authorization: the
// first one may have been redeemed with its answer lost, and a spent authorization is never replayed
// (D7). The new identifier is what makes the second attempt a second attempt rather than a replay.
func TestALapsedAttemptIsReattemptedWithAFreshAuthorization(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-reattempt")

	first, err := store.claimPushDispatch(context.Background(), id)
	if err != nil || first == nil {
		t.Fatalf("claim the first attempt: %v (claim %v)", err, first)
	}
	lapseLease(t, store, id)

	dispatchPushes(t, store)

	if len(executor.calls) != 1 {
		t.Fatalf("the re-attempt reached the executor %d times, want exactly 1", len(executor.calls))
	}
	if executor.calls[0].AuthorizationID == first.Execution.AuthorizationID {
		t.Fatal("the re-attempt replayed the first attempt's authorization")
	}
	row := pushRow(t, store, id)
	if got := row.N("dispatchGeneration"); got != 2 {
		t.Fatalf("the re-attempt left generation %d, want 2", got)
	}
	if row.S("authorizationId") != executor.calls[0].AuthorizationID {
		t.Fatalf("the row names %q but the attempt redeemed %q", row.S("authorizationId"), executor.calls[0].AuthorizationID)
	}
	if got := pushState(t, store, id); got != pushSucceeded+"/"+pushApplied {
		t.Fatalf("the re-attempt settled as %q, want a success", got)
	}
}

// TestAReattemptNeverRecordsAPossibleWriteAsAFailure is the direction a write must never be settled in,
// reached through the reconciliation rather than through a single attempt.
//
// The first attempt's answer was lost, and the second finds the authorized commit already on the ref.
// That is evidence the write landed — and no evidence that this approval is what landed it — so it
// settles `uncertain`/`push_attribution_unknown` with the observation kept. What must not happen is
// `failed`: an attempt that reached the remote may have written, and recording that as a refusal is
// Cloud asserting an outcome nobody established.
func TestAReattemptNeverRecordsAPossibleWriteAsAFailure(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushTargetSatisfied, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-landed")

	if _, err := store.claimPushDispatch(context.Background(), id); err != nil {
		t.Fatalf("claim the first attempt: %v", err)
	}
	lapseLease(t, store, id)

	dispatchPushes(t, store)

	if got := pushState(t, store, id); got != pushUncertain+"/"+pushAttributionUnknown {
		t.Fatalf("a re-attempt over an already-written ref settled as %q, want uncertain/%s", got, pushAttributionUnknown)
	}
	// The evidence the verdict was drawn from survives, which is what makes the uncertain row checkable
	// rather than merely declared.
	if got := pushObserved(t, store, id); got != pushNewCommit {
		t.Fatalf("the reconciliation recorded the observation %q, want %q", got, pushNewCommit)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("the re-attempt reached the executor %d times, want exactly 1", len(executor.calls))
	}
}

// TestTheAttemptBudgetEndsAWriteWhoseOutcomeCannotBeEstablished is the other half of re-attempting: it
// stops.
//
// A write whose outcome no attempt has established cannot be re-attempted forever, so the budget is
// spent and the approval settles with the honest answer — `uncertain`/`push_result_unknown`, never
// `failed`, because the last attempt may have written. The case also asserts the settlement is a
// settlement: the next pass finds nothing owed, so the approval is not re-claimed and the budget is not
// renewed.
func TestTheAttemptBudgetEndsAWriteWhoseOutcomeCannotBeEstablished(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-budget")

	// The budget is spent by claims that never answer, which is the shape of a process dying mid-dispatch
	// repeated until the bound is reached.
	for attempt := 1; attempt <= pushDispatchAttempts; attempt++ {
		claimed, err := store.claimPushDispatch(context.Background(), id)
		if err != nil || claimed == nil {
			t.Fatalf("claim attempt %d: %v (claim %v)", attempt, err, claimed)
		}
		lapseLease(t, store, id)
	}
	if got := pushRow(t, store, id).N("dispatchGeneration"); got != int64(pushDispatchAttempts) {
		t.Fatalf("the spent budget left generation %d, want %d", got, pushDispatchAttempts)
	}

	dispatchPushes(t, store)

	if len(executor.calls) != 0 {
		t.Fatalf("a write with no attempts left reached the executor %d times", len(executor.calls))
	}
	want := pushUncertain + "/" + pushResultUnknown
	if got := pushState(t, store, id); got != want {
		t.Fatalf("a spent attempt budget settled as %q, want %q", got, want)
	}
	if row := pushRow(t, store, id); row["settledAt"] == nil || row["dispatchLeaseExpiresAt"] != nil {
		t.Fatalf("the terminal settlement left the row unsettled or leased: %v/%v", row["settledAt"], row["dispatchLeaseExpiresAt"])
	}
	dispatchPushes(t, store)
	if got := pushState(t, store, id); got != want {
		t.Fatalf("a later pass rewrote a settled approval to %q", got)
	}
}

// TestAnAttemptInsideItsLeaseIsNotReplaced is the bound that keeps two attempts from overlapping.
//
// A live lease means an attempt may still be running, and starting a second one beside it would spend a
// second credential redemption on a write the first may be about to make. Both gates are asserted: the
// candidate scan skips the row, and the claim — which is where a second replica could otherwise slip in
// between the scan and the lock — refuses it again under the row lock.
func TestAnAttemptInsideItsLeaseIsNotReplaced(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-leased")

	first, err := store.claimPushDispatch(context.Background(), id)
	if err != nil || first == nil {
		t.Fatalf("claim the attempt: %v (claim %v)", err, first)
	}

	dispatchPushes(t, store)

	if len(executor.calls) != 0 {
		t.Fatalf("an approval inside its lease reached the executor %d times", len(executor.calls))
	}
	again, err := store.claimPushDispatch(context.Background(), id)
	if err != nil {
		t.Fatalf("claim an approval inside its lease: %v", err)
	}
	if again != nil {
		t.Fatalf("a live lease was claimed a second time: %+v", again)
	}
	row := pushRow(t, store, id)
	if row.N("dispatchGeneration") != 1 || row.S("authorizationId") != first.Execution.AuthorizationID {
		t.Fatalf("a refused claim changed the attempt: generation %d, authorization %q", row.N("dispatchGeneration"), row.S("authorizationId"))
	}
	if got := pushState(t, store, id); got != pushDispatching+"/" {
		t.Fatalf("the approval reads %q, want it still dispatching", got)
	}
}

// TestAnAnswerFromAReplacedAttemptIsRefused pins the generation fence, which is the half of the safety
// argument the lease cannot supply.
//
// The lease decides when an attempt may be *replaced*; it says nothing about an answer that arrives
// afterwards. The first attempt here finally reports success after a second attempt has claimed the row:
// applying it would record an outcome established about an attempt that is over, and would let a slow or
// replayed reply decide the approval's fate. The current attempt's answer is accepted in the same case,
// which is what shows the refusal is about the fence rather than about a settlement that never works.
func TestAnAnswerFromAReplacedAttemptIsRefused(t *testing.T) {
	store := dispatcherDB(t)
	store.SetPushExecutor(&recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed}})
	scene := seedPushScene(t, store, `["push"]`)
	id := approveForDispatch(t, store, scene, "req-fence")

	first, err := store.claimPushDispatch(context.Background(), id)
	if err != nil || first == nil {
		t.Fatalf("claim the first attempt: %v (claim %v)", err, first)
	}
	lapseLease(t, store, id)
	second, err := store.claimPushDispatch(context.Background(), id)
	if err != nil || second == nil {
		t.Fatalf("claim the replacing attempt: %v (claim %v)", err, second)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("the replacing attempt holds generation %d, want more than %d", second.Generation, first.Generation)
	}

	confirmed := PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}
	applied, err := store.settlePushDispatch(context.Background(), *first, confirmed, nil)
	if err != nil {
		t.Fatalf("settle from the replaced attempt: %v", err)
	}
	if applied {
		t.Fatal("a replaced attempt's answer settled the approval")
	}
	if got := pushState(t, store, id); got != pushDispatching+"/" {
		t.Fatalf("a refused settlement changed the row to %q", got)
	}

	applied, err = store.settlePushDispatch(context.Background(), *second, confirmed, nil)
	if err != nil || !applied {
		t.Fatalf("the current attempt's answer was refused: %v (applied %v)", err, applied)
	}
	if got := pushState(t, store, id); got != pushSucceeded+"/"+pushApplied {
		t.Fatalf("the current attempt settled as %q, want a success", got)
	}
}

// TestARefusalIsConcludedOnlyBeforeAnyAttemptWasMade is the one question the claim's refusal has to be
// read through: had an attempt already been claimed?
//
// Before one, the guard's refusal *is* the outcome — nothing was sent anywhere, so nothing was written,
// and the code names the repair. After one, the same refusal establishes nothing about the remote,
// because that attempt may have written and never reported, so it settles `uncertain` instead of recording
// a possible write as a refusal. And a failure Cloud cannot classify at all settles nothing, which leaves
// the row for the next pass rather than inventing a conclusion.
func TestARefusalIsConcludedOnlyBeforeAnyAttemptWasMade(t *testing.T) {
	t.Run("nothing had been attempted, so the refusal is the outcome", func(t *testing.T) {
		store := dispatcherDB(t)
		executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed}}
		store.SetPushExecutor(executor)
		scene := seedPushScene(t, store, `["push"]`)
		id := approveForDispatch(t, store, scene, "req-refused")

		freezeCredential(t, store, scene)
		dispatchPushes(t, store)

		if got := pushState(t, store, id); got != pushFailed+"/repository_credential_frozen" {
			t.Fatalf("a refusal before any attempt settled as %q", got)
		}
		if len(executor.calls) != 0 {
			t.Fatalf("a refused approval reached the executor %d times", len(executor.calls))
		}
	})

	t.Run("an attempt had been made, so the refusal establishes nothing", func(t *testing.T) {
		store := dispatcherDB(t)
		executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed}}
		store.SetPushExecutor(executor)
		scene := seedPushScene(t, store, `["push"]`)
		id := approveForDispatch(t, store, scene, "req-claimed-refused")

		if _, err := store.claimPushDispatch(context.Background(), id); err != nil {
			t.Fatalf("claim the attempt: %v", err)
		}
		lapseLease(t, store, id)
		freezeCredential(t, store, scene)

		dispatchPushes(t, store)

		// The attempt was claimed and never dispatched here, but the claim cannot know that: it knows an
		// attempt was made, and an attempt that was made may have written.
		if got := pushState(t, store, id); got != pushUncertain+"/"+pushResultUnknown {
			t.Fatalf("a refusal after an attempt settled as %q, want an unknown outcome", got)
		}
		if len(executor.calls) != 0 {
			t.Fatalf("a refused approval reached the executor %d times", len(executor.calls))
		}
	})

	t.Run("a failure that carries no code settles nothing", func(t *testing.T) {
		store := dispatcherDB(t)
		executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
		store.SetPushExecutor(executor)
		scene := seedPushScene(t, store, `["push"]`)
		id := approveForDispatch(t, store, scene, "req-unclassified")

		// A database failure or a failure to read the row: this process's trouble, not the approval's.
		if err := store.settleRefusedPushDispatch(context.Background(), id, errors.New("connection reset by peer")); err != nil {
			t.Fatalf("an unclassifiable refusal: %v", err)
		}
		if got := pushState(t, store, id); got != pushDispatching+"/" {
			t.Fatalf("an unclassifiable refusal settled the approval as %q", got)
		}
		// Leaving it unsettled is what keeps the attempt recoverable rather than wedged: the next pass
		// dispatches it as if nothing had happened.
		dispatchPushes(t, store)
		if got := pushState(t, store, id); got != pushSucceeded+"/"+pushApplied {
			t.Fatalf("an approval left unsettled by an unclassifiable refusal settled as %q", got)
		}
	})
}

// TestADeploymentWithoutAnExecutorClaimsNothing is the fail-closed half of the loop being wired only
// when the executor is.
//
// A deployment with no executor records the approval and settles it in the accepting transaction, so
// there is nothing for the pass to find. The row left `dispatching` by an *earlier* configuration is
// here too, because it is the state a deployment change actually produces — an executor installed, an
// approval claimed, and a restart without one — and the pass must leave it alone rather than take it:
// Cloud has no way to ask about that write, so anything it recorded would be invented. The row stays
// recoverable, and a deployment that can read the remote again reconciles it.
func TestADeploymentWithoutAnExecutorClaimsNothing(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedPushScene(t, store, `["push"]`)

	out, _, err := scene.pushRequest(store, "key-none", "req-none", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("approve a push: %v", err)
	}
	if err := store.DispatchPushesOnce(context.Background()); err != nil {
		t.Fatalf("a pass with no executor: %v", err)
	}
	if got := pushState(t, store, out.S("id")); got != pushFailed+"/"+pushExecutorUnavailable {
		t.Fatalf("a deployment with no executor left %q", got)
	}

	// The same approval, as a deployment change leaves it: claimed, never settled, no executor to ask.
	stagedExec(t, store, `UPDATE push_requests SET state='dispatching',result_code=NULL,settled_at=NULL,dispatch_generation=1,dispatch_lease_expires_at=now()-interval '1 hour' WHERE id=$1`, out.S("id"))
	if err := store.DispatchPushesOnce(context.Background()); err != nil {
		t.Fatalf("a pass with no executor over a recovery row: %v", err)
	}
	if got := pushState(t, store, out.S("id")); got != pushDispatching+"/" {
		t.Fatalf("a deployment with no executor settled a row it cannot ask about as %q", got)
	}
}

// TestTheDispatchLeaseOutlastsTheAuthorizationItBounds is the ordering the two bounds must keep, and it
// is asserted rather than assumed because the consequence of breaking it is silent.
//
// A lease shorter than the authorization's lifetime would let a row be claimed again while the first
// attempt's bearer is still valid, so one approval could have two live authorizations and the second
// attempt's compare-and-swap would be guarding a window the first could still write into. Keeping the
// lease strictly longer is what makes "one approval, one live authorization at a time" true.
func TestTheDispatchLeaseOutlastsTheAuthorizationItBounds(t *testing.T) {
	if PushDispatchLease <= PushDispatchLifetime {
		t.Fatalf("the dispatch lease %s must outlast the authorization lifetime %s", PushDispatchLease, PushDispatchLifetime)
	}
}
