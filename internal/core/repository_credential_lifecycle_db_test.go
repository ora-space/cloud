package core

// This file pins the obligations the approved credential decisions state about the *lifecycle* of a
// credential — what withdrawal and rotation do to new clone bindings, to executions already accepted,
// and to the durable records both leave behind — plus the caller side of the boundary and what happens
// when the handoff to the Credential Service cannot be completed. Every DB-backed scene runs against
// real PostgreSQL on the shipped seams (`Store.WriteGitCredential`, `Store.Control`), following the
// conventions of the other `*_db_test.go` files in this package.
//
// The gaps it fills, each stated where its test is:
//
//   - revocation, driven through the real revoke API rather than by staging `availability`, closes new
//     clone bindings while leaving an already-accepted execution and its settlement untouched;
//   - rotation advances the credential's own version without rewriting the version an accepted
//     execution recorded, and that execution still settles under the version it was admitted with;
//   - the credential view is scoped by the caller's tenant *and* owner, so one caller can neither
//     observe nor mutate another's row;
//   - every credential mutation requires an active membership of the owning tenant and writes nothing
//     when it is refused;
//   - a create whose handoff succeeded but whose metadata commit failed reports failure rather than
//     inferring success, and its retry reconciles onto the same derived identity instead of sealing a
//     second credential;
//   - a departure freezes its reference as a terminal state rather than as a cache of the membership
//     that ended, so a rejoined member's reference stays frozen and the recovery scan leaves it alone;
//   - the applied migrations that carry credential semantics are byte-for-byte immutable.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
)

// fakeGitToken is fixed, obviously synthetic material. No test in this file accepts, logs or persists a
// real secret: the value could not authenticate anywhere, and the credential service is a double that
// keeps it only in this process's memory.
const fakeGitToken = "ghp_synthetic-lifecycle-0000000000000000"

// lifecycleSecrets is the Credential Service's ingress as a test double for the lifecycle scenes. It
// answers a repeated write for the same credential id with the reference it already holds — the
// idempotency store D4 requires, and the property Cloud's retry path is built on — and counts each kind
// of handoff so a test can assert that a refused mutation never reached it.
type lifecycleSecrets struct {
	mu       sync.Mutex
	refs     map[string]string
	material map[string]string
	writeIDs []string
	rotates  int
	revokes  int
	fail     error
}

func (s *lifecycleSecrets) Write(_ context.Context, credentialID, token string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return "", s.fail
	}
	if s.refs == nil {
		s.refs, s.material = map[string]string{}, map[string]string{}
	}
	s.writeIDs = append(s.writeIDs, credentialID)
	if ref, ok := s.refs[credentialID]; ok {
		return ref, nil
	}
	ref := "cs_" + credentialID
	s.refs[credentialID], s.material[credentialID] = ref, token
	return ref, nil
}

func (s *lifecycleSecrets) Rotate(_ context.Context, credentialID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.rotates++
	if s.material == nil {
		s.material = map[string]string{}
	}
	s.material[credentialID] = token
	return nil
}

func (s *lifecycleSecrets) Revoke(_ context.Context, credentialID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.revokes++
	delete(s.material, credentialID)
	return nil
}

func (s *lifecycleSecrets) reference(credentialID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[credentialID]
}

func (s *lifecycleSecrets) handoffs() (writes, rotates, revokes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writeIDs), s.rotates, s.revokes
}

func (s *lifecycleSecrets) writeIdentities() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.writeIDs...)
}

// callerIdentity attaches a verifiable identity to a user the scene helpers seeded directly. Those
// helpers write no `user_identities` row, and the credential boundary resolves the caller only from
// verified claims against current state, so the identity is added here to make the caller exactly the
// row the scene owns.
func callerIdentity(t *testing.T, store *Store, user string) *Claims {
	t.Helper()
	source, subject := "lifecycle-caller", "caller-"+user
	stagedExec(t, store, `INSERT INTO user_identities(user_id,source,subject) VALUES($1,$2,$3)`, user, source, subject)
	return &Claims{Source: source, Subject: subject, DisplayName: "caller"}
}

