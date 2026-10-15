package core

import (
	"context"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneauth"
)

// The attempts one approval's write may have are bounded two ways, and each bound answers a different
// failure.
//
// The lease bounds one attempt. An attempt whose process died leaves no transaction to roll back, so
// without a deadline the row would hold `dispatching` with nothing working on it — which is the state this
// worker exists to end, and the one Cloud cannot distinguish from a write that is still in flight. The
// attempt budget bounds the reconciliation as a whole: a write whose outcome cannot be established has to
// settle with the honest answer rather than be re-attempted until somebody notices.
//
// There is deliberately no backoff and no third bound. Every claim either dispatches an attempt or settles
// the approval, so a row a pass takes always changes; see `DispatchPushesOnce`.
const (
	// PushDispatchLease bounds one attempt, and it is exported because it is also the ceiling on the
	// authority that attempt mints: it is strictly longer than `PushDispatchLifetime`, so an authorization
	// has always expired before the row it belongs to can be claimed again, and one approval never has two
	// live authorizations. `internal/config` checks a configured push lifetime against
	// `PushDispatchLifetime` for the same reason the two are stated together here — the bounds must not be
	// able to drift apart in a deployment file.
	//
	// It is not the write fence, and nothing should read it as one: an attempt that outlives its lease is
	// safe because the executor's compare-and-swap is what stops a second write, and because the settlement
	// below is fenced on the attempt's generation.
	PushDispatchLease = PushDispatchLifetime + time.Minute
	// pushDispatchAttempts bounds one approval's attempts. Three is what the reconciliation needs: one
	// attempt that may not come back, and two more to establish what became of it. A bound of one would make
	// every interrupted dispatch terminal at `push_result_unknown`, which is honest but gives up on evidence
	// the remote can still supply.
	pushDispatchAttempts = 3
	// pushDispatchBatch bounds one pass. It is a batch rather than a drain so that a pass cannot hold a
	// connection for an unbounded time, and because a claimed row leaves the candidate set either way, the
	// next pass sees whatever is left rather than the same rows again.
	pushDispatchBatch = 8
	// pushNoAttempt is the dispatch generation an approval holds until an attempt claims it. The accepting
	// transaction settles a deployment with no executor at this generation, and the first claim moves the
	// row to one past it, so the value is both a row's initial state and the fence the accepting settlement
	// writes under.
	pushNoAttempt = 0
)

// PushDispatch is one claimed attempt at carrying out an approved push.
//
// It is the push's counterpart of `MirrorPreparation`, and it carries the same two kinds of thing: what the
// executor is asked to do, and the generation its answer has to be settled under. Keeping the generation
// beside the execution is what lets the claim, the dispatch and the settlement be three separate
// transactions — the row is held only for the first and last of them, never across the write.
type PushDispatch struct {
	// Execution is the write this attempt is authorized to make. Its AuthorizationID is minted at claim
	// time, so it is fixed before the executor is called and names this attempt and no other.
	Execution PushExecution
	// Generation is the attempt fence: the settlement is recorded only while the row still holds this value.
	Generation int64
}

// pushDispatchCandidates lists the approvals that are owed an attempt now.
//
// "Owed an attempt" is one state: still `dispatching`, with no live lease. A freshly accepted approval has
// no lease at all and is therefore due immediately, which is what makes the accepting response truthful —
// the write is owed from the moment the approval commits, and the pass that follows carries it out. A row
// whose attempt is still inside its lease is not due, so an attempt in flight is never replaced.
//
// A settled approval is never a candidate, which is what makes "a settled approval is never carried out
// again" structural rather than a rule the dispatcher has to remember. Nothing here filters by the attempt
// budget either: a row whose attempts are spent is still owed an outcome, so it stays a candidate until a
// pass settles it. The `updated_at` ordering is the oldest-first walk the partial index covers.
func (s *Store) pushDispatchCandidates(ctx context.Context, limit int) ([]string, error) {
	var ids []string
	_, err := s.transact(ctx, func(t *transaction) Object {
		rows := t.list(`SELECT id FROM push_requests
			WHERE state='dispatching'
			  AND (dispatch_lease_expires_at IS NULL OR dispatch_lease_expires_at <= now())
			ORDER BY updated_at LIMIT $1`, limit)
		for _, row := range rows {
			ids = append(ids, row.S("id"))
		}
		return Object{}
	})
	return ids, err
}

