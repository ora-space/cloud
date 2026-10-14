package core

// Remaining revocable paths and the reachable P2 hazards, on real PostgreSQL.
//
// The revocation half pins the withdrawal's history retention where the lifecycle file left off: a
// repeat withdrawal must not rewrite the record it already wrote, and neither a withdrawal nor the
// freeze sweep may touch the executions, migration records and idempotency records a credential has
// already left behind. The "deleted credential" path is deliberately absent — `credential_refs.deleted_at`
// has no product writer (D5), so it stays uncovered rather than being faked with a direct UPDATE.
//
// The P2 half tests only what the shipped internal seams can reach. Where a branch has no seam it is
// reported, not worked around.

import (
	"context"
	"sync"
	"testing"
)

// credentialRevocationWitness is a credential row's whole withdrawn state as one comparable string,
// including the audit timestamp. `revoked_at` is the field a naive "revoke again" implementation would
// move; comparing it byte-for-byte is what makes the idempotency assertion non-vacuous.
func credentialRevocationWitness(t *testing.T, store *Store, id string) string {
	t.Helper()
	return textOf(t, store, `SELECT availability||'/'||COALESCE(unavailable_reason,'')||'/'||version::text||'/'||COALESCE(revoked_at::text,'never')||'/'||secret_ref||'/'||COALESCE(deleted_at::text,'live') FROM credential_refs WHERE id=$1`, id)
}

// TestRevokingACredentialAgainDoesNotRewriteItsWithdrawalRecord pins the withdrawal's idempotency on
// the paths the first revocation left: a second withdrawal under a fresh key, and a replay under the
// original key. Revocation is expressed through `availability`, and `revoked_at` is the audit timestamp
// beside that state (D5) — reaching the same withdrawn state twice must not move the timestamp, bump
// the version a second time, or delete the row.
//
// Non-vacuity: the second withdrawal is sent with the *stale* version 1 while the row is already at
// version 2. If revocation re-entered its write path the version precondition would fire and the call
// would fail; instead the row is already `invalid`, the write is skipped by design, and the call
// succeeds — which is exactly the "no conflict on a state the caller could not have observed" rule.
// A rewrite would also change the `revoked_at` text compared here.
func TestRevokingACredentialAgainDoesNotRewriteItsWithdrawalRecord(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	claims := callerIdentity(t, store, scene.user)

	first, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "revoke-first", 1), "")
	if err != nil {
		t.Fatalf("first withdrawal: %v", err)
	}
	if first.Status != 200 || first.Resource.S("availability") != "invalid" {
		t.Fatalf("a withdrawal is expressed through availability: %+v", first.Resource)
	}
	withdrawn := credentialRevocationWitness(t, store, scene.credential)

	// A second withdrawal under a new key, naming the version the caller last saw (now stale).
	second, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "revoke-second", 1), "")
	if err != nil {
		t.Fatalf("a repeat withdrawal must be idempotent, got %v", err)
	}
	if second.Status != 200 || second.Resource.S("availability") != "invalid" {
		t.Fatalf("a repeat withdrawal must still report the withdrawn credential: %+v", second.Resource)
	}
	if after := credentialRevocationWitness(t, store, scene.credential); after != withdrawn {
		t.Fatalf("a repeat withdrawal rewrote the record it already wrote: %s -> %s", withdrawn, after)
	}

	// A replay under the original key must likewise change nothing.
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "revoke-first", 1), ""); err != nil {
		t.Fatalf("replaying the withdrawal under its original key must succeed: %v", err)
	}
	if after := credentialRevocationWitness(t, store, scene.credential); after != withdrawn {
		t.Fatalf("replaying the withdrawal rewrote its record: %s -> %s", withdrawn, after)
	}

	// The row is withdrawn, not deleted: the reference that locates the material survives, and the
	// store material is destroyed (idempotently) rather than the row being erased.
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND deleted_at IS NULL AND secret_ref=$2`, scene.credential, "credential-ref/1"); n != 1 {
		t.Fatal("a repeat withdrawal erased or rewrote the credential's retained history")
	}
	if _, _, revokes := secrets.handoffs(); revokes == 0 {
		t.Fatal("no withdrawal ever reached the store")
	}
}

// TestRevocationAndTheFreezeSweepLeaveRecordedHistoryIntact pins the retention promise from the other
// side: the durable records a credential has already produced outlive both its withdrawal and the
// recovery scan. The revoked credential drives the scan directly (its owner is active, so the scan's
// predicate is false — this is the "the sweep erases nothing" case) after the withdrawal, and every
// recorded fact is compared before and after.
//
// The migration records are included on purpose: they are applied state whose checksums verify on
// startup, and a sweep or withdrawal that wrote to `schema_migrations` would be an attempt to rewrite
// history rather than to withdraw a credential.
func TestRevocationAndTheFreezeSweepLeaveRecordedHistoryIntact(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	claims := callerIdentity(t, store, scene.user)

	// A completed credentialed execution bound to the credential, recorded the way an accepted clone
	// leaves it.
	execution := "exec-retained-" + scene.workspace[:8]
	seedRecordedCredentialedExecution(t, store, scene, execution)

	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "retain-revoke", 1), ""); err != nil {
		t.Fatalf("withdraw: %v", err)
	}

	executionBefore, stampBefore := cloneExecutionWitness(t, store, execution)
	migrationsBefore := textOf(t, store, `SELECT string_agg(version||':'||checksum, ',' ORDER BY version) FROM schema_migrations`)
	idempotencyBefore := textOf(t, store, `SELECT request_hash||'/'||status::text FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, scene.tenant, scene.user, "retain-revoke")
	credentialsBefore := scalarOf(t, store, `SELECT count(*) FROM credential_refs`)
	executionsBefore := scalarOf(t, store, `SELECT count(*) FROM clone_executions`)

	// The scan runs twice: once against the already-withdrawn credential, and once more to prove the
	// scan itself is idempotent over this state. Neither may touch any recorded fact.
	runFreezeSweep(t, store)
	runFreezeSweep(t, store)

	if after, stamp := cloneExecutionWitness(t, store, execution); string(after) != string(executionBefore) || stamp != stampBefore {
		t.Fatalf("a withdrawal or the sweep rewrote the recorded execution: %s/%s -> %s/%s", executionBefore, stampBefore, after, stamp)
	}
	if after := textOf(t, store, `SELECT string_agg(version||':'||checksum, ',' ORDER BY version) FROM schema_migrations`); after != migrationsBefore {
		t.Fatal("a withdrawal or the sweep rewrote the applied migration records")
	}
	if after := textOf(t, store, `SELECT request_hash||'/'||status::text FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, scene.tenant, scene.user, "retain-revoke"); after != idempotencyBefore {
		t.Fatal("a withdrawal or the sweep rewrote the idempotency record of the withdrawal")
	}
	if got := scalarOf(t, store, `SELECT count(*) FROM credential_refs`); got != credentialsBefore {
		t.Fatalf("the sweep erased credential rows: %d -> %d", credentialsBefore, got)
	}
	if got := scalarOf(t, store, `SELECT count(*) FROM clone_executions`); got != executionsBefore {
		t.Fatalf("the sweep erased execution rows: %d -> %d", executionsBefore, got)
	}
	// The withdrawn row itself survives with its reference intact and its version un-bumped by the
	// sweep: `invalid` is not `available`, so the scan's predicate must skip it.
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='invalid' AND unavailable_reason='revoked_by_owner' AND version=2 AND secret_ref='credential-ref/1' AND deleted_at IS NULL`, scene.credential); n != 1 {
		t.Fatal("the sweep rewrote or erased the withdrawn credential")
	}
}