// bootstrapCaller provisions one tenant, its administrator and its space the way the deployment does,
// and returns the tenant, the user and the claims that resolve to that user.
func bootstrapCaller(t *testing.T, store *Store, label string) (tenant, user string, claims *Claims) {
	t.Helper()
	source := "lifecycle-bootstrap"
	subject := label + "-" + newID()
	out, err := store.Bootstrap(context.Background(), "lifecycle", source, subject, "Lifecycle")
	if err != nil {
		t.Fatalf("bootstrap %s: %v", label, err)
	}
	return out.S("tenantId"), out.S("userId"), &Claims{Source: source, Subject: subject, DisplayName: "Lifecycle"}
}

// stageOwnedCredential writes one available personal credential for an owner without going through the
// ingress, for the state a test needs before it exercises a boundary.
func stageOwnedCredential(t *testing.T, store *Store, tenant, owner string) string {
	t.Helper()
	credential := newID()
	stagedExec(t, store, `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,secret_ref,name,attribution,attribution_basis,availability,repository_scope,capabilities)
		VALUES($1,$2,$3,'git',$4,'staged','personal','user_stated','available',$5,'["clone"]'::jsonb)`,
		credential, tenant, owner, "credential-ref/"+credential, admittedScope)
	return credential
}

// credentialViewFault runs the one credential-lookup rule through its own transaction and reports the
// refusal code, or the empty string when the caller may read the row.
func credentialViewFault(t *testing.T, store *Store, tenant, user, id string) string {
	t.Helper()
	_, err := store.transact(context.Background(), func(tx *transaction) Object {
		return gitCredentialView(tx, tenant, user, id)
	})
	if err == nil {
		return ""
	}
	return faultCode(t, err)
}

// seedRecordedCredentialedExecution writes one clone execution exactly as an accepted credentialed
// clone leaves it: bound to the concrete credential version it ran with (D4), with the outcome already
// recorded. It is the durable fact a later revocation or rotation must not rewrite.
func seedRecordedCredentialedExecution(t *testing.T, store *Store, scene credentialedScene, execution string) {
	t.Helper()
	record := jsonText(Object{"outcome": cloneReadyResult, "node": Object{"nodeId": scene.node}, "commit": baseCommit})
	stagedExec(t, store, `INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id,result,credential_ref_id,credential_ref_version)
		VALUES($1,$2,$3,$4,'{"kind":"clone"}',1,$1::text,$5::jsonb,$6,1)`,
		execution, scene.operation, scene.workspace, scene.node, record, scene.credential)
}

