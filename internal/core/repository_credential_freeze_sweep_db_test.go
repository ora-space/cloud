package core

// Direct, PostgreSQL-backed evidence for `freezeInactiveRepositoryCredentials` — the recovery scan
// that withdraws a personal reference whose owner is no longer an active member (`repository_credentials.go:229`).
//
// The scan exists beside the `0023` triggers because the triggers only fire on the writes that end a
// membership or disable a user. A reference written *while its owner was already gone* is invisible to
// them, and that is the state every scene here stages: the departure is written first, the credential
// rows afterwards. Driving the scan directly (not through `refreshRuntimeControls`) keeps the
// assertions about the scan's own predicate rather than about the paths that invoke it.
//
// Every row is a real `credential_refs` row in a real isolated schema; the admission half runs the
// shipped `clone_dispatch`. Nothing here re-implements the sweep's predicate in Go.

import (
	"context"
	"testing"
)

// stageCredentialRow writes one `credential_refs` row in an explicit state, for the selection a sweep
// test needs. `reason` and `version` are stated rather than defaulted so an "already withdrawn" row
// can carry the reason and version a real withdrawal left, which is what the scan must not rewrite.
func stageCredentialRow(t *testing.T, store *Store, tenant, owner, attribution, availability, reason string, version int64) string {
	t.Helper()
	credential := newID()
	var unavailable any
	if reason != "" {
		unavailable = reason
	}
	stagedExec(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,availability,unavailable_reason,repository_scope,capabilities,version)
		VALUES($1,$2,$3,'git',$4,'staged',$5,'user_stated',$6,$7,$8,'["clone"]'::jsonb,$9)`,
		credential, tenant, owner, "credential-ref/"+credential, attribution, availability, unavailable, admittedScope, version)
	return credential
}

// stageActiveMember adds one active administrator and returns its user id. It keeps a tenant's
// `last_admin` invariant satisfied while another administrator is disabled, and provides the
// still-present owner an active reference beside the departing one, which is the adversarial
// neighbor a sweep written as "freeze everything here" would wrongly withdraw.
func stageActiveMember(t *testing.T, store *Store, tenant string) string {
	t.Helper()
	member := newID()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'member','active')`, member)
		tx.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenant, member)
		return Object{}
	}); err != nil {
		t.Fatalf("stage an active member: %v", err)
	}
	return member
}

// stageInactiveUserOwner provisions a tenant whose owner's *user* row (not its membership) is no
// longer usable, and returns the tenant and owner. The caller writes the credential rows afterwards,
// so the `disabled_user_repository_credentials` trigger cannot have seen them — which is what leaves
// the scan's own `u.status='active' AND u.deleted_at IS NULL` clause as the only thing that can
// withdraw the row.
func stageInactiveUserOwner(t *testing.T, store *Store, label, change string) (tenant, owner string) {
	t.Helper()
	tenant, owner, _ = bootstrapCaller(t, store, label)
	stageFillerAdmin(t, store, tenant)
	stagedExec(t, store, change, owner)
	return tenant, owner
}

// stageLiveProject stages an *active* Project whose main Workspace exists, bound to one credential.
// The Project is the discriminating input for the last selection question: the scan has no project
// predicate, so a live Project must neither protect its credential nor be touched by the scan.
func stageLiveProject(t *testing.T, store *Store, tenant, owner, credential string) string {
	t.Helper()
	space := textOf(t, store, `SELECT id FROM collab_workspaces WHERE tenant_id=$1 ORDER BY id LIMIT 1`, tenant)
	project, workspace := newID(), newID()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		tx.exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,credential_ref_id,lifecycle)
			VALUES($1,$2,$3,$4,'bound','https://git.example.invalid/group/private.git','main',$5,'active')`, project, tenant, owner, space, credential)
		tx.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready','main')`, workspace, tenant, owner, project)
		return Object{}
	}); err != nil {
		t.Fatalf("stage a live project: %v", err)
	}
	return project
}

// credentialRowWitness is one row's freeze-relevant state as a single comparable string. A sweep that
// withdrew the wrong row, rewrote its reason, or bumped its version twice would change one of these.
func credentialRowWitness(t *testing.T, store *Store, id string) string {
	t.Helper()
	return textOf(t, store, `SELECT availability||'/'||COALESCE(unavailable_reason,'')||'/'||version::text||'/'||secret_ref FROM credential_refs WHERE id=$1`, id)
}