// claimPushDispatch admits one attempt for an approval and returns what to dispatch.
//
// A nil dispatch with a nil error means there is nothing to do: the approval has moved on, an attempt still
// holds it, or its attempts are spent and it was settled here. That settlement happens inside this
// transaction because it is Cloud's own state and the row is already held — no external work follows from
// it, so there is nothing to release the row for.
//
// A non-nil error means the admission refused, and the caller classifies it. The refusal is raised by the
// guard admission called (`requirePushAccess`), in the same transaction that holds the row, so the facts
// the attempt rests on — the approval is still unspent, the Project still binds the credential the approval
// names at the version it names, and the reference still permits a write — are established against one
// snapshot immediately before the dispatch. That is the execution authority's D6 second revocation
// checkpoint. It is here rather than in a pass of its own because a check made in a different transaction
// would be a check something else could invalidate before the write it guards.
//
// # What this cannot close, stated plainly
//
// The window between this re-check and the executor's redemption of the credential is irreducible. The
// Store decides on its own, at redemption, that the token is genuine, unspent, unexpired, of a version
// it can still read and not revoked — but it cannot see whether this approval is still in force or
// whether the Project still binds that credential. A revocation that lands inside the window therefore
// takes effect at the next checkpoint or at the authorization's expiry, whichever comes first; it is not
// instantaneous, and nothing here should be described as making it so. The authorization's lifetime is
// bounded (`PushDispatchLifetime`) precisely to bound that window, and the lease above is bounded to be
// strictly longer than it so that a re-attempt can never mint a second live authorization.
func (s *Store) claimPushDispatch(ctx context.Context, id string) (*PushDispatch, error) {
	var claimed *PushDispatch
	_, err := s.transact(ctx, func(t *transaction) Object {
		// FOR UPDATE so two passes — this process's and another replica's — cannot claim one approval
		// concurrently, and so the re-check below cannot be overtaken between itself and the attempt it
		// admits. The row is held for the database work in this transaction and never across the dispatch.
		row := t.one(`SELECT r.*, (r.dispatch_lease_expires_at IS NULL OR r.dispatch_lease_expires_at <= now()) AS dispatch_due
			FROM push_requests r WHERE r.id=$1 FOR UPDATE`, id)
		if row == nil || row.S("state") != pushDispatching {
			return Object{} // Settled, or never committed. Either way there is no write left to carry out.
		}
		// The lease is re-checked under the lock rather than trusted from the candidate scan: that scan ran
		// before this transaction opened, and another replica may have claimed the row in between. A live
		// lease belongs to an attempt that may still be running, and starting a second one beside it would
		// spend a second credential redemption on a write the first may be about to make.
		if !row.B("dispatchDue") {
			return Object{}
		}
		spent := row.N("dispatchGeneration")
		if spent >= pushDispatchAttempts {
			// The attempts are spent and the row is due, which means the last of them never came back. The
			// approval settles with the only answer Cloud has: the outcome could not be established. Never
			// `failed` — an attempt that reached the executor may have written — and never re-attempted,
			// because the bound is what stops an unestablishable write from being tried forever.
			settlePushRequest(t, id, spent, pushUncertain, pushResultUnknown)
			return Object{}
		}

		// The Project is read with the visibility the accepting path used (`projectInSpace` filters a
		// soft-deleted row the same way), so a Project removed between the two checkpoints is refused rather
		// than re-authorized from a row that is no longer part of the live graph. A Project that cannot be
		// read is not a Project whose credential may be used: the guard's own codes say why a reference is
		// unusable, and a missing Project is refused with the guard's "no reference to use" code, because
		// from the write's point of view that is what it amounts to.
		p := t.one("SELECT * FROM projects WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL", row.S("projectId"), row.S("tenantId"))
		require(p != nil, 409, "repository_credential_unavailable")
		credentialID, credentialVersion, upstream := requirePushAccess(t, p)
		// The approval has to describe the same write the guard has just re-established: the credential the
		// Project binds now has to be the one the approval was made against, at the version it was made
		// against, for the same repository. A rotation between the two would otherwise let one approval be
		// spent on material it was not approved for, which is the silent widening the version pin exists to
		// prevent; a rebound Project would spend it on a different repository entirely.
		require(credentialID == row.S("credentialRefId") && credentialVersion == row.N("credentialRefVersion") &&
			upstream == row.S("repositoryUrl"), 409, "repository_credential_binding_conflict")
		// The credential's owner is read through the join rather than stored on the approval row: the
		// binding's owner is the credential's, and a second copy is a value that could disagree with the one
		// the Store checks at redemption.
		credential := t.one("SELECT owner_user_id FROM credential_refs WHERE id=$1", credentialID)
		require(credential != nil, 409, "repository_credential_unavailable")

		// The attempt's authorization is fixed here, before the Store is asked to sign anything, because
		// the Store records consumption under it and a value derived from the executor's reply would not be
		// fixed before the call.
		//
		// The first attempt carries the authorization the approval was *recorded* under, and a re-attempt
		// mints a new one. That is D7 read exactly: the plan persisted at acceptance includes its
		// authorization identity (D8), so the identifier the approving client was handed is the one this
		// attempt redeems, and the audit reads as one decision rather than two. Only a retry re-issues,
		// because a re-attempt that reconsumed the first attempt's authorization would be the replay D7
		// forbids outright — the first attempt may have reached the Store and been consumed with its answer
		// lost, and one authorization must never authorize two writes.
		generation := row.N("dispatchGeneration") + 1
		authorization := row.S("authorizationId")
		if row.N("dispatchGeneration") > pushNoAttempt {
			authorization = newID()
		}
		t.exec(`UPDATE push_requests SET authorization_id=$2, dispatch_generation=$3,
				dispatch_lease_expires_at=now()+make_interval(secs => $4), updated_at=now()
			WHERE id=$1`, id, authorization, generation, PushDispatchLease.Seconds())
		claimed = &PushDispatch{
			Generation: generation,
			Execution: PushExecution{
				AuthorizationID:   authorization,
				TenantID:          row.S("tenantId"),
				OwnerUserID:       credential.S("ownerUserId"),
				ProjectID:         row.S("projectId"),
				WorkspaceID:       row.S("workspaceId"),
				OperationID:       row.S("id"),
				CredentialID:      credentialID,
				CredentialVersion: credentialVersion,
				Repository:        row.S("repositoryUrl"),
				Ref:               row.S("pinnedRef"),
				OldCommit:         row.S("oldCommit"),
				NewCommit:         row.S("newCommit"),
				// The requirement, not the reference's whole declared set: an authorization carries exactly
				// what its operation permits, and `cloneauth.Issue` refuses any other set.
				Capabilities: []string{string(cloneauth.CapabilityPush)},
			},
		}
		return Object{}
	})
	return claimed, err
}

