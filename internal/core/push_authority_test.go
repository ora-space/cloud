package core

import (
	"context"
	"testing"
)

// This file covers the second of the push authority's two revocation checkpoints: the re-check Cloud
// makes against its own live state immediately before the write is dispatched.
//
// The checkpoint exists because the first one — the decision made when the approval was accepted — is
// only true as of that moment. Between accepting and dispatching the credential can be revoked or
// rotated, the Project can be rebound, or the approval can be settled by another path; every one of
// those makes the approval no longer describe something Cloud may do, and only Cloud can see it. The
// cases below therefore each change exactly one piece of live state in that window and assert two
// things: which stable code the approval settles with, and that the executor was never asked.
//
// The second assertion is the one that makes the first mean anything. A re-check that refused *after*
// handing the write to the executor would leave the same audit row while having already spent the
// authorization, so "no call" is what distinguishes a checkpoint from a report.

// approveWithoutDispatching records one approval through the real accepting function and returns the
// identifier the dispatch would have used, without dispatching.
//
// It calls `pushRequestPublic` inside a transaction — the exact call `Public`'s accepting arm makes —
// and simply does not make the call that follows it. That is what opens the window these cases are
// about: the approval is committed and `dispatching`, exactly as it would be at the instant before
// `dispatchPush` runs, and nothing has been sent anywhere.
func approveWithoutDispatching(t *testing.T, store *Store, scene pushScene, requestID string) string {
	t.Helper()
	var pending string
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		_, pending = pushRequestPublic(tx, &PublicRequest{
			Method: "POST", Path: "/api/v1/tenants/" + scene.tenant + "/workspaces/" + scene.workspace + "/pushes",
			TenantID: scene.tenant, WorkspaceID: scene.workspace,
			Body: Object{"requestId": requestID, "ref": pushRef, "oldCommit": pushOldCommit, "newCommit": pushNewCommit},
		}, scene.user)
		return Object{}
	}); err != nil {
		t.Fatalf("record the approval: %v", err)
	}
	if pending == "" {
		t.Fatal("the accepting function returned no dispatch, so the window cannot be opened")
	}
	return pending
}