// runFreezeSweep runs the recovery scan in its own transaction, exactly as `refreshRuntimeControls`
// does on every control action that precedes a credential check.
func runFreezeSweep(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.transact(context.Background(), func(tx *transaction) Object {
		freezeInactiveRepositoryCredentials(tx)
		return Object{}
	}); err != nil {
		t.Fatalf("run the freeze sweep: %v", err)
	}
}

// TestFreezeSweepFreezesExactlyTheRowsWhoseOwnerIsGoneAndIsIdempotent pins the scan's whole selection
// predicate and its idempotency in one scene.
//
// Non-vacuity is by construction of the input: the frozen target sits in the same tenant as a
// still-active owner's identical row and beside an already-`invalid` row that carries a real
// revocation reason, so a sweep written as "freeze every personal row in the tenant" would fail on
// the neighbor and a sweep that ignored `availability='available'` would overwrite a revocation. A
// departed owner in a *different* tenant is included so a sweep that only looked at one tenant
// would miss a row the predicate requires it to withdraw. The membership join is scoped to the
// credential's own tenant, so one row's owner is active but only in *another* tenant: a sweep that
// asked merely "is this owner active anywhere" would wrongly leave it available.
// The second run is the idempotency witness:
// the version column is the durable record of a write, so a second bump would be visible even though
// the availability value is unchanged.
func TestFreezeSweepFreezesExactlyTheRowsWhoseOwnerIsGoneAndIsIdempotent(t *testing.T) {
	store := dispatcherDB(t)

	// A departed owner whose membership already ended, with the rows written afterwards. These are
	// the rows the `0023` membership trigger could not have seen.
	tenant, departed, _ := bootstrapCaller(t, store, "sweep")
	stageFillerAdmin(t, store, tenant)
	stagedExec(t, store, `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, tenant, departed)

	personal := stageCredentialRow(t, store, tenant, departed, "personal", "available", "", 1)
	unknown := stageCredentialRow(t, store, tenant, departed, "unknown", "available", "", 1)
	revoked := stageCredentialRow(t, store, tenant, departed, "personal", "invalid", "revoked_by_owner", 3)
	alreadyFrozen := stageCredentialRow(t, store, tenant, departed, "personal", "frozen", "associated_member_departed", 5)
	team := stageCredentialRow(t, store, tenant, departed, "team", "available", "", 1)

	// An active owner in the same tenant, and an active owner in another tenant: neither is a
	// departure, so neither row may move.
	sameTenantActive := stageActiveMember(t, store, tenant)
	sameTenantRow := stageCredentialRow(t, store, tenant, sameTenantActive, "personal", "available", "", 1)
	otherTenant, otherActive, _ := bootstrapCaller(t, store, "keep")
	otherTenantRow := stageCredentialRow(t, store, otherTenant, otherActive, "personal", "available", "", 1)
	// The same active owner, holding a credential in the *departed* tenant. `credential_refs`'s
	// composite foreign key forces the owner to be a member of the credential's tenant, so the only
	// reachable form of "active elsewhere" is a dual membership: this owner is a disabled member of
	// `tenant` and an active member of `otherTenant`. The subquery's join is both tenant-scoped
	// (`m.tenant_id=c.tenant_id`) and status-scoped, so the row must freeze; a sweep that asked only
	// "is this owner active anywhere" would leave it available.
	stagedExec(t, store, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','disabled')`, tenant, otherActive)
	crossTenantRow := stageCredentialRow(t, store, tenant, otherActive, "personal", "available", "", 1)

	// A user that is disabled rather than a membership that ended, and one that is soft-deleted: the
	// scan's `u.status` / `u.deleted_at` clauses, reached with rows the users trigger never saw.
	userDisabledTenant, userDisabled := stageInactiveUserOwner(t, store, "user-disabled", `UPDATE users SET status='disabled' WHERE id=$1`)
	userDisabledRow := stageCredentialRow(t, store, userDisabledTenant, userDisabled, "personal", "available", "", 1)
	userDeletedTenant, userDeleted := stageInactiveUserOwner(t, store, "user-deleted", `UPDATE users SET deleted_at=now() WHERE id=$1`)
	userDeletedRow := stageCredentialRow(t, store, userDeletedTenant, userDeleted, "personal", "available", "", 1)

	// A departure in a third tenant: the scan is not scoped to any one tenant.
	third, thirdDeparted, _ := bootstrapCaller(t, store, "third")
	stageFillerAdmin(t, store, third)
	stagedExec(t, store, `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, third, thirdDeparted)
	thirdRow := stageCredentialRow(t, store, third, thirdDeparted, "personal", "available", "", 1)

	// A live Project bound to the departed owner's credential. The scan must withdraw the credential
	// anyway — a Project's own state is not evidence about its owner — and must leave the Project's
	// binding untouched, because withdrawing is a state change on the credential, never a write to the
	// Project.
	boundProject := stageLiveProject(t, store, tenant, departed, personal)

	runFreezeSweep(t, store)

	for _, want := range []struct{ id, state string }{
		{personal, "frozen/associated_member_departed/2/credential-ref/" + personal},
		{unknown, "frozen/associated_member_departed/2/credential-ref/" + unknown},
		{thirdRow, "frozen/associated_member_departed/2/credential-ref/" + thirdRow},
		// Frozen because its owner is not an active member *of this tenant*: the owner's membership here
		// is disabled while the same owner is active in another tenant, so only the tenant-scoped,
		// status-scoped join can decide this row.
		{crossTenantRow, "frozen/associated_member_departed/2/credential-ref/" + crossTenantRow},
		// The scan cannot tell a disabled user from a departed member and writes the membership
		// reason for both; pinning that here states the reason code the scan actually produces.
		{userDisabledRow, "frozen/associated_member_departed/2/credential-ref/" + userDisabledRow},
		{userDeletedRow, "frozen/associated_member_departed/2/credential-ref/" + userDeletedRow},
		// Left alone: a revocation is a different fact and must keep its own reason and version; an
		// already-frozen row is already in the scan's target state; a team reference is out of scope.
		{revoked, "invalid/revoked_by_owner/3/credential-ref/" + revoked},
		{alreadyFrozen, "frozen/associated_member_departed/5/credential-ref/" + alreadyFrozen},
		{team, "available//1/credential-ref/" + team},
		{sameTenantRow, "available//1/credential-ref/" + sameTenantRow},
		{otherTenantRow, "available//1/credential-ref/" + otherTenantRow},
	} {
		if got := credentialRowWitness(t, store, want.id); got != want.state {
			t.Fatalf("after the sweep, credential %s is %q, want %q", want.id, got, want.state)
		}
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM projects WHERE id=$1 AND lifecycle='active' AND credential_ref_id=$2`, boundProject, personal); n != 1 {
		t.Fatal("the sweep rewrote or cleared the live Project's credential binding")
	}

	// Idempotency: a second run over the same state must write nothing. The frozen row's version is
	// the witness — a second bump would contradict "the scan leaves an already-frozen row alone" —
	// and every other row is compared as before.
	after := map[string]string{}
	for _, id := range []string{personal, unknown, revoked, alreadyFrozen, team, sameTenantRow, otherTenantRow, thirdRow, userDisabledRow, userDeletedRow, crossTenantRow} {
		after[id] = credentialRowWitness(t, store, id)
	}
	runFreezeSweep(t, store)
	for id, want := range after {
		if got := credentialRowWitness(t, store, id); got != want {
			t.Fatalf("the second sweep changed credential %s: %q -> %q", id, want, got)
		}
	}
}

// TestASweepFrozenReferenceIsRefusedAtAdmission drives the withdrawn row through the shipped
// `clone_dispatch`, so the sweep's result is observed where it matters rather than only as a column
// value. The executor is installed, so an admission that slipped past the frozen check would be
// visibly executed and registered here; the refusal is the scan's doing, not a missing executor's.
func TestASweepFrozenReferenceIsRefusedAtAdmission(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	// The owner leaves before the reference exists, so the trigger cannot have frozen it and the scan
	// is the only writer that could have.
	stageDeparture(t, store, scene, "personal")
	swept := stageReference(t, store, scene, scene.user, "personal")
	runFreezeSweep(t, store)
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND unavailable_reason='associated_member_departed'`, swept); n != 1 {
		t.Fatal("the recovery scan did not freeze the reference written after its owner departed")
	}

	stagedExec(t, store, `UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, scene.project, swept)
	if _, err := scene.dispatch(store, "exec-swept"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("what the recovery scan froze must be refused at admission as frozen, got %v", err)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a reference the scan froze reached the executor %d times", len(cloner.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 0 {
		t.Fatalf("a reference the scan froze produced %d clone executions", n)
	}
}
