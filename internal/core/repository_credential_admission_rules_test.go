package core

import (
	"context"
	"strings"
	"testing"
)

// This file adds evidence for rules the admission guard and the `0023` triggers already enforce:
// which reference a Project may reach, what a departure does to one, what disabling or deleting the
// account itself does to one, and what revocation does to one. Nothing here changes behavior; every
// case drives the real `clone_dispatch` so the assertions are about what admission does rather than
// about a restatement of it.
//
// Two repository spellings, taken from the existing admission tests: the Project URL keeps whatever the
// caller submitted, and the reference's scope is its normalized form.

const (
	admittedRepository = "https://git.example.invalid/group/private.git"
	admittedScope      = "https://git.example.invalid/group/private"
	otherRepository    = "https://git.example.invalid/group/other.git"
)

// stagedExec runs one statement directly against the isolated schema, for state a test needs and the
// product has no path to: a soft-deleted row, a reference whose owner has already left, a departure
// written before the row it should have frozen.
func stagedExec(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.Pool.Exec(query, args...); err != nil {
		t.Fatalf("stage %q: %v", query, err)
	}
}

// stageMember stages one active administrator of the scene's tenant and returns its id.
//
// These cases disable a member on purpose, and they must not disable the one the scene already has: it
// is the operation's actor, and the control plane checks the actor's membership before any credential
// rule is read. Disabling it would make every case here fail as `membership_required` and prove nothing
// about credentials. A second administrator also keeps the tenant's own integrity rule satisfied while
// the first one is disabled.
func stageMember(t *testing.T, store *Store, scene credentialedScene) string {
	t.Helper()
	member := newID()
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'member','active')`, member)
		t.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, scene.tenant, member)
		return Object{}
	}); err != nil {
		t.Fatalf("stage a member: %v", err)
	}
	return member
}

// stageReference stages one available reference of the given attribution, owned by `owner` in the
// scene's tenant, and returns its id. It deliberately does not bind the Project to it.
func stageReference(t *testing.T, store *Store, scene credentialedScene, owner, attribution string) string {
	t.Helper()
	credential := newID()
	stagedExec(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,availability,repository_scope,capabilities)
		VALUES($1,$2,$3,'git',$4,'staged',$5,'available',$6,'["clone"]'::jsonb)`,
		credential, scene.tenant, owner, "credential-ref/"+credential, attribution, admittedScope)
	return credential
}

// stageDeparture stages the case the departure rule exists for: the Project's owner has left while the
// clone is driven by someone still present.
//
// The owner is the one who leaves rather than a second member, and that is forced by the schema:
// `projects.owner_user_id` is immutable and a Project's reference must be owned by the same user, so
// the departing member and the Project's owner are the same person by construction. What moves instead
// is the operation's actor, because the control plane checks the actor's membership before it reads any
// credential rule — an operation whose actor had left would be refused as `membership_required` and
// would never reach the checks these tests are about. `attribution` is set first, so the departure
// trigger has something to freeze or to leave alone.
func stageDeparture(t *testing.T, store *Store, scene credentialedScene, attribution string) {
	t.Helper()
	successor := stageMember(t, store, scene)
	stagedExec(t, store, `UPDATE credential_refs SET attribution=$2 WHERE id=$1`, scene.credential, attribution)
	stagedExec(t, store, `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, scene.tenant, scene.user)
	stagedExec(t, store, `UPDATE operations SET actor_user_id=$2 WHERE id=$1`, scene.operation, successor)
}

// TestAReferenceTheProjectCannotReachIsRefusedRatherThanFallingBackToAnonymous pins both lines that
// keep a Project from being authorized by a reference that is not its own. The schema is the first:
// `projects` binds a reference through `(credential_ref_id, tenant_id, owner_user_id)`, so a Project
// cannot name another tenant's or another owner's reference at all. That is a schema guarantee rather
// than an admission rule, and this test fails if it is ever weakened, because the guard's tenant
// predicate is a second, independent line: if the constraint were dropped, the guard would still refuse
// and nothing else here would notice the two had collapsed into one.
//
// What is reachable, and what the second half covers, is a reference that is still bound and no longer
// selectable — soft deletion is how revocation reaches the lookup. The dangerous alternative is worth
// naming: treating "the bound reference is gone" as "no reference at all" would clone a private
// repository anonymously, which is the fallback the guard exists to prevent.
func TestAReferenceTheProjectCannotReachIsRefusedRatherThanFallingBackToAnonymous(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	// A real reference of the right shape, owned by somebody else entirely.
	stranger, owner, foreign := newID(), newID(), newID()
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO users(id,display_name,status) VALUES($1,'stranger','active')`, owner)
		t.exec(`INSERT INTO tenants(id,name,status) VALUES($1,'stranger','active')`, stranger)
		t.exec(`INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, stranger, owner)
		t.exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,availability,repository_scope,capabilities)
			VALUES($1,$2,$3,'git','credential-ref/foreign','foreign','personal','available',$4,'["clone"]'::jsonb)`,
			foreign, stranger, owner, admittedScope)
		return Object{}
	}); err != nil {
		t.Fatalf("stage a reference from another tenant: %v", err)
	}
	if _, err := store.Pool.Exec(`UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, scene.project, foreign); err == nil {
		t.Fatal("the schema now lets a Project bind another tenant's reference: the lookup's tenant predicate has become the only line, and it needs a test of its own")
	}

	// The reachable case: the Project's own reference, soft-deleted. The binding is untouched, so the
	// lookup is what has to refuse.
	stagedExec(t, store, `UPDATE credential_refs SET deleted_at=now() WHERE id=$1`, scene.credential)
	if _, err := scene.dispatch(store, "exec-soft-deleted"); faultCode(t, err) != "repository_credential_unavailable" {
		t.Fatalf("a soft-deleted reference must be refused as unavailable, got %v", err)
	}

	if len(cloner.calls) != 0 {
		t.Fatalf("a reference the Project cannot reach reached the executor %d times", len(cloner.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 0 {
		t.Fatalf("a refused credentialed clone left %d clone executions behind", n)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2`, scene.project, scene.credential); n != 1 {
		t.Fatal("the refusal rewrote or cleared the Project's own binding")
	}
}