// stageFillerAdmin adds one active administrator to a tenant, which the schema requires before the
// scene's own administrator can be disabled: leaving a tenant with no active administrator violates
// the deferred `last_admin` guard, so a scene that disables an administrator must have a second one.
func stageFillerAdmin(t *testing.T, store *Store, tenant string) {
	t.Helper()
	filler := newID()
	stagedExec(t, store, `INSERT INTO users(id,display_name,status) VALUES($1,'filler','active')`, filler)
	stagedExec(t, store, `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, tenant, filler)
}

// credentialMutationRequest builds one credential-ingress mutation as the scene's owner.
func credentialMutationRequest(method, tenant, credentialID string, claims *Claims, key string, version int64) *PublicRequest {
	return &PublicRequest{
		Method:       method,
		Path:         "/api/v1/tenants/" + tenant + "/git-credentials/" + credentialID,
		TenantID:     tenant,
		CredentialID: credentialID,
		Key:          key,
		Body:         Object{"version": version},
		Identity:     claims,
	}
}

// TestRevokingACredentialLeavesTheAcceptedExecutionAndItsSettlementIntact is the history-preservation
// half of the revocation question (credential decision D5 and invariant 6): a withdrawal that is
// expressed by advancing the credential's own state must not rewrite the credential's recorded
// history, and it must not touch — or block the settlement of — an execution that was already
// accepted. The settlement half is the reason this is not cosmetic: `advanceClone` never consults the
// guard, so an accepted clone still becomes the Workspace's baseline after its credential is gone.
//
// The revocation is driven through the real API rather than staged, so the row the guard reads is the
// one the product path produced. Every refusal the guard could give for this scene is
// `repository_credential_frozen`, which is deliberately *not* one of the terminal refusals, so the
// Operation stays claimable and the already-accepted execution can still be settled.
func TestRevokingACredentialLeavesTheAcceptedExecutionAndItsSettlementIntact(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	store.SetCredentialedCloner(&recordingCloner{})
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	// An execution accepted and finished before the revocation, bound to credential version 1.
	execution := "exec-accepted-" + scene.workspace[:8]
	seedRecordedCredentialedExecution(t, store, scene, execution)
	before, beforeStamp := cloneExecutionWitness(t, store, execution)

	claims := callerIdentity(t, store, scene.user)
	revoked, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "revoke-lifecycle", 1), "")
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if revoked.Status != 200 || revoked.Resource.S("availability") != "invalid" || revoked.Resource.S("unavailableReason") != "revoked_by_owner" {
		t.Fatalf("withdrawal is expressed through availability: %+v", revoked.Resource)
	}
	// The row keeps its history: revocation is a state change, never a deletion, and the store material
	// is destroyed separately rather than by rewriting the row. An empty secret_ref here would mean the
	// reference that locates the material had been erased to express the revocation.
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='invalid' AND unavailable_reason='revoked_by_owner' AND revoked_at IS NOT NULL AND deleted_at IS NULL AND version=2 AND secret_ref='credential-ref/1'`, scene.credential); n != 1 {
		t.Fatal("revocation rewrote or erased the credential's recorded history instead of withdrawing it")
	}
	if _, _, revokes := secrets.handoffs(); revokes != 1 {
		t.Fatalf("revocation must destroy the material exactly once, got %d", revokes)
	}

	// The accepted execution's recorded facts survive the withdrawal byte for byte, including the
	// version it ran with.
	after, afterStamp := cloneExecutionWitness(t, store, execution)
	if string(after) != string(before) || afterStamp != beforeStamp {
		t.Fatalf("revocation rewrote the accepted execution's recorded facts: %s/%s -> %s/%s", before, beforeStamp, after, afterStamp)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE execution_id=$1 AND credential_ref_version=1`, execution); n != 1 {
		t.Fatal("revocation rewrote the version the accepted execution recorded")
	}

	// Finishing an accepted execution is not new remote Git work: the guard is not consulted, so the
	// Workspace's baseline still advances from the clone that already ran.
	version := scalarOf(t, store, `SELECT version FROM operations WHERE id=$1`, scene.operation)
	if _, err := driveControl(t, store, "advance", Object{"operationId": scene.operation, "version": version}); err != nil {
		t.Fatalf("an accepted execution must still settle after revocation, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2`, scene.workspace, baseCommit); n != 1 {
		t.Fatal("revocation stopped the accepted clone's settlement from advancing the Workspace baseline")
	}
	if state := textOf(t, store, `SELECT state FROM operations WHERE id=$1`, scene.operation); state != "succeeded" {
		t.Fatalf("the accepted execution's operation must settle to succeeded, got %q", state)
	}
}

// TestRevokingACredentialClosesNewCloneBindings is the other half of the revocation question: after the
// withdrawal, a *new* clone binding must be refused, and it must be refused before anything is
// registered or handed to an executor. The executor is installed, so an admission that slipped past
// the guard would be visibly executed and registered here — the refusal is the revocation's doing, not
// a missing executor's — and the scene carries no prior execution, so nothing but the guard stands
// between the dispatch and the executor.
//
// The code is the revocation's own rather than `repository_credential_frozen`, and that is what the
// product's delete button changes about the guard's answer: a revoked reference is destroyed, not
// paused, so the refusal is one of the terminal ones and the Operation settles instead of retrying.
func TestRevokingACredentialClosesNewCloneBindings(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	cloner := &recordingCloner{}
	store.Secrets = secrets
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	claims := callerIdentity(t, store, scene.user)
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, scene.credential, claims, "revoke-lifecycle", 1), ""); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if _, err := scene.dispatch(store, "exec-after-revoke"); faultCode(t, err) != repositoryCredentialRevoked {
		t.Fatalf("a revoked credential must refuse a new clone binding as revoked, got %v", err)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a revoked credential reached the executor %d times", len(cloner.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE execution_id=$1`, "exec-after-revoke"); n != 0 {
		t.Fatal("a refused clone after revocation registered an execution")
	}
}