// TestAPushIsNotDispatchedWhenItsAuthorityIsGoneByTheTimeTheWriteWouldRun is the negative matrix of the
// pre-write re-check. Each case leaves the approval valid at acceptance and invalid at dispatch, and
// each must end with the guard's own code and with the executor never called.
//
// The codes are asserted individually rather than as "some refusal", because they are what an operator
// acts on: a frozen credential is a person to re-authorize, a version that moved is a rotation to
// re-approve against, and a rebound Project means the approval is about a different repository than the
// one it names. Collapsing them would send every one of those to the same place.
func TestAPushIsNotDispatchedWhenItsAuthorityIsGoneByTheTimeTheWriteWouldRun(t *testing.T) {
	cases := []struct {
		name string
		// change mutates live state after the approval committed and before the dispatch is attempted.
		change   func(t *testing.T, store *Store, scene pushScene)
		wantCode string
	}{
		{
			name: "the credential was revoked",
			change: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, "UPDATE credential_refs SET availability='invalid',unavailable_reason='revoked_by_owner' WHERE id=$1", scene.credential)
			},
			wantCode: "repository_credential_frozen",
		},
		{
			// The rotation case is the reason the version is pinned at approval time rather than resolved
			// at dispatch time. The credential is still available and still bound to the same repository;
			// only its material has moved. Without the version comparison this approval would be spent on
			// material it was never approved against, which is exactly the silent widening the pin exists
			// to prevent.
			name: "the credential rotated",
			change: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, "UPDATE credential_refs SET version=version+1 WHERE id=$1", scene.credential)
			},
			wantCode: "repository_credential_binding_conflict",
		},
		{
			name: "the project was rebound to another credential",
			change: func(t *testing.T, store *Store, scene pushScene) {
				other := newID()
				stagedExec(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,availability,repository_scope,capabilities)
					VALUES($1,$2,$3,'git',$4,'rebound','personal','user_stated','available',$5,'["push"]'::jsonb)`,
					other, scene.tenant, scene.user, "credential-ref/"+other, admittedScope)
				stagedExec(t, store, "UPDATE projects SET credential_ref_id=$2 WHERE id=$1", scene.project, other)
			},
			wantCode: "repository_credential_binding_conflict",
		},
		{
			name: "the project's credential was removed",
			change: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, "UPDATE projects SET credential_ref_id=NULL WHERE id=$1", scene.project)
			},
			wantCode: "push_credential_required",
		},
		{
			// A Project that cannot be read is not a Project whose credential may be used. From the
			// write's point of view that amounts to having no usable reference at all, which is the code
			// the guard already has for it.
			name: "the project is gone",
			change: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, "UPDATE projects SET deleted_at=now() WHERE id=$1", scene.project)
			},
			wantCode: "repository_credential_unavailable",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := dispatcherDB(t)
			executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
			store.SetPushExecutor(executor)
			scene := seedPushScene(t, store, `["push"]`)

			id := approveWithoutDispatching(t, store, scene, "req-recheck")
			if got := pushState(t, store, id); got != pushDispatching+"/" {
				t.Fatalf("the approval must be dispatching before the window is used, got %q", got)
			}

			testCase.change(t, store, scene)
			store.dispatchPush(context.Background(), id)

			want := pushFailed + "/" + testCase.wantCode
			if got := pushState(t, store, id); got != want {
				t.Fatalf("the re-check settled the approval as %q, want %q", got, want)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("an approval whose authority was gone reached the executor %d times", len(executor.calls))
			}
		})
	}
}

// TestAPushIsStillDispatchedWhenNothingAboutItsAuthorityChanged is the control for the matrix above,
// and it is what keeps those cases from being satisfiable by a re-check that refuses everything.
//
// It is also the evidence that the re-check runs *before* the executor rather than beside it: the same
// approval that the cases above leave undispatched is here dispatched once, with the executor seeing
// the plan Cloud approved.
func TestAPushIsStillDispatchedWhenNothingAboutItsAuthorityChanged(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)

	id := approveWithoutDispatching(t, store, scene, "req-unchanged")
	store.dispatchPush(context.Background(), id)

	if got := pushState(t, store, id); got != pushSucceeded+"/"+pushApplied {
		t.Fatalf("an unchanged approval settled as %q, want a success", got)
	}
	if len(executor.calls) != 1 {
		t.Fatalf("the executor was called %d times, want 1", len(executor.calls))
	}
	if call := executor.calls[0]; call.OperationID != id || call.CredentialID != scene.credential || call.CredentialVersion != 1 {
		t.Fatalf("the executor was handed something other than the approval: %+v", call)
	}
}

// TestASettledPushIsNeverDispatchedAgain is the second checkpoint's other half: an approval that is no
// longer `dispatching` is not carried out, whatever a later caller asks for.
//
// This is the case a retried or duplicated dispatch meets. The row has already settled — here by the
// deployment-level answer — and the re-check is what stops the write from happening anyway, which
// matters because the authorization identifier in the row is single-use: a second dispatch would either
// be refused by the Credential Service, having burned an audit trail, or, in a deployment where the
// first settlement happened *before* a signature was ever minted, would find a spendable grant behind a
// row that already reads as concluded.
func TestASettledPushIsNeverDispatchedAgain(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)

	id := approveWithoutDispatching(t, store, scene, "req-settled")
	// Settle it the way a concurrent or earlier dispatch would have, without dispatching.
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		settlePushRequest(tx, id, pushFailed, pushNotApplied)
		return Object{}
	}); err != nil {
		t.Fatalf("settle the approval: %v", err)
	}

	store.dispatchPush(context.Background(), id)

	if got := pushState(t, store, id); got != pushFailed+"/"+pushNotApplied {
		t.Fatalf("a redispatch of a settled approval rewrote it to %q", got)
	}
	if len(executor.calls) != 0 {
		t.Fatalf("a settled approval reached the executor %d times", len(executor.calls))
	}
}