// TestALateCloneResultAfterTheOperationSucceededCannotRewriteIt covers the late-result hazard on the
// Workspace path: once the operation has advanced past `clone` and succeeded, a clone result that
// arrives afterwards is a historical fact about its execution and must not rewrite the recorded result
// or move the Workspace baseline that was already taken from it. The execution registry's contract is
// that a result is written once — a repeat of the same fact is a no-op, a different one conflicts —
// and this scene asks it after the operation that owned the execution has finished.
//
// Non-vacuity: the late result carries a valid, *different* commit, so a registry that wrote the
// arrival unconditionally would change the recorded commit and, through `advanceClone`'s later reading
// of the latest execution, the baseline could move. The assertion is on both the recorded result and
// the baseline, so either rewrite would fail it.
func TestALateCloneResultAfterTheOperationSucceededCannotRewriteIt(t *testing.T) {
	store := dispatcherDB(t)
	scene := anonymousWorkspaceCloneScene(t, store)
	execution := "exec-late-" + scene.workspace[:8]
	if _, err := scene.dispatch(store, execution); err != nil {
		t.Fatalf("register the clone execution: %v", err)
	}
	first := Object{"outcome": "clone_ready", "node": Object{"nodeId": scene.node}, "commit": baseCommit}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": first}); err != nil {
		t.Fatalf("take over the clone result: %v", err)
	}

	// Advance the operation past clone; with no plugins it settles succeeded and the Workspace's
	// baseline is taken from the clone that already ran.
	version := scalarOf(t, store, "SELECT version FROM operations WHERE id=$1", scene.operation)
	if _, err := driveControl(t, store, "advance", Object{"operationId": scene.operation, "version": version}); err != nil {
		t.Fatalf("advance past the clone: %v", err)
	}
	if state := textOf(t, store, "SELECT state FROM operations WHERE id=$1", scene.operation); state != "succeeded" {
		t.Fatalf("the operation must settle succeeded before the late result is judged, got %q", state)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2", scene.workspace, baseCommit); n != 1 {
		t.Fatal("the accepted clone must have set the Workspace baseline")
	}
	recorded, stamp := cloneExecutionWitness(t, store, execution)

	// A different fact arriving after the operation finished must conflict, not overwrite.
	late := Object{"outcome": "clone_ready", "node": Object{"nodeId": scene.node}, "commit": "fedcba9876543210fedcba9876543210fedcba98"}
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": late}); err == nil {
		t.Fatal("a differing late clone result must be refused")
	} else if code := faultCode(t, err); code != "result_conflict" {
		t.Fatalf("a differing late clone result: got %q, want result_conflict", code)
	}
	if after, stamp2 := cloneExecutionWitness(t, store, execution); string(after) != string(recorded) || stamp2 != stamp {
		t.Fatalf("a late clone result rewrote the recorded one after the operation moved on: %s/%s -> %s/%s", recorded, stamp, after, stamp2)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2", scene.workspace, baseCommit); n != 1 {
		t.Fatal("a late clone result moved the Workspace baseline the finished operation had already taken")
	}
	if state := textOf(t, store, "SELECT state FROM operations WHERE id=$1", scene.operation); state != "succeeded" {
		t.Fatalf("a late clone result disturbed the finished operation, now %q", state)
	}

	// A replay of the *same* late fact is an idempotent no-op, so a lost-reply retry cannot be read as
	// a second outcome either.
	if _, err := driveControl(t, store, "clone_takeover", Object{"operationId": scene.operation, "executionId": execution, "sequence": 0, "event": "e30=", "result": first}); err != nil {
		t.Fatalf("repeating the recorded result must be a no-op, got %v", err)
	}
	if after, stamp2 := cloneExecutionWitness(t, store, execution); string(after) != string(recorded) || stamp2 != stamp {
		t.Fatal("repeating the recorded result wrote something")
	}
}