// TestRotatingACredentialLeavesTheAcceptedExecutionsBindingAndSettlementIntact is the other half of the
// per-execution freeze (D4): a rotation advances the credential's own version, and an execution
// accepted under the previous version keeps that version and settles with it. Rewriting the execution's
// version instead would make "which credential did this clone use" unanswerable exactly when a rotation
// makes it interesting, which the decision forbids.
func TestRotatingACredentialLeavesTheAcceptedExecutionsBindingAndSettlementIntact(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	store.SetCredentialedCloner(&recordingCloner{})
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)

	execution := "exec-rotating-" + scene.workspace[:8]
	seedRecordedCredentialedExecution(t, store, scene, execution)
	before, _ := cloneExecutionWitness(t, store, execution)

	claims := callerIdentity(t, store, scene.user)
	rotated, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("PUT", scene.tenant, scene.credential, claims, "", 1), fakeGitToken)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if rotated.Status != 200 || rotated.Resource.N("version") != 2 {
		t.Fatalf("a rotation must advance the credential's own version: %+v", rotated.Resource)
	}
	if _, rotates, _ := secrets.handoffs(); rotates != 1 {
		t.Fatalf("a rotation must replace the material exactly once, got %d", rotates)
	}

	// The accepted execution still records version 1, and nothing about its row moved.
	if after, _ := cloneExecutionWitness(t, store, execution); string(after) != string(before) {
		t.Fatalf("rotation rewrote the accepted execution's recorded binding: %s -> %s", before, after)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE execution_id=$1 AND credential_ref_version=1`, execution); n != 1 {
		t.Fatal("rotation changed the version the accepted execution recorded; it must settle under the version it was admitted with")
	}

	// It settles against that version: the credential's version is now 2, and the execution is still
	// the fact the Workspace's baseline is advanced from.
	version := scalarOf(t, store, `SELECT version FROM operations WHERE id=$1`, scene.operation)
	if _, err := driveControl(t, store, "advance", Object{"operationId": scene.operation, "version": version}); err != nil {
		t.Fatalf("an execution accepted before a rotation must still settle, got %v", err)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM workspaces WHERE id=$1 AND base_commit_id=$2`, scene.workspace, baseCommit); n != 1 {
		t.Fatal("a rotation stopped the accepted clone's settlement from advancing the Workspace baseline")
	}
}