// settlePushDispatch records the answer to one attempt, accepting it only while that attempt is still
// current.
//
// The fence is the generation the attempt was claimed under, and it is what makes a late answer harmless:
// an attempt that outlived its lease and was replaced settles nothing, so a slow or replayed reply cannot
// overwrite the result of the attempt that superseded it. The caller learns which of the two happened from
// the returned bool.
//
// The classification is the one a write must never get wrong, and it is unchanged from the settlement that
// ran inside the request: a coded refusal is the executor concluding that this attempt wrote nothing, an
// uncoded error is an attempt that may have written and is recorded `uncertain`, and a verdict is recorded
// as what it says. Nothing here infers a failure from silence.
//
// The error is returned rather than dropped so the pass can report it and stop: a settlement that could not
// be written leaves the approval `dispatching`, which is the truthful record — an approval whose outcome
// Cloud has not yet established — and the next pass, once the lease lapses, reconciles it against the
// remote.
//
//nolint:gocritic // The claim is passed by value on purpose: the generation this settles is the one the attempt was claimed under, so it must not be reachable for writing from here.
func (s *Store) settlePushDispatch(ctx context.Context, d PushDispatch, outcome PushOutcome, cause error) (bool, error) {
	state, code, observed := pushSettlement(outcome, cause)
	applied := false
	_, err := s.transact(ctx, func(t *transaction) Object {
		applied = settlePushObservedRequest(t, d.Execution.OperationID, d.Generation, state, code, observed)
		return Object{}
	})
	return applied, err
}

// pushSettlement turns one attempt's return into the state, the reason code and the observation the audit
// records.
//
// It is the only place the two answers an attempt can give are told apart, and the distinction is the one
// the whole write path turns on: an error carrying a reason code is the executor deciding that this attempt
// wrote nothing, and everything else — a dial failure, a reset connection, an unreadable answer, a verdict
// the executor stated — is either a fact about the remote or an admission that there is none. An uncoded
// error is never a failure, because the write may have landed and recording it as refused is the one
// direction a write must not be settled in.
func pushSettlement(outcome PushOutcome, cause error) (state, code, observed string) {
	if cause == nil {
		settlePushFor(outcome.Verdict, &state, &code)
		return state, code, outcome.ObservedCommit
	}
	if refusal, coded := pushRefusalCode(cause); coded {
		return pushFailed, refusal, ""
	}
	return pushUncertain, pushResultUnknown, ""
}