// pointerRaceSecrets simulates the other writer in a rotate's second-transaction failure: the store has
// already replaced the material when the credential's metadata is changed out from under the pointer
// advance. It is a test double on the `CredentialSecretWriter` seam the product already consumes, so no
// production seam is added — the concurrent writer it stands in for is another process, not a fork of
// this code.
type pointerRaceSecrets struct {
	store *Store
	mu    sync.Mutex
	did   bool
	token string
}

func (s *pointerRaceSecrets) Write(context.Context, string, string) (string, error) {
	return "", nil
}

func (s *pointerRaceSecrets) Rotate(_ context.Context, credentialID, token string) error {
	s.mu.Lock()
	s.did, s.token = true, token
	s.mu.Unlock()
	// The material is now the new token. Before the metadata pointer can advance, a concurrent writer
	// changes the row: this is the failure the second transaction's own precondition must catch.
	_, err := s.store.Pool.Exec(`UPDATE credential_refs SET version=version+1,name='concurrent-writer' WHERE id=$1`, credentialID)
	return err
}

func (s *pointerRaceSecrets) Revoke(context.Context, string) error { return nil }

func (s *pointerRaceSecrets) rotated() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.did, s.token
}

// TestARotateWhoseMetadataAdvanceFailsLeavesTheStoreRotatedAndThePointerBehind pins the D4 ordering of
// rotation on its second transaction: the store's material is written first, the metadata pointer
// advances second, and when the second transaction fails the credential is left with material newer
// than its recorded version rather than with a pointer to material that was never written.
//
// The failure is staged on the seam between the two transactions (`Secrets.Rotate`), where any real
// concurrent writer would act: a second writer advances the row's version while the rotate is in
// flight, so the second transaction's `version` precondition fails and the whole rotate is reported as
// a conflict. The version column is the witness: had the second transaction's own `version=version+1`
// run, the row would be at 3; the observed 2 is only the concurrent writer's bump, so the pointer
// advance provably did not apply. The concurrent writer's `name` also survives, proving the failed
// second transaction did not write the row at all.
func TestARotateWhoseMetadataAdvanceFailsLeavesTheStoreRotatedAndThePointerBehind(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &pointerRaceSecrets{store: store}
	store.Secrets = secrets
	tenant, user, claims := bootstrapCaller(t, store, "rotate-race")
	credential := stageOwnedCredential(t, store, tenant, user)

	_, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("PUT", tenant, credential, claims, "", 1), fakeGitToken)
	if err == nil {
		t.Fatal("a rotate whose metadata advance failed must report failure, not success")
	}
	if code := faultCode(t, err); code != "version_conflict" {
		t.Fatalf("the second transaction must fail on its version precondition, got %v", err)
	}

	// The store rotated exactly once, and it holds the new material: the write Cloud asked for landed.
	rotated, token := secrets.rotated()
	if !rotated || token != fakeGitToken {
		t.Fatalf("the store must hold the rotated material exactly once, got rotated=%v token=%q", rotated, token)
	}
	// The pointer is behind: the row is at version 2 (the concurrent writer's bump only) and still
	// carries that writer's name, so this rotate's own advance never ran.
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND version=2 AND name='concurrent-writer' AND availability='available'`, credential); n != 1 {
		t.Fatal("the failed second transaction wrote the credential row: the store rotated but the pointer advance must not have applied")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND version=3`, credential); n != 0 {
		t.Fatal("the rotate's metadata advance applied despite the conflicting concurrent write")
	}
}