// TestACredentialIsScopedToItsOwnersTenantAndOwner pins that ownership is re-derived from verified
// claims and current state on every path, never from the credential id a caller names. A credential id
// is opaque and carries no authority: a caller in one tenant cannot observe or mutate another owner's
// row, nor a row in another tenant, and a refused attempt never reaches the store and leaves the named
// row exactly as it was.
func TestACredentialIsScopedToItsOwnersTenantAndOwner(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	claims := callerIdentity(t, store, scene.user)

	// Another owner inside the same tenant, and a credential of an entirely different tenant.
	otherOwner := stageMember(t, store, scene)
	otherOwnersCredential := stageReference(t, store, scene, otherOwner, "personal")
	otherTenant, otherUser, otherClaims := bootstrapCaller(t, store, "other")
	foreignCredential := stageOwnedCredential(t, store, otherTenant, otherUser)

	// Observation: the view is scoped by tenant *and* owner, so neither row is readable by this caller.
	if code := credentialViewFault(t, store, scene.tenant, scene.user, otherOwnersCredential); code != "not_found" {
		t.Fatalf("another owner's credential must read as absent, got %q", code)
	}
	if code := credentialViewFault(t, store, scene.tenant, scene.user, foreignCredential); code != "not_found" {
		t.Fatalf("another tenant's credential named under this tenant must read as absent, got %q", code)
	}
	// A negative control on the same rule: the owner does read it.
	if code := credentialViewFault(t, store, otherTenant, otherUser, foreignCredential); code != "" {
		t.Fatalf("the owning caller must be able to read its own credential, got %q", code)
	}
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("PUT", scene.tenant, otherOwnersCredential, claims, "", 1), fakeGitToken); faultCode(t, err) != "not_found" {
		t.Fatalf("rotating another owner's credential must be refused as absent, got %v", err)
	}
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", scene.tenant, otherOwnersCredential, claims, "cross-owner", 1), ""); faultCode(t, err) != "not_found" {
		t.Fatalf("revoking another owner's credential must be refused as absent, got %v", err)
	}
	// Naming the other tenant itself is refused one step earlier: no membership there.
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", otherTenant, foreignCredential, claims, "cross-tenant", 1), ""); faultCode(t, err) != "membership_required" {
		t.Fatalf("a caller cannot act in a tenant it does not belong to, got %v", err)
	}
	// The owning caller's own row still works, so the refusals are about attribution rather than a
	// boundary that refuses everything.
	if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", otherTenant, foreignCredential, otherClaims, "owner", 1), ""); err != nil {
		t.Fatalf("the owning caller must be able to withdraw its own credential: %v", err)
	}

	// No refused attempt reached the store, and neither named row was disturbed.
	if writes, rotates, _ := secrets.handoffs(); writes != 0 || rotates != 0 {
		t.Fatalf("a refused credential mutation reached the store: writes=%d rotates=%d", writes, rotates)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='available' AND version=1`, otherOwnersCredential); n != 1 {
		t.Fatal("a refused mutation disturbed the credential it named")
	}
}

// TestCredentialMutationsRequireAnActiveMembershipOfTheOwningTenant is the caller side of the ingress
// rule: only an authenticated and currently-authorized end user may write, and only its own
// credentials. A membership or user that has been disabled, and a caller who never belonged to the
// tenant, are each refused before any write, and the refusal leaves no metadata and no handoff.
func TestCredentialMutationsRequireAnActiveMembershipOfTheOwningTenant(t *testing.T) {
	// Each case names the refusal it must produce, because the two fails happen at different steps on
	// the way in: a disabled user is refused when the verified identity is resolved into current state
	// (`user_disabled`), while a still-active user whose membership was withdrawn is refused one step
	// later at the tenant boundary (`membership_required`). Pinning each exact code keeps a refusal
	// from silently moving earlier or later on the path.
	refused := []struct {
		name    string
		code    string
		disable func(t *testing.T, store *Store, tenant, user string)
	}{
		{
			name: "a disabled membership", code: "membership_required",
			disable: func(t *testing.T, store *Store, tenant, user string) {
				stageFillerAdmin(t, store, tenant)
				stagedExec(t, store, `UPDATE tenant_memberships SET status='disabled' WHERE tenant_id=$1 AND user_id=$2`, tenant, user)
			},
		},
		{
			name: "a disabled user", code: "user_disabled",
			disable: func(t *testing.T, store *Store, tenant, user string) {
				stageFillerAdmin(t, store, tenant)
				stagedExec(t, store, `UPDATE users SET status='disabled' WHERE id=$1`, user)
			},
		},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			secrets := &lifecycleSecrets{}
			store.Secrets = secrets
			tenant, user, claims := bootstrapCaller(t, store, "perm")
			credential := stageOwnedCredential(t, store, tenant, user)
			c.disable(t, store, tenant, user)

			// Disabling the owner may itself have frozen the row (the 0023 trigger). Capture what the
			// refused mutations must not change, rather than an assumed pre-state.
			state := textOf(t, store, `SELECT availability||'/'||COALESCE(unavailable_reason,'')||'/'||version::text FROM credential_refs WHERE id=$1`, credential)

			create := &PublicRequest{
				Method: "POST", Path: "/api/v1/tenants/" + tenant + "/git-credentials",
				TenantID: tenant, Key: "perm-create", Body: Object{"name": "work", "repositoryScope": admittedScope}, Identity: claims,
			}
			if _, err := store.WriteGitCredential(context.Background(), create, fakeGitToken); faultCode(t, err) != c.code {
				t.Fatalf("a create by an unauthorized caller must be refused as %s, got %v", c.code, err)
			}
			if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("PUT", tenant, credential, claims, "", 1), fakeGitToken); faultCode(t, err) != c.code {
				t.Fatalf("a rotate by an unauthorized caller must be refused as %s, got %v", c.code, err)
			}
			if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", tenant, credential, claims, "perm-revoke", 1), ""); faultCode(t, err) != c.code {
				t.Fatalf("a revoke by an unauthorized caller must be refused as %s, got %v", c.code, err)
			}

			if writes, rotates, revokes := secrets.handoffs(); writes != 0 || rotates != 0 || revokes != 0 {
				t.Fatalf("a refused mutation reached the store: writes=%d rotates=%d revokes=%d", writes, rotates, revokes)
			}
			if after := textOf(t, store, `SELECT availability||'/'||COALESCE(unavailable_reason,'')||'/'||version::text FROM credential_refs WHERE id=$1`, credential); after != state {
				t.Fatalf("a refused mutation changed the credential: %s -> %s", state, after)
			}
			if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND owner_user_id=$2`, tenant, user); n != 1 {
				t.Fatalf("a refused create wrote metadata for an unauthorized caller: %d rows", n)
			}
			if n := scalarOf(t, store, `SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, tenant, user, "perm-create"); n != 0 {
				t.Fatal("a refused create recorded an idempotency outcome")
			}
		})
	}

	t.Run("a caller who never belonged to the tenant", func(t *testing.T) {
		store := dispatcherDB(t)
		secrets := &lifecycleSecrets{}
		store.Secrets = secrets
		_, _, outsider := bootstrapCaller(t, store, "outsider")
		tenant, user, _ := bootstrapCaller(t, store, "holding")
		credential := stageOwnedCredential(t, store, tenant, user)

		if _, err := store.WriteGitCredential(context.Background(), credentialMutationRequest("DELETE", tenant, credential, outsider, "not-a-member", 1), ""); faultCode(t, err) != "membership_required" {
			t.Fatalf("a non-member must be refused at the tenant boundary, got %v", err)
		}
		if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='available' AND version=1`, credential); n != 1 {
			t.Fatal("a non-member's refused mutation disturbed the credential")
		}
		if _, _, revokes := secrets.handoffs(); revokes != 0 {
			t.Fatal("a non-member's refused mutation reached the store")
		}
	})
}

