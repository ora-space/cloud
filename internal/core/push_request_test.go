package core

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
)

// This file covers the Cloud half of the push decision: the controlled request face, the single-use
// authorization record, the exact binding, idempotency and the audit. Every case drives the real
// public route rather than the helper underneath it, so what is asserted is what a caller meets.
//
// The executor is a test double in the cases that need one and absent in the cases that are about the
// deployment having none. Nothing here reaches a remote and nothing here can: the write path does not
// exist yet, which is the state under test rather than an inconvenience of it.

const (
	pushRef       = "refs/heads/main"
	pushOldCommit = "0a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b"
	pushNewCommit = "9f8e7d6c5b4a39281706f5e4d3c2b1a09f8e7d6c"
	// pushThirdCommit is a value the approval authorized neither from nor to: what a reconciliation finds
	// when somebody else moved the ref, and the evidence the conflict case has to keep.
	pushThirdCommit = "13579bdf2468ace013579bdf2468ace013579bdf"
	// pushNullCommit is Git's null object id: a well-formed spelling that names no commit, and how a
	// delete and a create-from-nothing are written.
	pushNullCommit = "0000000000000000000000000000000000000000"
)

// pushScene is one tenant, a Project bound to a credential of the given capability set, and that
// Project's main Workspace — the smallest state from which a push approval can be made.
type pushScene struct {
	tenant     string
	user       string
	project    string
	workspace  string
	credential string
	claims     *Claims
}

// seedPushScene seeds the scene with the bootstrapped administrator as the Workspace's creator.
func seedPushScene(t *testing.T, store *Store, capabilities string) pushScene {
	t.Helper()
	tenant, user, claims := bootstrapCaller(t, store, "push")
	return seedPushSceneFor(t, store, tenant, user, claims, user, capabilities)
}