// TestAProjectThatFailsToBindACredentialLeavesNothingAndTheCorrectionRebinds covers the reachable half
// of the failed-binding hazard: `createProject` (`public.go:582`) is the one writer of
// `projects.credential_ref_id`, and it binds through the same two conditions the clone-time guard
// enforces. A create naming a credential that can never clone is refused at bind time, and the refusal
// must be total — no half-written Project, Workspace or Operation — so the caller can correct the
// binding by creating again with a usable credential.
//
// The unreachable half is a *re-bind of an existing Project's* column: no product path updates
// `credential_ref_id` after creation (PATCH only writes `name`, `public.go:163`), so there is no seam
// for it and it is reported rather than staged with a direct UPDATE.
//
// Non-vacuity: the refused and the corrected create differ only in the credential's availability —
// same repository, same scope, same branch — so a guard that accepted the withdrawn credential would
// make the first create succeed and the "leaves nothing" count would be non-zero, and a create that
// wrote the Project before checking the binding would leave a row behind even though the call failed.
func TestAProjectThatFailsToBindACredentialLeavesNothingAndTheCorrectionRebinds(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	tenant, user, claims := bootstrapCaller(t, store, "rebind")
	space := textOf(t, store, `SELECT id FROM collab_workspaces WHERE tenant_id=$1 ORDER BY id LIMIT 1`, tenant)

	withdrawn := stageOwnedCredential(t, store, tenant, user)
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", tenant, withdrawn, claims, "rebind-withdraw", 1), ""); err != nil {
		t.Fatalf("withdraw the credential the first attempt names: %v", err)
	}

	create := func(key, credential string) (Object, error) {
		out, _, err := store.Public(context.Background(), &PublicRequest{
			Method: "POST", Path: "/api/v1/tenants/" + tenant + "/spaces/" + space + "/projects",
			TenantID: tenant, SpaceID: space, Key: key,
			Body:     Object{"name": "bound", "repositoryUrl": admittedRepository, "defaultBranch": "main", "credentialRefId": credential},
			Identity: claims,
		})
		return out, err
	}

	// The failed attempt: the binding is refused before anything is written.
	if _, err := create("rebind-fail", withdrawn); err == nil {
		t.Fatal("creating a Project against a withdrawn credential must be refused")
	} else if code := faultCode(t, err); code != "repository_credential_frozen" {
		t.Fatalf("a failed bind must be refused as frozen, got %q", code)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM projects WHERE tenant_id=$1`, tenant); n != 0 {
		t.Fatalf("a refused bind left %d Project rows behind", n)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM operations WHERE tenant_id=$1`, tenant); n != 0 {
		t.Fatal("a refused bind left an Operation behind")
	}

	// The corrected binding: a fresh usable credential, same repository and scope, now succeeds and is
	// the column the Project carries.
	usable := stageOwnedCredential(t, store, tenant, user)
	if _, err := create("rebind-ok", usable); err != nil {
		t.Fatalf("the corrected binding must go through, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM projects WHERE tenant_id=$1 AND credential_ref_id=$2`, tenant, usable); n != 1 {
		t.Fatal("the corrected create did not bind the usable credential")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM projects WHERE tenant_id=$1 AND credential_ref_id=$2`, tenant, withdrawn); n != 0 {
		t.Fatal("the corrected create bound the withdrawn credential")
	}
}