// TestACredentialCreateThatLosesItsMetadataCommitReconcilesByTheSameIdentity covers the ordering the
// ingress decision relies on when the handoff succeeds and Cloud's own metadata commit does not. The
// retry after such a failure must reproduce the same derived identity, adopt the orphan material the
// store already holds and commit exactly one row — it must never read the orphan as a success, and it
// must never seal a second credential.
//
// The failure is reproduced without a fault-injection seam: the derived id is known before the row
// exists, so occupying it makes the metadata insert fail exactly as a lost commit would, and the
// collision is then cleared for the retry.
func TestACredentialCreateThatLosesItsMetadataCommitReconcilesByTheSameIdentity(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{}
	store.Secrets = secrets
	tenant, user, claims := bootstrapCaller(t, store, "recover")
	key := "recover-" + newID()
	id := gitCredentialID(tenant, user, key)

	// Occupy the id the handoff will use, so the metadata insert after a successful handoff fails. The
	// occupant is a real row of this owner, moved onto the derived id.
	occupant := stageOwnedCredential(t, store, tenant, user)
	stagedExec(t, store, `UPDATE credential_refs SET id=$2 WHERE id=$1`, occupant, id)

	create := func() (GitCredentialResult, error) {
		return store.WriteGitCredential(context.Background(), &PublicRequest{
			Method: "POST", Path: "/api/v1/tenants/" + tenant + "/git-credentials",
			TenantID: tenant, Key: key, Body: Object{"name": "recovering", "repositoryScope": admittedScope}, Identity: claims,
		}, fakeGitToken)
	}

	if _, err := create(); err == nil {
		t.Fatal("a create whose metadata commit failed must report failure rather than infer success")
	}
	// The failed attempt recorded nothing, so no later reader can mistake it for a completed create.
	if n := scalarOf(t, store, `SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, tenant, user, key); n != 0 {
		t.Fatal("a create that failed after the handoff recorded an idempotency outcome")
	}
	if got := secrets.writeIdentities(); len(got) != 1 || got[0] != id {
		t.Fatalf("the handoff must use the derived identity, got %v want [%s]", got, id)
	}

	// Clear the collision — the cause of the failed commit — and retry under the same key.
	stagedExec(t, store, `DELETE FROM credential_refs WHERE id=$1`, id)
	result, err := create()
	if err != nil {
		t.Fatalf("the retry under the same key must complete, got %v", err)
	}
	if result.Status != 201 || result.Resource.S("id") != id {
		t.Fatalf("the retry must reconcile onto the same credential identity: %+v", result.Resource)
	}
	if ref := textOf(t, store, `SELECT secret_ref FROM credential_refs WHERE id=$1`, id); ref != secrets.reference(id) {
		t.Fatalf("the retry wrote a reference other than the one the store already held: %q vs %q", ref, secrets.reference(id))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND owner_user_id=$2`, tenant, user); n != 1 {
		t.Fatalf("the retry sealed a second credential: %d rows", n)
	}
	if got := secrets.writeIdentities(); len(got) != 2 || got[1] != id {
		t.Fatalf("the retry must re-derive the same identity, got %v", got)
	}
}