// seedPushSceneFor seeds the scene with an explicit Workspace creator, which is the one thing a case
// needs to vary: the creator is the identity the Workspace's own runtime-use gate admits, and keeping
// it distinct from the Project's owner is how a case reaches the push rule's own refusal rather than
// the shared gate that precedes it.
//
// The capability set is spelled by the case because it is the whole difference between an approval and
// a refusal: the product's credential path writes exactly `["clone"]`, so a case that needs a
// push-capable reference states the set it wants rather than pretending one exists.
func seedPushSceneFor(t *testing.T, store *Store, tenant, user string, claims *Claims, creator, capabilities string) pushScene {
	t.Helper()
	scene := pushScene{tenant: tenant, user: user, claims: claims, project: newID(), workspace: newID(), credential: newID()}
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		space := t.one("SELECT id FROM collab_workspaces WHERE tenant_id=$1 AND archived_at IS NULL", tenant).S("id")
		t.exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle)
			VALUES($1,$2,$3,$4,'pushed',$5,'main','active')`, scene.project, tenant, user, space, admittedRepository)
		t.exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,availability,repository_scope,capabilities)
			VALUES($1,$2,$3,'git',$4,'push','personal','user_stated','available',$5,$6::jsonb)`,
			scene.credential, tenant, user, "credential-ref/"+scene.credential, admittedScope, capabilities)
		t.exec("UPDATE projects SET credential_ref_id=$2 WHERE id=$1", scene.project, scene.credential)
		// The owner is the Project's owner, as the schema requires; only the recorded creator varies.
		t.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$4,'verified_request',$5,'main','running','ready','main')`,
			scene.workspace, tenant, user, creator, scene.project)
		return Object{}
	}); err != nil {
		t.Fatalf("seed the push scene: %v", err)
	}
	return scene
}

// pushRequest drives the accepting route once. The Idempotency-Key is a separate parameter from the
// body's requestId on purpose: the two are different layers, and a case that repeats a request to test
// the durable one must not be answered by the transport one first.
func (s pushScene) pushRequest(store *Store, key, requestID, ref, oldCommit, newCommit string) (Object, int, error) {
	return store.Public(context.Background(), &PublicRequest{
		Method: "POST", Path: "/api/v1/tenants/" + s.tenant + "/workspaces/" + s.workspace + "/pushes",
		TenantID: s.tenant, WorkspaceID: s.workspace, Key: key,
		Body:     Object{"requestId": requestID, "ref": ref, "oldCommit": oldCommit, "newCommit": newCommit},
		Identity: s.claims,
	})
}

// pushRequestIn drives the accepting route against another tenant's path, which is what the tenant
// scope of the route is asked about.
func (s pushScene) pushRequestIn(store *Store, tenant, key, requestID string) (Object, int, error) {
	return store.Public(context.Background(), &PublicRequest{
		Method: "POST", Path: "/api/v1/tenants/" + tenant + "/workspaces/" + s.workspace + "/pushes",
		TenantID: tenant, WorkspaceID: s.workspace, Key: key,
		Body:     Object{"requestId": requestID, "ref": pushRef, "oldCommit": pushOldCommit, "newCommit": pushNewCommit},
		Identity: s.claims,
	})
}

// readPush drives the read route for one approval.
func (s pushScene) readPush(store *Store, pushID string) (Object, int, error) {
	return store.Public(context.Background(), &PublicRequest{
		Method: "GET", Path: "/api/v1/tenants/" + s.tenant + "/workspaces/" + s.workspace + "/pushes/" + pushID,
		TenantID: s.tenant, WorkspaceID: s.workspace, PushID: pushID, Identity: s.claims,
	})
}

// listPushes drives the read route's listing.
func (s pushScene) listPushes(store *Store) (Object, int, error) {
	return store.Public(context.Background(), &PublicRequest{
		Method: "GET", Path: "/api/v1/tenants/" + s.tenant + "/workspaces/" + s.workspace + "/pushes",
		TenantID: s.tenant, WorkspaceID: s.workspace, Limit: 50, Identity: s.claims,
	})
}

// pushRow reads one approval's durable columns. It takes its own transaction: a case reads this from
// inside a dispatch, where no Cloud transaction is held.
func pushRow(t *testing.T, store *Store, id string) Object {
	t.Helper()
	var row Object
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		row = t.one("SELECT * FROM push_requests WHERE id=$1", id)
		return Object{}
	}); err != nil {
		t.Fatalf("read the push approval: %v", err)
	}
	return row
}

// pushState reads back the state and reason code as one string, so a failure names both.
func pushState(t *testing.T, store *Store, id string) string {
	t.Helper()
	return scalarText(t, store, `SELECT coalesce(state,'')||'/'||coalesce(result_code,'') FROM push_requests WHERE id=$1`, id)
}

// pushObserved reads back the commit the executor's reconciliation saw, as the empty string when none
// was recorded. The distinction the SQL draws between NULL and the empty string is collapsed here on
// purpose: no reader of the audit can tell those apart and none should have to, since "no observation
// was made" is the only thing either spelling can mean for this column.
func pushObserved(t *testing.T, store *Store, id string) string {
	t.Helper()
	return scalarText(t, store, `SELECT coalesce(observed_commit,'') FROM push_requests WHERE id=$1`, id)
}

// faultStatus reads the HTTP status a coded refusal would be answered with. `Public` reports a refusal
// as an error and leaves its own status variable at its success default, so the status a caller meets
// lives on the Fault and not on the value it returns beside it. Every case below asserts both the
// status and the code, because they are two independent facts about the same refusal.
func faultStatus(t *testing.T, err error) int {
	t.Helper()
	var fault *Fault
	if !errors.As(err, &fault) {
		t.Fatalf("want a coded refusal, got %v", err)
	}
	return fault.Status
}

// recordingPushExecutor records what it was handed and answers with the scripted outcome.
type recordingPushExecutor struct {
	calls   []PushExecution
	outcome PushOutcome
	err     error
	// during runs inside Execute, while no Cloud transaction is held: it is how a case reads the
	// durable record at the instant the write was attempted.
	during func()
}

func (e *recordingPushExecutor) Execute(_ context.Context, push PushExecution) (PushOutcome, error) {
	e.calls = append(e.calls, push)
	if e.during != nil {
		e.during()
	}
	return e.outcome, e.err
}

// pushRefusalError is a coded refusal: the executor reached an answer, and the answer was no.
type pushRefusalError struct{ code string }

func (e pushRefusalError) Error() string      { return e.code }
func (e pushRefusalError) ReasonCode() string { return e.code }

// TestAPushApprovalIsRefusedForACallerOutsideTheTenant is the outermost half of the identity rule: a
// verified identity that is not a member of the tenant reaches nothing, and the refusal is the shared
// membership gate's rather than any push rule's.
//
// It is also the shape of the requirement that no message, tool call, session event or generated
// content can be an authorization: there is no field in the request body and no route through which
// anything but a verified end-user identity can present itself as the approver, and an identity with no
// standing in the tenant is refused before the push is even read.
func TestAPushApprovalIsRefusedForACallerOutsideTheTenant(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)

	// A verified identity the deployment has never seen. `Public` provisions the user from the claims,
	// so what is refused here is the membership and not the authentication.
	outsider := scene
	outsider.claims = &Claims{Source: "lifecycle-bootstrap", Subject: "outsider-" + newID(), DisplayName: "Outsider"}
	if _, _, err := outsider.pushRequest(store, "key-outsider", "req-outsider", pushRef, pushOldCommit, pushNewCommit); err == nil {
		t.Fatal("an identity with no membership in the tenant must not be able to approve a push")
	} else if faultStatus(t, err) != 403 || faultCode(t, err) != "membership_required" {
		t.Fatalf("a caller outside the tenant must be refused by the membership gate, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if len(executor.calls) != 0 {
		t.Fatalf("a refused caller reached the executor: %d calls", len(executor.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE tenant_id=$1`, scene.tenant); n != 0 {
		t.Fatalf("a refused caller left %d approvals behind", n)
	}
	// The control: the same body on the same route, from the Project's owner, is accepted. Without it a
	// route that refused everything would pass the case above.
	if _, _, err := scene.pushRequest(store, "key-owner", "req-owner", pushRef, pushOldCommit, pushNewCommit); err != nil {
		t.Fatalf("the Project's owner must be able to approve, got %v", err)
	}
}