// TestARevokedReferenceIsRefusedAsRevokedBeforeTheDeploymentIsConsulted pins two things at once: that
// revocation is observed at admission, and that the reference's own rule is reported before the
// deployment's. No executor is installed here, so a reference that passed the earlier checks would be
// refused with `repository_credential_executor_unavailable`; receiving `repository_credential_revoked`
// instead is what shows the order. The code is also what makes this refusal terminal rather than
// retryable, so it is the difference between a Project that fails once and one that keeps a revoked
// reference's operation slot for good.
func TestARevokedReferenceIsRefusedAsRevokedBeforeTheDeploymentIsConsulted(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	stagedExec(t, store, `UPDATE credential_refs SET availability='invalid',unavailable_reason='revoked_by_owner' WHERE id=$1`, scene.credential)

	if _, err := scene.dispatch(store, "exec-revoked"); faultCode(t, err) != repositoryCredentialRevoked {
		t.Fatalf("a revoked reference must be refused as revoked, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 0 {
		t.Fatalf("a revoked reference produced %d clone executions", n)
	}
}

// TestADepartedOwnersReferenceIsFrozenAtAdmission covers the two mechanisms that enforce the departure
// rule, and the last step is why the test needs both. The `0023` trigger freezes the row as the
// departure is written, so the first dispatch is refused by availability alone. Setting the row back to
// `available` removes that evidence and leaves the guard's own predicate — "a personal or unknown
// reference whose owner is not an active member" — as the only thing refusing, which is exactly the
// state a reference the trigger never saw would be in. If only the trigger enforced this, a row that
// reached `available` by any other path would be admitted. No executor is installed, so
// `repository_credential_frozen` also shows the reference's rule firing before the deployment's.
func TestADepartedOwnersReferenceIsFrozenAtAdmission(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	stageDeparture(t, store, scene, "personal")

	if _, err := scene.dispatch(store, "exec-departed"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("a departed owner's reference must be refused as frozen, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND unavailable_reason='associated_member_departed'`, scene.credential); n != 1 {
		t.Fatal("the departure did not freeze the reference it belongs to")
	}

	stagedExec(t, store, `UPDATE credential_refs SET availability='available',unavailable_reason=NULL WHERE id=$1`, scene.credential)
	if _, err := scene.dispatch(store, "exec-departed-unfrozen"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("admission must refuse an available reference whose owner is not an active member, got %v", err)
	}
}

// TestATeamReferenceSurvivesItsOwnersDepartureAtAdmission is the discriminating half of the case above:
// identical rows, identical departure, one column different. A team reference keeps its history, so the
// departure neither freezes it nor stops admission at the membership predicate; the first refusal it
// meets is the deployment-level one, and no executor is installed here for exactly that reason. If the
// personal rule ever started applying to team references, this test would report
// `repository_credential_frozen` instead — the regression worth catching, because it would make every
// team credential unusable the moment its owner changed roles.
func TestATeamReferenceSurvivesItsOwnersDepartureAtAdmission(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	stageDeparture(t, store, scene, "team")

	if _, err := scene.dispatch(store, "exec-team"); faultCode(t, err) != repositoryCredentialExecutorUnavailable {
		t.Fatalf("a team reference must outlive its owner's membership, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='available'`, scene.credential); n != 1 {
		t.Fatal("the departure froze a team reference: team history is retained, not frozen")
	}
}

// TestAFrozenReferenceIsRefusedBeforeItsScopeIsCompared fixes the order between two refusals that share
// one row. The first dispatch runs against a reference whose scope names a different repository, so the
// scope comparison is what refuses it; the second flips only availability and expects the freeze
// instead. Reporting the scope first would send an operator to re-scope a reference that is unusable
// for an unrelated reason. The executor is installed throughout, so "never reached the executor" is an
// assertion rather than a consequence of no executor existing.
func TestAFrozenReferenceIsRefusedBeforeItsScopeIsCompared(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, otherRepository, admittedScope)

	if _, err := scene.dispatch(store, "exec-scope"); faultCode(t, err) != "repository_credential_scope_mismatch" {
		t.Fatalf("a reference scoped to another repository must be refused as a mismatch, got %v", err)
	}
	stagedExec(t, store, `UPDATE credential_refs SET availability='frozen',unavailable_reason='associated_member_departed' WHERE id=$1`, scene.credential)
	if _, err := scene.dispatch(store, "exec-frozen-scope"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("a frozen reference must be refused as frozen before its scope is compared, got %v", err)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a refused reference reached the executor %d times", len(cloner.calls))
	}
}

// TestAnExecutionCannotRecordHalfACredentialBindingAndRevocationLeavesItAlone covers the durable
// record of what a credentialed clone ran with, from both ends. The pairing is first: `0023`'s
// `execution_credential_binding` check admits either both columns or neither, and an execution that
// recorded an id without a version could be read as having run with whatever material the reference
// holds *later* — which is the one thing a per-execution freeze exists to prevent. The second half is
// the other direction: revoking or rotating the reference advances its own version and must not rewrite
// the version an execution already recorded, because that record is the evidence of what was
// authorized, and it has to survive exactly the event (revocation) that makes it interesting.
func TestAnExecutionCannotRecordHalfACredentialBindingAndRevocationLeavesItAlone(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	record := func(execution, ref, version string) error {
		_, err := store.Pool.Exec(`INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id,credential_ref_id,credential_ref_version)
			VALUES($1,$2,$3,$4,'{"kind":"clone"}',1,$1::text,NULLIF($5,'')::uuid,NULLIF($6,'')::bigint)`,
			execution, scene.operation, scene.workspace, scene.node, ref, version)
		return err
	}
	if err := record(newID(), scene.credential, "1"); err != nil {
		t.Fatalf("an execution that names a reference and a version must be recordable: %v", err)
	}
	for _, half := range []struct{ name, ref, version string }{
		{"an id without a version", scene.credential, ""},
		{"a version without an id", "", "1"},
	} {
		err := record(newID(), half.ref, half.version)
		if err == nil {
			t.Fatalf("the schema recorded %s: the execution would then read as having run with whatever the reference holds later", half.name)
		}
		// The refusal has to be the pairing constraint rather than any other accident of the row:
		// PostgreSQL names the constraint it rejected, so the evidence stays specific.
		if !strings.Contains(err.Error(), "execution_credential_binding") {
			t.Fatalf("%s was refused by something other than the pairing constraint: %v", half.name, err)
		}
	}

	stagedExec(t, store, `UPDATE credential_refs SET version=version+1,availability='invalid',unavailable_reason='revoked_by_owner' WHERE id=$1`, scene.credential)
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE credential_ref_id=$1 AND credential_ref_version=1`, scene.credential); n != 1 {
		t.Fatal("revoking the reference rewrote the version an execution had already recorded")
	}
}

// TestTheRecoveryScanFreezesWhatTheTriggersCouldNotSeeAndLeavesTeamAlone covers
// `freezeInactiveRepositoryCredentials` directly. The triggers fire on the writes that end a
// membership, so they cannot show what the scan is for: references written while their owner was
// already gone, or a membership that ended through a path the triggers do not cover. The scan is run
// here over exactly that state, and its attribution predicate is checked in both directions — team
// rows keep their history, as they do in the triggers.
//
// The last step dispatches against the repaired row: a scan whose result admission does not observe
// would be a table with no reader.
func TestTheRecoveryScanFreezesWhatTheTriggersCouldNotSeeAndLeavesTeamAlone(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	// The owner leaves before the references below exist, which is the state the triggers cannot see:
	// they fire on the writes that end a membership, not on rows written afterwards.
	stageDeparture(t, store, scene, "personal")
	leftover := stageReference(t, store, scene, scene.user, "unknown")
	team := stageReference(t, store, scene, scene.user, "team")

	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		freezeInactiveRepositoryCredentials(t)
		return Object{}
	}); err != nil {
		t.Fatalf("run the recovery scan: %v", err)
	}

	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND unavailable_reason='associated_member_departed'`, leftover); n != 1 {
		t.Fatal("the recovery scan left an unowned personal reference available")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='available'`, team); n != 1 {
		t.Fatal("the recovery scan froze a team reference: team history is retained")
	}

	stagedExec(t, store, `UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, scene.project, leftover)
	if _, err := scene.dispatch(store, "exec-swept"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("what the recovery scan froze must be refused at admission, got %v", err)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a reference the scan froze reached the executor %d times", len(cloner.calls))
	}
}

// TestADisabledOrDeletedAccountFreezesItsReferencesThroughTheTrigger covers the account half of the
// same rule on the path the recovery scan cannot stand in for. The scan and the `users` trigger
// withdraw the same rows for the same absence, but they are not interchangeable: the scan only sees
// rows that already existed when it runs, while the trigger fires on the write that disables or
// deletes the account. The scene is therefore the opposite of the sweep tests' — the reference is
// written first and the account change afterwards, which is the order that reaches the trigger.
//
// Both spellings of "the account is gone" are driven, because the trigger watches two columns and they
// arrive by different statements. The reason is what makes the evidence specific rather than merely
// consistent with the outcome: the scan writes `associated_member_departed` for the same absence, so
// reading back `associated_user_disabled` shows which writer acted, and a scene in which the trigger
// never fired would fail here even though the row would still be frozen.
func TestADisabledOrDeletedAccountFreezesItsReferencesThroughTheTrigger(t *testing.T) {
	for _, gone := range []struct{ name, statement string }{
		{"a disabled account", `UPDATE users SET status='disabled' WHERE id=$1`},
		{"a soft-deleted account", `UPDATE users SET deleted_at=now() WHERE id=$1`},
	} {
		t.Run(gone.name, func(t *testing.T) {
			store := dispatcherDB(t)
			cloner := &recordingCloner{}
			store.SetCredentialedCloner(cloner)
			scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

			// Two staged facts, each forced by something other than this rule. The tenant keeps an active
			// administrator — the account under test is its own — because disabling the last one is refused
			// by the deferred tenant guard rather than reaching the credential trigger. And the Operation's
			// actor moves to that member, because the control plane checks the actor's membership before it
			// reads any credential rule: an actor whose own account is gone would be refused as a membership
			// problem and never reach the freeze this case is about.
			successor := stageMember(t, store, scene)
			stagedExec(t, store, `UPDATE operations SET actor_user_id=$2 WHERE id=$1`, scene.operation, successor)
			before := scalarOf(t, store, `SELECT version FROM credential_refs WHERE id=$1`, scene.credential)

			stagedExec(t, store, gone.statement, scene.user)

			if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen'`, scene.credential); n != 1 {
				t.Fatal("writing the account change must freeze the reference the account owns")
			}
			if reason := scalarText(t, store, `SELECT coalesce(unavailable_reason,'') FROM credential_refs WHERE id=$1`, scene.credential); reason != "associated_user_disabled" {
				t.Fatalf("the reference must carry the account's own reason, got %q", reason)
			}
			if got := scalarOf(t, store, `SELECT version FROM credential_refs WHERE id=$1`, scene.credential); got != before+1 {
				t.Fatalf("the freeze must advance the reference's version from %d, got %d", before, got)
			}
			if _, err := scene.dispatch(store, "exec-account-gone"); faultCode(t, err) != "repository_credential_frozen" {
				t.Fatalf("what the account trigger froze must be refused at admission, got %v", err)
			}
			if len(cloner.calls) != 0 {
				t.Fatalf("a reference the account trigger froze reached the executor %d times", len(cloner.calls))
			}
			if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 0 {
				t.Fatalf("a refused credentialed clone left %d clone executions behind", n)
			}
		})
	}
}