// settleRefusedPushDispatch records an admission that refused before any attempt was dispatched.
//
// The rule is one question: had an attempt already been claimed? If not, the approval settles `failed`
// under the guard's own code, and that is a fact rather than a summary — nothing was sent anywhere, so
// nothing was written, and the code names what an operator has to fix. If an attempt had been claimed, the
// guard's refusal says nothing about the remote: that attempt may have written and never reported, so
// settling the refusal as a failure would record a possible write as a refusal. It settles `uncertain`
// under the result-unknown code, which is what Cloud actually knows.
//
// This is deliberately not the credentialed-clone classification (`terminalCredentialRefusals`), and the
// difference is the question being asked. There the question is whether a refusal can change on a retry,
// because an Operation should keep waiting for a credential that may be unfrozen. Here it is what an attempt
// may already have done, because a push approval is one person's decision that this write happen now: an
// authority that is gone by the time the write would run is not re-established days later, and the person
// approves again if they still want it.
//
// A cause with no code settles nothing. That is the shape the worker cannot classify — the approval could
// not be read at all, which is this process's or the database's trouble rather than the approval's — and
// leaving the row `dispatching` keeps the next pass able to try again, where inventing an outcome would
// record a conclusion nobody reached.
func (s *Store) settleRefusedPushDispatch(ctx context.Context, id string, cause error) error {
	code, coded := pushRefusalCode(cause)
	if !coded {
		return nil
	}
	_, err := s.transact(ctx, func(t *transaction) Object {
		row := t.one("SELECT * FROM push_requests WHERE id=$1 FOR UPDATE", id)
		if row == nil || row.S("state") != pushDispatching {
			return Object{}
		}
		generation := row.N("dispatchGeneration")
		if generation == pushNoAttempt {
			settlePushRequest(t, id, generation, pushFailed, code)
			return Object{}
		}
		settlePushRequest(t, id, generation, pushUncertain, pushResultUnknown)
		return Object{}
	})
	return err
}

// DispatchPushesOnce runs one pass: claim a batch of due approvals, dispatch each attempt, settle what came
// back.
//
// It runs on the process's context rather than a request's, and that is the whole reason it exists. The
// dispatch used to run inside the request that recorded the approval, so a client that hung up canceled the
// write and the settlement with it, leaving a row `dispatching` that nothing read. The write belongs to the
// approval rather than to the request that asked for it, and the approval is durable before this pass ever
// looks at it — so the caller's answer is the approval, and this is what carries it out.
//
// Claim, dispatch, settle: the row is held for the first and last of those and never across the executor
// call, which is the execution contract's rule and also what keeps an attempt's lease from running while it
// waits behind other work. A pass that dies mid-dispatch loses nothing but its lease, which the next pass
// finds lapsed and reconciles against the remote with a fresh authorization.
//
// Returning the error rather than logging it is what lets the caller's loop decide, the same shape the other
// periodic passes in this package use.
func (s *Store) DispatchPushesOnce(ctx context.Context) error {
	if s.pushExecutor == nil {
		// No executor installed: no approval is claimed, and the accepting path settles each one as the
		// deployment-level refusal it is. A deployment that removes the executor after an approval was
		// already claimed leaves that row for a deployment that can read the remote again — Cloud cannot
		// establish anything about a write it has no way to ask about.
		return nil
	}
	ids, err := s.pushDispatchCandidates(ctx, pushDispatchBatch)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		dispatch, err := s.claimPushDispatch(ctx, id)
		if err != nil {
			if err := s.settleRefusedPushDispatch(ctx, id, err); err != nil {
				return err
			}
			continue
		}
		if dispatch == nil {
			continue
		}
		// Checked between the claim and the dispatch rather than only at the top of the loop: an attempt
		// claimed during a shutdown would otherwise be minted and then thrown away, and the executor's
		// failure to be reached would be recorded as an outcome nobody established. The row keeps the
		// attempt and lapses back into the candidate set, where the next process dispatches it.
		if err := ctx.Err(); err != nil {
			return err
		}
		outcome, cause := s.pushExecutor.Execute(ctx, dispatch.Execution)
		if _, err := s.settlePushDispatch(ctx, *dispatch, outcome, cause); err != nil {
			return err
		}
	}
	return nil
}