// TestAPushApprovalRequiresTheProjectRuleAndNotOnlyTheWorkspaceOne separates the two predicates that
// compose at the request face. A member who may use the Workspace — they created it — but who owns
// neither the Project nor an administrator role is refused by the push rule itself, and the code is the
// push rule's own, which is what shows the two checks have not collapsed into one.
func TestAPushApprovalRequiresTheProjectRuleAndNotOnlyTheWorkspaceOne(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{}
	store.SetPushExecutor(executor)
	tenant, owner, ownerClaims := bootstrapCaller(t, store, "push")
	member := newID()
	stagedExec(t, store, `INSERT INTO users(id,display_name,status) VALUES($1,'member','active')`, member)
	stagedExec(t, store, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'member','active')`, tenant, member)
	memberClaims := callerIdentity(t, store, member)
	scene := seedPushSceneFor(t, store, tenant, owner, ownerClaims, member, `["push"]`)

	asMember := scene
	asMember.claims = memberClaims
	if _, _, err := asMember.pushRequest(store, "key-member", "req-member", pushRef, pushOldCommit, pushNewCommit); err == nil {
		t.Fatal("a member who created the Workspace but owns neither the Project nor the tenant must not approve a push")
	} else if faultStatus(t, err) != 403 || faultCode(t, err) != "push_approval_forbidden" {
		t.Fatalf("the Project rule must be the one that refuses, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if len(executor.calls) != 0 || scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace) != 0 {
		t.Fatal("a refused approval reached the executor or was recorded")
	}
	// The same caller reads nothing either, so the read face answers the same question as the write.
	if _, _, err := asMember.readPush(store, scene.workspace); err == nil {
		t.Fatal("a caller who may not approve must not be able to read the approvals either")
	} else if faultStatus(t, err) != 403 || faultCode(t, err) != "push_approval_forbidden" {
		t.Fatalf("the read face must refuse the same caller, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
}

// TestACloneOnlyReferenceIsNeverSubstitutedForAWriteReference is the capability boundary read from the
// request face: the capability set is the reference's whole authority, and a reference that may only
// read is refused rather than treated as usable for a write.
//
// The last half is the discriminating one. The same scene and the same request with one column changed
// — `["clone","push"]` — is admitted, because the requirement is that the set contains push, not that
// it contains nothing else. Without that half, a guard that refused every reference would pass this
// test.
func TestACloneOnlyReferenceIsNeverSubstitutedForAWriteReference(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["clone"]`)

	if _, _, err := scene.pushRequest(store, "key-clone-only", "req-clone-only", pushRef, pushOldCommit, pushNewCommit); err == nil {
		t.Fatal("a clone-only reference must not authorize a write")
	} else if faultStatus(t, err) != 409 || faultCode(t, err) != repositoryCredentialCapabilityUnsupported {
		t.Fatalf("a clone-only reference must be refused for its capability, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 0 {
		t.Fatalf("a refused capability left %d approvals behind", n)
	}
	if len(executor.calls) != 0 {
		t.Fatal("a clone-only reference reached the executor")
	}
	// An empty set is not "no restriction" either: the column's default permits nothing, and reading it
	// as permission would turn an unclassified reference into an unrestricted one.
	stagedExec(t, store, `UPDATE credential_refs SET capabilities='[]'::jsonb WHERE id=$1`, scene.credential)
	if _, _, err := scene.pushRequest(store, "key-empty", "req-empty", pushRef, pushOldCommit, pushNewCommit); err == nil {
		t.Fatal("a reference that declares no capability must not authorize a write")
	} else if faultStatus(t, err) != 409 || faultCode(t, err) != repositoryCredentialCapabilityUnsupported {
		t.Fatalf("an unclassified reference must be refused for its capability, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	// A reference that declares more than a push needs is admitted: the set is the reference's
	// authority, and it belongs to whatever work exercises it.
	stagedExec(t, store, `UPDATE credential_refs SET capabilities='["clone","push"]'::jsonb WHERE id=$1`, scene.credential)
	if _, _, err := scene.pushRequest(store, "key-both", "req-both", pushRef, pushOldCommit, pushNewCommit); err != nil {
		t.Fatalf("a reference that declares push must be admitted, got %v", err)
	}
	// Admitted means the write is owed. The approval is carried out by the dispatch pass rather than
	// inside the request, so asserting it reached the executor is what distinguishes an admitted write
	// from a row left `dispatching` with nothing to run it.
	dispatchPushes(t, store)
	if len(executor.calls) != 1 {
		t.Fatalf("an admitted write reached the executor %d times, want 1", len(executor.calls))
	}
}

// TestTheCredentialGuardsRunBeforeAnyApprovalIsRecorded drives each credential refusal through the
// request face, one staged state at a time, so each code is shown to come from the rule it names.
//
// Three of these are deliberately stricter than the clone guard. A Project that names no credential is
// refused rather than treated as an anonymous push, because there is no unauthenticated write this
// product performs; a reference with no scope is refused rather than exempted, because the clone
// guard's exemption exists for a reference written before scope was part of the binding and a push has
// no such history; and a binding whose two columns disagree is refused rather than resolved, because
// silently preferring either value would authorize a write against a credential nobody chose.
func TestTheCredentialGuardsRunBeforeAnyApprovalIsRecorded(t *testing.T) {
	cases := []struct {
		name  string
		stage func(t *testing.T, store *Store, scene pushScene)
		code  string
	}{
		{
			name: "a Project that names no credential",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, `UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project)
			},
			code: "push_credential_required",
		},
		{
			name: "a credential whose row is gone",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, `UPDATE credential_refs SET deleted_at=now() WHERE id=$1`, scene.credential)
			},
			code: "repository_credential_unavailable",
		},
		{
			name: "a frozen credential",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, `UPDATE credential_refs SET availability='frozen',unavailable_reason='revoked_by_owner' WHERE id=$1`, scene.credential)
			},
			code: "repository_credential_frozen",
		},
		{
			name: "a credential scoped to another repository",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, `UPDATE projects SET repository_url=$2 WHERE id=$1`, scene.project, otherRepository)
			},
			code: "repository_credential_scope_mismatch",
		},
		{
			name: "a credential with no scope at all",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				stagedExec(t, store, `UPDATE credential_refs SET repository_scope=NULL WHERE id=$1`, scene.credential)
			},
			code: "repository_credential_scope_mismatch",
		},
		{
			name: "a Project whose two binding columns disagree",
			stage: func(t *testing.T, store *Store, scene pushScene) {
				other := stageReference(t, store, credentialedScene{tenant: scene.tenant}, scene.user, "personal")
				stagedExec(t, store, `UPDATE projects SET repository_credential_ref_id=$2 WHERE id=$1`, scene.project, other)
			},
			code: "repository_credential_binding_conflict",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := dispatcherDB(t)
			executor := &recordingPushExecutor{}
			store.SetPushExecutor(executor)
			scene := seedPushScene(t, store, `["push"]`)
			testCase.stage(t, store, scene)

			if _, _, err := scene.pushRequest(store, "key-guard", "req-guard", pushRef, pushOldCommit, pushNewCommit); err == nil {
				t.Fatalf("%s must be refused", testCase.name)
			} else if faultStatus(t, err) != 409 || faultCode(t, err) != testCase.code {
				t.Fatalf("%s must be refused as %q, got %d %q", testCase.name, testCase.code, faultStatus(t, err), faultCode(t, err))
			}
			if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 0 {
				t.Fatalf("a refused approval left %d rows behind", n)
			}
			if len(executor.calls) != 0 {
				t.Fatal("a refused approval reached the executor")
			}
		})
	}
}

// TestAPushIsRefusedWhenTheCredentialsOwnerHasLeftTheTenant is the departure rule at the request face.
// A personally attributed reference belongs to a person, and it stops being usable when that person
// leaves, regardless of who is asking: the caller here is a still-present administrator who may approve
// on the Project's behalf, and what refuses them is the reference's own owner being gone.
//
// The reference is put back to `available` after the departure so the case is about the owner's
// membership rather than about the freeze the departure trigger already wrote: with the trigger's
// result left in place, a passing test would prove only the availability rule and would keep passing
// after the membership check was deleted.
func TestAPushIsRefusedWhenTheCredentialsOwnerHasLeftTheTenant(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	// A second administrator, because the schema forbids disabling a tenant's last one.
	admin := stageMember(t, store, credentialedScene{tenant: scene.tenant})
	stagedExec(t, store, `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, scene.tenant, scene.user)
	stagedExec(t, store, `UPDATE credential_refs SET availability='available',unavailable_reason=NULL WHERE id=$1`, scene.credential)

	approver := scene
	approver.claims = callerIdentity(t, store, admin)
	if _, _, err := approver.pushRequest(store, "key-departed", "req-departed", pushRef, pushOldCommit, pushNewCommit); err == nil {
		t.Fatal("a credential whose owner has left must not authorize a push")
	} else if faultStatus(t, err) != 409 || faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("a departed owner's credential must be refused as frozen, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if len(executor.calls) != 0 || scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace) != 0 {
		t.Fatal("a refused approval reached the executor or was recorded")
	}
}

// TestARequestThatNamesAnythingButOneBranchUpdateIsRefusedAtTheBoundary is the shape half of the exact
// binding: a short branch name, a tag, a wildcard, a force prefix, a second ref, an abbreviated or
// upper-case commit, the null object id and a pair that changes nothing are each refused before any
// approval exists. A delete, a ref creation, a force update and a multi-ref push are therefore not
// expressible rather than merely unimplemented.
func TestARequestThatNamesAnythingButOneBranchUpdateIsRefusedAtTheBoundary(t *testing.T) {
	cases := []struct {
		name                      string
		ref, oldCommit, newCommit string
		code                      string
	}{
		{"a short branch name", "main", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"a tag", "refs/tags/v1.0.0", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"a wildcard", "refs/heads/*", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"a force prefix", "+refs/heads/main", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"a second ref", "refs/heads/main refs/heads/other", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"a traversal spelling", "refs/heads/../../main", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		{"an empty ref", "", pushOldCommit, pushNewCommit, "invalid_push_ref"},
		// The delete spelling: `new` as the null oid is how a ref update says "remove this ref".
		{"a delete", pushRef, pushOldCommit, pushNullCommit, "invalid_push_commit"},
		// The creation spelling: `old` as the null oid says the ref does not exist yet.
		{"a ref creation", pushRef, pushNullCommit, pushNewCommit, "invalid_push_commit"},
		{"an abbreviated commit", pushRef, pushOldCommit[:7], pushNewCommit, "invalid_push_commit"},
		{"an upper-case commit", pushRef, pushOldCommit, strings.ToUpper(pushNewCommit), "invalid_push_commit"},
		{"an empty commit", pushRef, pushOldCommit, "", "invalid_push_commit"},
		{"a pair that changes nothing", pushRef, pushOldCommit, pushOldCommit, "invalid_push_commit"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := dispatcherDB(t)
			executor := &recordingPushExecutor{}
			store.SetPushExecutor(executor)
			scene := seedPushScene(t, store, `["push"]`)
			if _, _, err := scene.pushRequest(store, "key-shape", "req-shape", testCase.ref, testCase.oldCommit, testCase.newCommit); err == nil {
				t.Fatalf("%s must be refused", testCase.name)
			} else if faultStatus(t, err) != 400 || faultCode(t, err) != testCase.code {
				t.Fatalf("%s must be refused as 400 %q, got %d %q", testCase.name, testCase.code, faultStatus(t, err), faultCode(t, err))
			}
			if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 0 {
				t.Fatalf("%s left an approval behind", testCase.name)
			}
			if len(executor.calls) != 0 {
				t.Fatalf("%s reached the executor", testCase.name)
			}
		})
	}
}

// TestOneRequestProducesOneApprovalAndOneAuthorization is the durable half of idempotency: the recorded
// plan and the identifier the credential service would consume belong to one row, and repeating the
// request — with a fresh transport key, so it is the approval's own identity that answers — reads the
// same approval back without minting a second authorization.
//
// A different input under the same request id is a conflict rather than a second row, which is what
// keeps one request id from ever naming two different writes.
func TestOneRequestProducesOneApprovalAndOneAuthorization(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedPushScene(t, store, `["push"]`)

	first, status, err := scene.pushRequest(store, "key-first", "req-1", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("the first approval must be accepted, got %v", err)
	}
	if status != 202 {
		t.Fatalf("accepting an approval must answer 202, got %d", status)
	}
	replay, _, err := scene.pushRequest(store, "key-second", "req-1", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("a repeated request must replay its approval, got %v", err)
	}
	if replay.S("id") != first.S("id") || replay.S("authorizationId") != first.S("authorizationId") {
		t.Fatalf("a repeated request produced a second approval: %v then %v", first["id"], replay["id"])
	}
	if replay.S("state") != first.S("state") || replay.S("resultCode") != first.S("resultCode") {
		t.Fatalf("a replay must read back the recorded outcome, got %v/%v", replay["state"], replay["resultCode"])
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 1 {
		t.Fatalf("one request produced %d approvals", n)
	}
	if n := scalarOf(t, store, `SELECT count(DISTINCT authorization_id) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 1 {
		t.Fatalf("one request produced %d authorizations", n)
	}

	if _, _, err := scene.pushRequest(store, "key-third", "req-1", pushRef, pushOldCommit, pushNewCommit[:39]+"d"); err == nil {
		t.Fatal("a different write under the same request id must conflict")
	} else if faultStatus(t, err) != 409 || faultCode(t, err) != "idempotency_conflict" {
		t.Fatalf("a different write under the same request id must be a conflict, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1`, scene.workspace); n != 1 {
		t.Fatalf("a conflicting request produced %d approvals", n)
	}
}

// TestTheApprovalIsPersistedBeforeAnythingIsDispatched is the ordering the decision requires: the
// executor's own call is the observation point, and at that instant the approval is already a committed
// row carrying the whole plan. A write dispatched from a plan still in memory would fail here, and so
// would one whose record was written after the fact — which is exactly the failure the ordering exists
// to prevent, because a lost reply would then leave nothing to reconcile against.
//
// The dispatch is driven by its own pass rather than by the accepting call, so the observation point is
// the pass's: the row the executor is handed was committed by a transaction that ended before this pass
// ever looked at it, which is a stronger statement than "committed before the executor was called".
func TestTheApprovalIsPersistedBeforeAnythingIsDispatched(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{outcome: PushOutcome{Verdict: PushConfirmed}}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)

	var observed Object
	executor.during = func() {
		observed = pushRow(t, store, executor.calls[len(executor.calls)-1].OperationID)
	}
	if _, _, err := scene.pushRequest(store, "key-order", "req-order", pushRef, pushOldCommit, pushNewCommit); err != nil {
		t.Fatalf("approve a push: %v", err)
	}
	dispatchPushes(t, store)
	if len(executor.calls) != 1 {
		t.Fatalf("the executor was called %d times, want 1", len(executor.calls))
	}
	if observed == nil {
		t.Fatal("the executor ran before the approval was persisted")
	}
	// The plan, field for field: the row the write is derived from is the row that was approved.
	for field, want := range map[string]string{
		"tenantId": scene.tenant, "projectId": scene.project, "workspaceId": scene.workspace,
		"actorUserId": scene.user, "repositoryUrl": admittedScope,
		"pinnedRef": pushRef, "oldCommit": pushOldCommit, "newCommit": pushNewCommit,
		"credentialRefId": scene.credential, "state": pushDispatching,
	} {
		if got := observed.S(field); got != want {
			t.Fatalf("the dispatched plan's %s is %q, want %q", field, got, want)
		}
	}
	if got := observed.N("credentialRefVersion"); got != 1 {
		t.Fatalf("the dispatched plan must pin the credential version, got %d", got)
	}
	if observed["resultCode"] != nil || observed["settledAt"] != nil {
		t.Fatal("an approval still dispatching must carry no outcome")
	}
	// It carries an authorization identity and no authorization: the record names the decision, and
	// nothing in it is a bearer that could be replayed.
	if !validID(observed.S("authorizationId")) {
		t.Fatalf("the plan must name the authorization it was approved under, got %q", observed.S("authorizationId"))
	}
}

// TestAPushApprovalSettlesWithTheDeploymentsOwnAnswerWhenNothingCanExecuteIt is the fail-closed state
// this change ships: with no executor installed, an approval is recorded and settled in one transaction
// with a deployment-level code, no authorization is signed, no remote is contacted, and no approval is
// ever left looking dispatched.
//
// The record is kept rather than refused because it is the audit the decision requires — the record of
// which authorized person asked for which write — and a refusal would leave none. What makes that safe
// is the rest of the shape: a settled `failed` row is not a success, and the identifier Cloud minted
// for it names an authorization no component ever signed.
func TestAPushApprovalSettlesWithTheDeploymentsOwnAnswerWhenNothingCanExecuteIt(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedPushScene(t, store, `["push"]`)

	out, status, err := scene.pushRequest(store, "key-no-executor", "req-no-executor", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("the approval itself must be accepted, got %v", err)
	}
	if status != 202 {
		t.Fatalf("accepting an approval must answer 202, got %d", status)
	}
	// The response already carries the terminal state, so no caller has to poll to learn that nothing
	// could execute the write.
	if out.S("state") != pushFailed || out.S("resultCode") != pushExecutorUnavailable {
		t.Fatalf("a deployment with no executor must answer failed/%s, got %v/%v", pushExecutorUnavailable, out["state"], out["resultCode"])
	}
	if out["settledAt"] == nil {
		t.Fatal("a settled approval must carry its settlement instant")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1 AND state='dispatching'`, scene.workspace); n != 0 {
		t.Fatalf("a deployment with no executor left %d approvals looking dispatched", n)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests WHERE workspace_id=$1 AND state='succeeded'`, scene.workspace); n != 0 {
		t.Fatal("a deployment with no executor recorded a success")
	}

	// The audit is readable and says exactly what happened, both by id and in the listing.
	read, status, err := scene.readPush(store, out.S("id"))
	if err != nil {
		t.Fatalf("the approval must be readable, got %v", err)
	}
	if status != 200 {
		t.Fatalf("reading one approval must answer 200, got %d", status)
	}
	if read.S("state") != pushFailed || read.S("resultCode") != pushExecutorUnavailable {
		t.Fatalf("the audit reads back %v/%v", read["state"], read["resultCode"])
	}
	listing, _, err := scene.listPushes(store)
	if err != nil {
		t.Fatalf("list the Workspace's approvals: %v", err)
	}
	items, _ := listing["items"].([]Object)
	if len(items) != 1 || items[0].S("id") != out.S("id") {
		t.Fatalf("the listing returned %v, want the one approval %s", listing["items"], out.S("id"))
	}
}

// TestTheTerminalStatesAreTheFactsAboutTheRemoteAndNothingElse drives the state machine through its
// executor, which is the only way any of these states is reached today. Each case is one scripted
// report, and what is asserted is the state the audit keeps together with the observation it was
// decided from.
//
// The five verdicts are all here rather than only the ones that look like outcomes, because the two
// uncertain rows are the ones a two-valued "did it work" would have collapsed: an observed value
// nobody can attribute and an observed value nobody authorized are different findings about the
// remote, and both are recorded as uncertain without either being recorded as a success or a failure.
// The observed commit is asserted beside each, since it is what makes the row checkable rather than
// merely declared — a conflict whose evidence was dropped would send its reader to look it up by hand.
func TestTheTerminalStatesAreTheFactsAboutTheRemoteAndNothingElse(t *testing.T) {
	cases := []struct {
		name         string
		outcome      PushOutcome
		err          error
		wantState    string
		wantCode     string
		wantObserved string
	}{
		{
			name: "confirmed and attributed", outcome: PushOutcome{Verdict: PushConfirmed, ObservedCommit: pushNewCommit},
			wantState: pushSucceeded, wantCode: pushApplied, wantObserved: pushNewCommit,
		},
		{
			name: "target satisfied but unattributable", outcome: PushOutcome{Verdict: PushTargetSatisfied, ObservedCommit: pushNewCommit},
			wantState: pushUncertain, wantCode: pushAttributionUnknown, wantObserved: pushNewCommit,
		},
		{
			name: "not applied", outcome: PushOutcome{Verdict: PushNotApplied, ObservedCommit: pushOldCommit},
			wantState: pushFailed, wantCode: pushNotApplied, wantObserved: pushOldCommit,
		},
		{
			name: "a value nobody authorized", outcome: PushOutcome{Verdict: PushTargetConflict, ObservedCommit: pushThirdCommit},
			wantState: pushUncertain, wantCode: pushTargetConflict, wantObserved: pushThirdCommit,
		},
		{
			// The unreachable-remote case: the executor answered with the state that says so, and made no
			// observation to record. It is the one verdict with no evidence, which is exactly what it means.
			name: "outcome unknown, nothing observed", outcome: PushOutcome{Verdict: PushUnknown},
			wantState: pushUncertain, wantCode: pushResultUnknown,
		},
		{
			name: "a coded refusal", err: pushRefusalError{code: "push_not_fast_forward"},
			wantState: pushFailed, wantCode: "push_not_fast_forward",
		},
		{
			name: "an unreachable executor", err: errors.New("connection refused"),
			wantState: pushUncertain, wantCode: pushResultUnknown,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store := dispatcherDB(t)
			executor := &recordingPushExecutor{outcome: testCase.outcome, err: testCase.err}
			store.SetPushExecutor(executor)
			scene := seedPushScene(t, store, `["push"]`)

			out, _, err := scene.pushRequest(store, "key-state", "req-state", pushRef, pushOldCommit, pushNewCommit)
			if err != nil {
				t.Fatalf("approve a push: %v", err)
			}
			// The accept reports the plan at acceptance — still dispatching — because the write is not run
			// in the request's lifetime. The outcome is the audit's, reached by the dispatch pass and read
			// back from the record.
			if out.S("state") != pushDispatching || out["settledAt"] != nil {
				t.Fatalf("accepting a push must report it as dispatching, got %v/%v", out["state"], out["resultCode"])
			}
			dispatchPushes(t, store)
			want := testCase.wantState + "/" + testCase.wantCode
			if got := pushState(t, store, out.S("id")); got != want {
				t.Fatalf("the audit kept %q, want %q", got, want)
			}
			if got := pushObserved(t, store, out.S("id")); got != testCase.wantObserved {
				t.Fatalf("the audit kept the observation %q, want %q", got, testCase.wantObserved)
			}
			if read, _, err := scene.readPush(store, out.S("id")); err != nil || read.S("state") != testCase.wantState {
				t.Fatalf("the audit reads back %v (%v), want %s", read["state"], err, testCase.wantState)
			}
			if len(executor.calls) != 1 {
				t.Fatalf("the executor was called %d times, want 1", len(executor.calls))
			}
			// The executor is handed the approval's plan and nothing else: the authorization it signs is
			// its own business, and Cloud hands it no material of any kind.
			call := executor.calls[0]
			if call.Repository != admittedScope || call.Ref != pushRef || call.OldCommit != pushOldCommit || call.NewCommit != pushNewCommit {
				t.Fatalf("the executor was handed a plan Cloud did not approve: %+v", call)
			}
			if len(call.Capabilities) != 1 || call.Capabilities[0] != "push" {
				t.Fatalf("the plan must carry exactly the capability the operation permits, got %v", call.Capabilities)
			}
			if call.AuthorizationID != out.S("authorizationId") || call.OperationID != out.S("id") {
				t.Fatalf("the plan's identity disagrees with the approval's: %+v", call)
			}
			if call.TenantID != scene.tenant || call.WorkspaceID != scene.workspace || call.CredentialID != scene.credential || call.CredentialVersion != 1 {
				t.Fatalf("the plan's binding disagrees with the approval's: %+v", call)
			}
		})
	}
}

// TestASettledApprovalIsNeverRewritten pins the one-write rule the settlement guard enforces. A second
// settlement — a retried dispatch, a repeated report — must write nothing, because the first recorded
// outcome is the audit's fact, and letting a later one overwrite it is how a failure becomes a success
// in the record.
func TestASettledApprovalIsNeverRewritten(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedPushScene(t, store, `["push"]`)

	out, _, err := scene.pushRequest(store, "key-settle", "req-settle", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("approve a push: %v", err)
	}
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		settlePushRequest(t, out.S("id"), pushNoAttempt, pushSucceeded, pushApplied)
		return Object{}
	}); err != nil {
		t.Fatalf("settling an already settled approval must be a no-op, got %v", err)
	}
	if got := pushState(t, store, out.S("id")); got != pushFailed+"/"+pushExecutorUnavailable {
		t.Fatalf("a settled approval was rewritten to %q", got)
	}
	// The same guard refuses the inverse direction, because it is one rule about one state rather than a
	// preference order: a settled approval cannot be put back into dispatching either. The refusal is a
	// programming error rather than a caller's, so it surfaces as a fault and leaves the row alone.
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		settlePushRequest(t, out.S("id"), pushNoAttempt, pushDispatching, pushResultUnknown)
		return Object{}
	}); err == nil {
		t.Fatal("settling an approval back into dispatching must be refused")
	}
	if got := pushState(t, store, out.S("id")); got != pushFailed+"/"+pushExecutorUnavailable {
		t.Fatalf("a refused settlement changed the row to %q", got)
	}
}