// TestADeploymentManagedReferenceIsRefusedForItsEmptyCapabilityOnceAnExecutorExists stages the case the
// integration suite structurally cannot reach. There no executor is installed, so every credentialed
// clone is refused at the deployment level and two very different obstacles produce the same answer: a
// deployment that can serve nothing, and a reference that permits nothing. Installing the executor
// removes the first refusal, and what remains is the capability clause reading the column the
// deployment path actually writes — `ConfigureCredential` omits `capabilities`, so the reference it
// creates carries the column's default, the empty set, which `CanonicalCapabilities` refuses rather than
// reading as "no restriction".
//
// The reference is created through that path instead of being assembled with an UPDATE, because the
// shape is the point. An empty set written by hand would only show that the guard refuses an empty set;
// this shows what happens to the reference a deployment can really create.
//
// Two stores, because each seeded scene takes the schema's single global controller lease and a refused
// dispatch settles its Operation as failed — neither the lease nor a terminal Operation can be reused,
// so the control has to be its own scene. The control is the same write path with the one capability
// granted, and it is admitted, which is what makes the refusal above about what the reference declares
// rather than about deployment-managed references being unusable.
func TestADeploymentManagedReferenceIsRefusedForItsEmptyCapabilityOnceAnExecutorExists(t *testing.T) {
	refused := dispatcherDB(t)
	refusedCloner := &recordingCloner{}
	refused.SetCredentialedCloner(refusedCloner)
	underTest := seedCredentialedCloneScene(t, refused, admittedRepository, admittedScope)
	empty, err := refused.ConfigureCredential(context.Background(), underTest.tenant, underTest.user, "credential-ref/deployment-managed")
	if err != nil {
		t.Fatalf("stage the deployment-managed reference: %v", err)
	}
	stagedExec(t, refused, `UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, underTest.project, empty.S("id"))

	// The three facts that make this the deployment path's shape, read back rather than assumed: the
	// write path sets none of them, so every default has to be the one the guard actually sees.
	if n := scalarOf(t, refused, `SELECT count(*) FROM credential_refs WHERE id=$1 AND attribution='unknown' AND capabilities='[]'::jsonb AND repository_scope IS NULL AND availability='available' AND deleted_at IS NULL`, empty.S("id")); n != 1 {
		t.Fatal("ConfigureCredential must write the attribution, capability set and scope this case is about")
	}
	// The owner is present and active and the executor is installed, so nothing but the capability set
	// stands between this reference and admission.
	if n := scalarOf(t, refused, `SELECT count(*) FROM credential_refs c JOIN tenant_memberships m ON m.tenant_id=c.tenant_id AND m.user_id=c.owner_user_id JOIN users u ON u.id=m.user_id WHERE c.id=$1 AND m.status='active' AND u.status='active' AND u.deleted_at IS NULL`, empty.S("id")); n != 1 {
		t.Fatal("the reference's owner must be present, or the refusal below could be the freeze rule instead")
	}

	if _, err := underTest.dispatch(refused, "exec-managed-empty-capability"); faultCode(t, err) != repositoryCredentialCapabilityUnsupported {
		t.Fatalf("a deployment-managed reference that permits no clone must be refused for its capability, got %v", err)
	}
	if len(refusedCloner.calls) != 0 {
		t.Fatalf("a reference that permits no clone reached the executor %d times", len(refusedCloner.calls))
	}
	if n := scalarOf(t, refused, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, underTest.workspace); n != 0 {
		t.Fatalf("a refused credentialed clone left %d clone executions behind", n)
	}

	admitted := dispatcherDB(t)
	admittedCloner := &recordingCloner{}
	admitted.SetCredentialedCloner(admittedCloner)
	control := seedCredentialedCloneScene(t, admitted, admittedRepository, admittedScope)
	granted, err := admitted.ConfigureCredential(context.Background(), control.tenant, control.user, "credential-ref/deployment-managed")
	if err != nil {
		t.Fatalf("stage the control's deployment-managed reference: %v", err)
	}
	stagedExec(t, admitted, `UPDATE projects SET credential_ref_id=$2 WHERE id=$1`, control.project, granted.S("id"))
	stagedExec(t, admitted, `UPDATE credential_refs SET capabilities='["clone"]'::jsonb WHERE id=$1`, granted.S("id"))

	if _, err := control.dispatch(admitted, "exec-managed-capable"); err != nil {
		t.Fatalf("the same deployment-managed reference must be admitted once it declares the capability, got %v", err)
	}
	if len(admittedCloner.calls) != 1 {
		t.Fatalf("the admitted clone reached the executor %d times, want 1", len(admittedCloner.calls))
	}
}