// TestACredentialCreateFailsClosedWhenTheStoreCannotBeReached is the other failure path: when the
// handoff itself fails, the create is refused as a whole, no metadata is written, nothing is recorded,
// and the failure is never read as a success.
func TestACredentialCreateFailsClosedWhenTheStoreCannotBeReached(t *testing.T) {
	store := dispatcherDB(t)
	secrets := &lifecycleSecrets{fail: errors.New("dial tcp 10.0.0.9:8443: connection refused")}
	store.Secrets = secrets
	tenant, user, claims := bootstrapCaller(t, store, "closed")
	key := "closed-" + newID()

	_, err := store.WriteGitCredential(context.Background(), &PublicRequest{
		Method: "POST", Path: "/api/v1/tenants/" + tenant + "/git-credentials",
		TenantID: tenant, Key: key, Body: Object{"name": "work", "repositoryScope": admittedScope}, Identity: claims,
	}, fakeGitToken)
	if err == nil {
		t.Fatal("an unreachable credential store must fail the create, not succeed")
	}
	fault := ErrorCode(err)
	if fault.Code != "credential_store_unavailable" || fault.Status != 503 {
		t.Fatalf("an unreachable store must fail the create closed, got %s/%d", fault.Code, fault.Status)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE tenant_id=$1 AND owner_user_id=$2`, tenant, user); n != 0 {
		t.Fatal("a failed handoff wrote credential metadata")
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM idempotency_records WHERE tenant_id=$1 AND user_id=$2 AND key=$3`, tenant, user, key); n != 0 {
		t.Fatal("a failed handoff recorded an idempotency outcome")
	}
}

// TestTheAppliedCredentialMigrationsAreByteForByteImmutable pins the other meaning of "history is not
// rewritten": the migrations that carry credential semantics are applied state, and their checksums are
// verified on every startup, so editing one would make a deployed database refuse to migrate. The
// digests are pinned here so such an edit fails this test before it can reach a deployment; a forward
// change adds a new migration instead.
func TestTheAppliedCredentialMigrationsAreByteForByteImmutable(t *testing.T) {
	known := map[string]string{
		"0001_core.sql": "2de559ceab8ec42a2d784aed04072946e5d3143bd05709399319a803986cf0ee",
		"0023_repository_credential_continuity.sql": "12c936a2f02be7bc65e4b526457e03b5357c071475f6d4c69da1ef8504a899c1",
		"0033_user_git_credentials.sql":             "a7f28d9e87b3c4648b10c40cafe12f98dea12979c5e4b5d6d6a4d25e26873c6e",
	}
	for name, digest := range known {
		raw, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read applied migration %s: %v", name, err)
		}
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != digest {
			t.Fatalf("applied migration %s was edited: digest %s, want %s; add a forward migration instead", name, got, digest)
		}
	}
}