// TestTheAuditCarriesTheDecisionAndNoSecret is the audit's own shape. It asserts the exact key set a
// caller receives, so a field added later has to be considered rather than merely appearing; and it
// asserts what the record is about — the approver, the repository, the ref, both commits, the
// credential's identity and version, and a stable state with a stable reason — which is the whole of
// what the decision needs to be reconstructable.
//
// The observation is carried beside the verdict rather than inside it: `observedCommit` is what the
// executor read off the remote, and the reader re-derives the state from it instead of trusting a
// label. This approval never reached an executor, so there was nothing to read and the field is empty;
// it is present and empty rather than dropped, because a reader must be able to tell "nothing was
// observed" from "this record is from before the field existed".
//
// Nothing in it is credential material. The one field that could have been is the authorization, and
// the record holds an identifier for it rather than the bearer: the authorization itself is never
// persisted, never returned and never logged, and in a deployment with no executor it is never signed
// at all. The durable column list is asserted for the same reason: a secret persisted under any
// spelling would appear as a column this record does not have.
func TestTheAuditCarriesTheDecisionAndNoSecret(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedPushScene(t, store, `["push"]`)
	out, _, err := scene.pushRequest(store, "key-audit", "req-audit", pushRef, pushOldCommit, pushNewCommit)
	if err != nil {
		t.Fatalf("approve a push: %v", err)
	}

	keys := make([]string, 0, len(out))
	for key := range out {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := strings.Fields("actorUserId authorizationId createdAt credentialRefId credentialRefVersion id newCommit observedCommit oldCommit projectId ref repository requestId resultCode settledAt state tenantId updatedAt workspaceId")
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("the audit carries\n%v\nwant\n%v", keys, want)
	}
	for field, value := range map[string]string{
		"actorUserId": scene.user, "tenantId": scene.tenant, "projectId": scene.project,
		"workspaceId": scene.workspace, "requestId": "req-audit", "repository": admittedScope,
		"ref": pushRef, "oldCommit": pushOldCommit, "newCommit": pushNewCommit,
		"credentialRefId": scene.credential, "state": pushFailed, "resultCode": pushExecutorUnavailable,
		"observedCommit": "",
	} {
		if got := out.S(field); got != value {
			t.Fatalf("the audit's %s is %q, want %q", field, got, value)
		}
	}
	if !validID(out.S("authorizationId")) {
		t.Fatalf("the audit must name the authorization it minted, got %q", out.S("authorizationId"))
	}
	var columns string
	// The schema predicate is load-bearing: information_schema spans every schema the role can see, not
	// just the one on search_path, so without it a same-named table in a concurrently running test's
	// schema joins the list and the assertion reports columns this record does not have.
	if err := store.Pool.QueryRow(`SELECT string_agg(column_name::text, ',' ORDER BY column_name) FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='push_requests'`).Scan(&columns); err != nil {
		t.Fatalf("read the push approval's columns: %v", err)
	}
	// The list is asserted whole rather than filtered, so it has to be extended with every column the
	// record gains. The two dispatch columns are Cloud's own bookkeeping about its attempts — a count and
	// a deadline, both scalars about when this process may try — and neither holds anything read from the
	// remote, handed to the executor, or derived from credential material.
	if columns != "actor_user_id,authorization_id,created_at,credential_ref_id,credential_ref_version,dispatch_generation,dispatch_lease_expires_at,id,new_commit,observed_commit,old_commit,pinned_ref,project_id,repository_url,request_id,result_code,settled_at,state,tenant_id,updated_at,workspace_id" {
		t.Fatalf("the durable record's columns are\n%s", columns)
	}
}