// TestARejoinedMemberDoesNotUnfreezeTheReferenceThatLeftWithThem is the direct evidence that freezing is
// a recorded terminal state, not a cache of "the owner is not an active member".
//
// The order is what makes it discriminating. The departure runs first, so the `0023` trigger writes the
// freeze — and the version it bumps is read back, because a refusal alone would also come from a row
// that was never frozen. Only then does the member come back, which means that at the second dispatch
// every relationship the departure predicate reads is satisfied again: the membership is active, the
// user is active, nothing is soft-deleted. An implementation that *derived* frozen from the membership
// would clear the condition and admit the clone; this one still refuses, and refuses with the
// reference's own code rather than the deployment's, so the operator is told the reference is unusable
// rather than that the deployment cannot clone.
//
// The recovery scan is driven directly as well. It is the second writer of this column and it runs on a
// real path (`runtime_control.go`), so a scan that recomputed availability rather than only ever
// freezing would undo the trigger's work here, and no other test in the suite would notice.
//
// The last step is the control that keeps the earlier ones honest: the freeze is cleared and the same
// dispatch is run again, where it is admitted. Without that step a rejoin which silently failed to take
// effect would produce the identical refusal, and the test would pass while proving nothing about
// freezing — every assertion would be satisfied by the owner-absent rule instead.
func TestARejoinedMemberDoesNotUnfreezeTheReferenceThatLeftWithThem(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	stageDeparture(t, store, scene, "personal")

	frozen := scalarOf(t, store, `SELECT version FROM credential_refs WHERE id=$1`, scene.credential)
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND unavailable_reason='associated_member_departed'`, scene.credential); n != 1 {
		t.Fatal("the departure did not freeze the reference, so a rejoin here would prove nothing")
	}

	// The member rejoins. Nothing else moves: the Project still names the reference, and the reference
	// still names the repository the Project asks for, so the only thing left to refuse the clone is the
	// freeze itself.
	stagedExec(t, store, `UPDATE tenant_memberships SET status='active' WHERE tenant_id=$1 AND user_id=$2`, scene.tenant, scene.user)
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		freezeInactiveRepositoryCredentials(t)
		return Object{}
	}); err != nil {
		t.Fatalf("run the departure recovery scan after the rejoin: %v", err)
	}

	if _, err := scene.dispatch(store, "exec-rejoined"); faultCode(t, err) != "repository_credential_frozen" {
		t.Fatalf("a rejoined member's reference must stay frozen, got %v", err)
	}
	if got := scalarOf(t, store, `SELECT version FROM credential_refs WHERE id=$1`, scene.credential); got != frozen {
		t.Fatalf("the rejoin moved the frozen reference's version from %d to %d", frozen, got)
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM credential_refs WHERE id=$1 AND availability='frozen' AND unavailable_reason='associated_member_departed'`, scene.credential); n != 1 {
		t.Fatal("the rejoin cleared the freeze or replaced the reason that records why it was frozen")
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a refused reference reached the executor %d times", len(cloner.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 0 {
		t.Fatalf("a refused credentialed clone left %d clone executions behind", n)
	}

	// The control that keeps the assertions above from passing for the wrong reason: the rejoin has to be
	// real. The executor is installed, so once nothing refuses, the clone is admitted. Clearing the
	// freeze is therefore the one change that should flip the outcome, and flipping it shows the
	// membership predicate really is satisfied again — which is what makes the `repository_credential_frozen`
	// above the work of the recorded freeze rather than of the owner-absent rule. A rejoin that silently
	// did not take effect would leave the owner-absent rule refusing, and the test would have proved
	// nothing about freezing being terminal.
	stagedExec(t, store, `UPDATE credential_refs SET availability='available',unavailable_reason=NULL WHERE id=$1`, scene.credential)
	if _, err := scene.dispatch(store, "exec-rejoined-unfrozen"); err != nil {
		t.Fatalf("after the rejoin only the recorded freeze was refusing, so clearing it must admit the clone, got %v", err)
	}
	if len(cloner.calls) != 1 {
		t.Fatalf("the admitted clone reached the executor %d times, want 1", len(cloner.calls))
	}
	if n := scalarOf(t, store, `SELECT count(*) FROM clone_executions WHERE workspace_id=$1`, scene.workspace); n != 1 {
		t.Fatalf("the admitted clone recorded %d executions, want 1", n)
	}
}