// TestAnotherTenantsWorkspaceIsAbsentRatherThanForbidden pins the tenant scope of both routes. The
// caller is an administrator of two tenants and names the other tenant's path with this tenant's
// Workspace id, so the shared membership gate passes and the only thing left to refuse the request is
// the Workspace lookup's own tenant predicate. The answer is the one an absent Workspace gets, which is
// what keeps the route from being usable to learn whether an identifier belongs to somebody else.
func TestAnotherTenantsWorkspaceIsAbsentRatherThanForbidden(t *testing.T) {
	store := dispatcherDB(t)
	executor := &recordingPushExecutor{}
	store.SetPushExecutor(executor)
	scene := seedPushScene(t, store, `["push"]`)
	other, _, _ := bootstrapCaller(t, store, "other-tenant")
	stagedExec(t, store, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, other, scene.user)

	if _, _, err := scene.pushRequestIn(store, other, "key-cross", "req-cross"); err == nil {
		t.Fatal("a Workspace must not be reachable through another tenant's path")
	} else if faultStatus(t, err) != 404 || faultCode(t, err) != "not_found" {
		t.Fatalf("a cross-tenant approval must be answered as absent, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if _, _, err := store.Public(context.Background(), &PublicRequest{
		Method: "GET", Path: "/api/v1/tenants/" + other + "/workspaces/" + scene.workspace + "/pushes",
		TenantID: other, WorkspaceID: scene.workspace, Identity: scene.claims,
	}); err == nil {
		t.Fatal("a Workspace's approvals must not be reachable through another tenant's path")
	} else if faultStatus(t, err) != 404 || faultCode(t, err) != "not_found" {
		t.Fatalf("a cross-tenant read must be answered as absent, got %d %q", faultStatus(t, err), faultCode(t, err))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM push_requests`); n != 0 {
		t.Fatalf("a cross-tenant request wrote %d approvals", n)
	}
	if len(executor.calls) != 0 {
		t.Fatal("a cross-tenant request reached the executor")
	}
}
