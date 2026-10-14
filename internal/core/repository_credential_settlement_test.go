package core

// The refusal of a credentialed clone is a deployment fact Cloud cannot reverse, so it is also the
// refused Operation's outcome. These tests pin that settlement at the layer a Controller reaches it —
// a real `clone_dispatch` through `Store.Control` against a real schema — and pin everything it must
// not do: it must not fire for any other refusal, must not write over a newer Controller, must be a
// no-op the second time, and must not leave the Operation holding the claim the Project's next
// Operation needs.
//
// The claim half is deliberately differential. The previous scheduling fix was tested by claiming
// seeded `queued` Operations once each, which is not the shape of the defect: a real Operation is
// claimed once per step of its chain, so a wedge that stays `running` is re-claimed on every round and
// starves the work behind it from the second round on. The two subtests at the end run the same scene
// with the wedge settled and with the wedge left running, so the test fails if the settlement stops
// making the difference it is supposed to make.
//
// The two tests after those ask the question the sequential ones cannot: whether a claim and a
// settlement overlapping on one row can produce a state neither writer intended. One stages the
// interleaving by hand so it is deterministic, and one runs both writers at once under `-race`.
//
// Nothing here mints an authorization, opens a socket, reads a token or runs Git. A-side preconditions
// (a live Project and its main Workspace, a connected Node, a reserved runtime control, a Controller
// lease) are staged by `seedCredentialedCloneScene` with direct SQL, as that helper documents.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/internal/cloneexec"
)

// terminalFact is the durable state a settlement writes, read straight from PostgreSQL: the public
// Operation shape strips the columns this boundary writes, and the assertions are about durable state
// rather than about a response.
type terminalFact struct {
	state, errorCode string
	retryAt          *time.Time
	version          int64
}

func readTerminalFact(t *testing.T, store *Store, operation string) terminalFact {
	t.Helper()
	var f terminalFact
	if err := store.Pool.QueryRow(
		`SELECT state,COALESCE(error_code,''),retry_at,version FROM operations WHERE id=$1`, operation,
	).
		Scan(&f.state, &f.errorCode, &f.retryAt, &f.version); err != nil {
		t.Fatalf("read operation %s: %v", operation, err)
	}
	return f
}

// settlementRequest is the control request a settlement is asked to act on: the request
// `scene.dispatch` builds, so a direct call to the settlement is the same call the dispatch boundary
// makes, with the epoch stated rather than implied.
func settlementRequest(s credentialedScene, epoch int64) *ControlRequest {
	return &ControlRequest{
		Action: "clone_dispatch",
		Body: Object{
			"epoch":       epoch,
			"operationId": s.operation,
			"executionId": "exec-settlement",
			"nodeId":      s.node,
			"input":       Object{"kind": "clone", "repositoryUrl": s.repository, "branch": s.ref},
		},
		Service: controllerClaims(),
	}
}

// refusal is the Fault the dispatch boundary hands the settlement when the guard refuses.
func refusal() error {
	return &Fault{Status: 409, Code: repositoryCredentialExecutorUnavailable, Params: Object{}}
}

// seedRefusedCredentialedWedge stages the scene, drives the one dispatch that is refused, and returns
// the scene with the refusal already recorded. Every test below starts from it, because the refusal is
// the precondition of a settlement and not something any of them is asking about.
func seedRefusedCredentialedWedge(t *testing.T, store *Store) credentialedScene {
	t.Helper()
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	if _, err := scene.dispatch(store, "exec-wedge"); err == nil {
		t.Fatal("this deployment installs no executor, so the dispatch must be refused")
	}
	return scene
}

// seedSettledQueueVictim offers one ordinary Operation on a Project of its own inside the tenant the
// credentialed scene established. `one_project_operation` allows a Project one live Operation, so a
// second waiting Operation needs a second Project — which is also the shape the starvation question is
// asked in.
func seedSettledQueueVictim(t *testing.T, store *Store, s credentialedScene, state, step string) (project, operation string) {
	t.Helper()
	project, operation = newID(), newID()
	workspace := newID()
	if _, err := store.transact(context.Background(), func(t *transaction) Object {
		t.exec(`INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,repository_url,default_branch,lifecycle)
			VALUES($1,$2,$3,$4,'victim','https://git.example.invalid/victim.git','main','active')`, project, s.tenant, s.user, s.space)
		t.exec(`INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref)
			VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready','main')`, workspace, s.tenant, s.user, project)
		t.exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash)
			VALUES($1,$2,$3,$4,'start',$5,$6,'{}',$7,$7)`, operation, s.tenant, s.user, project, state, step, "victim-"+operation)
		return Object{}
	}); err != nil {
		t.Fatalf("seed victim operation: %v", err)
	}
	return project, operation
}

// TestRefusedCredentialedCloneSettlesTheOperationTerminally is the settlement itself.
//
// A Controller reads the refusal as a retryable conflict and leaves the Operation `running`, so the
// refusal has to be made terminal somewhere, and the Operation is where Cloud keeps the durable fact
// about the work. The assertions are the whole terminal fact: the state, the stable reason code the
// public Operation contract already carries, and no retry deadline — `claim` moves a due retry to
// `running` without clearing the deadline it fired on, so a settled Operation that kept one would be
// read back as still waiting.
func TestRefusedCredentialedCloneSettlesTheOperationTerminally(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedRefusedCredentialedWedge(t, store)

	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "failed" {
		t.Fatalf("a permanently refused clone must settle the Operation as failed, got %q", fact.state)
	}
	if fact.errorCode != repositoryCredentialExecutorUnavailable {
		t.Fatalf("the Operation must carry the refusal's own code, got %q", fact.errorCode)
	}
	if fact.retryAt != nil {
		t.Fatalf("a terminal Operation must carry no retry deadline, got %v", fact.retryAt)
	}
	if fact.version != 2 {
		t.Fatalf("the settlement is one versioned write, want version 2, got %d", fact.version)
	}
	// No fabricated clone execution: a settled refusal is not a clone that ran, and a row claiming
	// otherwise would be a Node-visible execution a later recovery would try to run with no credential.
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
		t.Fatalf("a settled refusal registered %d clone executions", n)
	}
	// No bypass: the settlement records a refusal. It does not touch the binding it refused, the
	// Project's lifecycle, or the Workspace's admission — the user still has to act, and the credential
	// is still exactly the one the Project named.
	if n := scalarOf(t, store, "SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2 AND lifecycle='active'", scene.project, scene.credential); n != 1 {
		t.Fatal("the settlement rewrote the credential binding or the Project's lifecycle")
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM workspaces WHERE id=$1 AND admission_open=false AND observed_state='ready'", scene.workspace); n != 1 {
		t.Fatal("the settlement opened admission on a Workspace whose code was never cloned")
	}
}

// TestSettlementIgnoresEveryRefusalButThePermanentOnes pins the boundary of the change: only the
// reasons no actor can clear settle an Operation, and every other refusal keeps the retry behavior it
// always had.
//
// The rows are the ways a clone dispatch on a credentialed Project can be refused. A frozen credential
// is refused by the same guard, one step earlier, and an executor-side `credential_executor_unavailable`
// is a transport failure of a deployment that *did* install the boundary — the near-miss that a
// substring match on the code would wrongly settle.
func TestSettlementIgnoresEveryRefusalButThePermanentOnes(t *testing.T) {
	cases := []struct {
		name   string
		seed   func(t *testing.T, store *Store) (credentialedScene, *ControlRequest)
		cloner CredentialedCloner
		code   string
		reason string
	}{
		{
			name: "a frozen credential is refused before the executor is ever consulted",
			seed: func(t *testing.T, store *Store) (credentialedScene, *ControlRequest) {
				s := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
				if _, err := store.Pool.Exec(`UPDATE credential_refs SET availability='frozen' WHERE id=$1`, s.credential); err != nil {
					t.Fatalf("freeze the credential: %v", err)
				}
				return s, settlementRequest(s, 1)
			},
			code:   "repository_credential_frozen",
			reason: "a credential can be unfrozen, so the Operation must stay claimable",
		},
		{
			name: "a scope mismatch is a refusal about this request, not the deployment",
			seed: func(t *testing.T, store *Store) (credentialedScene, *ControlRequest) {
				s := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/other")
				return s, settlementRequest(s, 1)
			},
			code:   "repository_credential_scope_mismatch",
			reason: "a retryable refusal about this request must keep the Operation claimable",
		},
		{
			name: "an Operation that is not in its clone step is an ordinary conflict",
			seed: func(t *testing.T, store *Store) (credentialedScene, *ControlRequest) {
				s := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
				if _, err := store.Pool.Exec(`UPDATE operations SET step='cleanup' WHERE id=$1`, s.operation); err != nil {
					t.Fatalf("move the Operation past its clone step: %v", err)
				}
				return s, settlementRequest(s, 1)
			},
			code:   "dispatch_conflict",
			reason: "an unrelated conflict on the same Operation is not a capability absence",
		},
		{
			name: "an executor that cannot be reached is a transient failure, not a missing one",
			seed: func(t *testing.T, store *Store) (credentialedScene, *ControlRequest) {
				s := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
				return s, settlementRequest(s, 1)
			},
			cloner: &recordingCloner{err: &cloneexec.Failure{Code: cloneexec.ReasonCodeUnavailable}},
			code:   cloneexec.ReasonCodeUnavailable,
			reason: "the executor's own unavailable code differs from the guard's by one word and must never be settled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := dispatcherDB(t)
			scene, request := c.seed(t, store)
			if c.cloner != nil {
				store.SetCredentialedCloner(c.cloner)
			}

			_, err := store.Control(context.Background(), request)
			if err == nil {
				t.Fatalf("%s: the dispatch must be refused", c.reason)
			}
			if got := faultCode(t, err); got != c.code {
				t.Fatalf("%s: got %q, want %q", c.reason, got, c.code)
			}
			fact := readTerminalFact(t, store, scene.operation)
			if fact.state != "running" || fact.errorCode != "" {
				t.Fatalf("%s: only a refusal nothing can clear settles an Operation, got %s/%q", c.reason, fact.state, fact.errorCode)
			}
		})
	}
}

// TestAnUnsupportedCapabilitySettlesTheOperationToo is the second permanent refusal.
//
// A reference whose capability set does not permit the clone cannot be made to permit it: the column
// is written when the reference is created and no path updates it, and the Project's binding to the
// reference is likewise written once, at creation. Nothing a member or an operator can do through the
// product clears this refusal, so an Operation left claimable would hold the Project's single
// operation slot and keep it `provisioning` for good — the same wedge the deployment-level refusal is
// settled for, reached through a different door.
//
// The last two assertions are what keep the settlement a settlement: it writes the terminal fact and
// nothing else. No execution row is fabricated for a clone that never ran, and the binding it refused
// is left exactly as it was.
func TestAnUnsupportedCapabilitySettlesTheOperationToo(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	if _, err := store.Pool.Exec(`UPDATE credential_refs SET capabilities='[]'::jsonb WHERE id=$1`, scene.credential); err != nil {
		t.Fatalf("strip the reference's capability: %v", err)
	}

	_, err := scene.dispatch(store, "exec-capability-wedge")
	if got := faultCode(t, err); got != repositoryCredentialCapabilityUnsupported {
		t.Fatalf("refusal code = %q, want %q", got, repositoryCredentialCapabilityUnsupported)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a reference that permits no clone reached the executor %d times", len(cloner.calls))
	}
	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "failed" || fact.errorCode != repositoryCredentialCapabilityUnsupported {
		t.Fatalf("an unsupported capability must settle the Operation as failed, got %s/%q", fact.state, fact.errorCode)
	}
	if fact.retryAt != nil {
		t.Fatalf("a terminal Operation must carry no retry deadline, got %v", fact.retryAt)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
		t.Fatalf("a settled capability refusal registered %d clone executions", n)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2 AND lifecycle='active'", scene.project, scene.credential); n != 1 {
		t.Fatal("the settlement rewrote the credential binding or the Project's lifecycle")
	}
}

// TestARevokedReferenceSettlesTheOperationToo is the third permanent refusal, and the only one a
// member causes through the product. Deleting a credential is a revocation: the reference becomes
// `invalid` with `revoked_by_owner` and `revoked_at` and its token is destroyed in the Credential
// Service, and no path in the product writes that state back. So the same wedge the other two
// refusals are settled for applies here, and it applies to the state the product's own delete button
// produces rather than to one only an operator could stage.
//
// Settling it is what a deletion cannot do by itself: the Project stays bound to the reference —
// deleting a credential never re-points a binding — so every later dispatch of that Project meets the
// same refusal for as long as the binding exists, and without a settlement the Project's single
// operation slot stays occupied and it stays `provisioning` for good.
//
// The negative half is the frozen reference in the table above: a reference that is out of use but
// not destroyed must keep retrying, which is the difference between a state that can end and one that
// cannot.
func TestARevokedReferenceSettlesTheOperationToo(t *testing.T) {
	store := dispatcherDB(t)
	cloner := &recordingCloner{}
	store.SetCredentialedCloner(cloner)
	scene := seedCredentialedCloneScene(t, store, admittedRepository, admittedScope)
	// Exactly the three columns `DELETE /git-credentials/{id}` writes alongside destroying the token.
	stagedExec(t, store, `UPDATE credential_refs SET availability='invalid',unavailable_reason='revoked_by_owner',revoked_at=now() WHERE id=$1`, scene.credential)

	_, err := scene.dispatch(store, "exec-revoked-wedge")
	if got := faultCode(t, err); got != repositoryCredentialRevoked {
		t.Fatalf("refusal code = %q, want %q", got, repositoryCredentialRevoked)
	}
	if len(cloner.calls) != 0 {
		t.Fatalf("a revoked reference reached the executor %d times", len(cloner.calls))
	}
	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "failed" || fact.errorCode != repositoryCredentialRevoked {
		t.Fatalf("a revoked reference must settle the Operation as failed with its own code, got %s/%q", fact.state, fact.errorCode)
	}
	if fact.retryAt != nil {
		t.Fatalf("a terminal Operation must carry no retry deadline, got %v", fact.retryAt)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
		t.Fatalf("a settled revocation registered %d clone executions", n)
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM projects WHERE id=$1 AND credential_ref_id=$2 AND lifecycle='active'", scene.project, scene.credential); n != 1 {
		t.Fatal("the settlement rewrote the credential binding or the Project's lifecycle")
	}
}

// TestSettlementIgnoresActionsThatAreNotACloneDispatch pins the other half of the boundary. The
// settlement keys on the action as well as on the code, because the code is a property of the
// deployment and could be reported on a path that is not this Operation's clone admission.
func TestSettlementIgnoresActionsThatAreNotACloneDispatch(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedRefusedCredentialedWedge(t, store)
	// Put the scene back to the state the refusal left it in, so the only thing under test is whether a
	// non-dispatch action carrying the same code settles it.
	if _, err := store.Pool.Exec(`UPDATE operations SET state='running',error_code=NULL,version=1 WHERE id=$1`, scene.operation); err != nil {
		t.Fatalf("restore the scene: %v", err)
	}
	request := settlementRequest(scene, 1)
	request.Action = "effect_permit"

	if err := store.settleRefusedCredentialedClone(context.Background(), request, refusal()); err != nil {
		t.Fatalf("a settlement that does not apply must be a silent no-op: %v", err)
	}
	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "running" || fact.errorCode != "" {
		t.Fatalf("a refusal outside clone admission must not settle the Operation, got %s/%q", fact.state, fact.errorCode)
	}
}

// TestSettlementIsIdempotent pins that the settlement states a fact rather than applying a change:
// asked twice, the second call writes nothing. The condition is the Operation's own state, so a
// repeated settlement cannot produce a second, conflicting fact or bump a version a Controller is
// still holding.
func TestSettlementIsIdempotent(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedRefusedCredentialedWedge(t, store)
	first := readTerminalFact(t, store, scene.operation)

	request := settlementRequest(scene, 1)
	for round := range 3 {
		if err := store.settleRefusedCredentialedClone(context.Background(), request, refusal()); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	if again := readTerminalFact(t, store, scene.operation); again != first {
		t.Fatalf("a repeated settlement rewrote the terminal fact: %+v then %+v", first, again)
	}
}

// TestSettlementDoesNotOverwriteANewerController pins the fence. The refusal was observed under one
// Controller's epoch; a settlement that arrives after a successor claimed the Operation is stale
// evidence, and the successor — which would be refused and settle in its own turn — must not have its
// state written out from under it.
func TestSettlementDoesNotOverwriteANewerController(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedRefusedCredentialedWedge(t, store)
	// A successor claims the Operation: same step, newer epoch, and the state a claim leaves behind.
	if _, err := store.Pool.Exec(`UPDATE operations SET state='running',error_code=NULL,retry_at=NULL,controller_epoch=2,version=version+1 WHERE id=$1`, scene.operation); err != nil {
		t.Fatalf("hand the Operation to a successor: %v", err)
	}
	before := readTerminalFact(t, store, scene.operation)

	if err := store.settleRefusedCredentialedClone(context.Background(), settlementRequest(scene, 1), refusal()); err != nil {
		t.Fatalf("a stale settlement must be a silent no-op: %v", err)
	}
	if after := readTerminalFact(t, store, scene.operation); after != before {
		t.Fatalf("a settlement under a superseded epoch wrote over the successor: %+v then %+v", before, after)
	}
	// The successor's own refusal settles it, so the deferral is a fence and not a hole.
	if err := store.settleRefusedCredentialedClone(context.Background(), settlementRequest(scene, 2), refusal()); err != nil {
		t.Fatalf("the successor's own settlement: %v", err)
	}
	if after := readTerminalFact(t, store, scene.operation); after.state != "failed" || after.errorCode != repositoryCredentialExecutorUnavailable {
		t.Fatalf("the current Controller's settlement must land, got %s/%q", after.state, after.errorCode)
	}
}

// TestSettlementRefusesToWriteOnceTheBindingIsGone pins the fence that reads durable state rather
// than ownership: the refusal is about *this Project's* credential binding, so a settlement is only
// allowed to state it while that binding is still there.
//
// The window is real and short — the refusal and its settlement are two transactions of one Control
// call — but what it would write is not a race artifact, it is a wrong durable fact: unbinding the
// credential turns the Project back into an anonymous clone, which is a supported path that will
// dispatch and run. Settling that Operation as "no executor exists for your credential" would
// terminate work that no longer depends on a credential at all.
func TestSettlementRefusesToWriteOnceTheBindingIsGone(t *testing.T) {
	store := dispatcherDB(t)
	// Seeded directly, because the scene's own state *is* what a refusal leaves behind: the refusal
	// unwinds its transaction and records nothing, and a real dispatch would settle immediately rather
	// than leave a settlement to arrive late. What can arrive late is a second settlement — from a
	// concurrent Control call, or simply after the world moved — and that is what this test calls.
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	if before := readTerminalFact(t, store, scene.operation); before.state != "running" {
		t.Fatalf("the refused dispatch must leave the Operation where it was, got %q", before.state)
	}
	// The world moved between the refusal and the settlement: the Project names no credential now.
	if _, err := store.Pool.Exec(`UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project); err != nil {
		t.Fatalf("unbind the Project between the refusal and its settlement: %v", err)
	}
	before := readTerminalFact(t, store, scene.operation)

	if err := store.settleRefusedCredentialedClone(context.Background(), settlementRequest(scene, 1), refusal()); err != nil {
		t.Fatalf("a settlement that no longer applies must be a silent no-op: %v", err)
	}
	if after := readTerminalFact(t, store, scene.operation); after != before {
		t.Fatalf("a refusal about a credential binding settled an Operation that no longer names one: %+v then %+v", before, after)
	}
	// The Operation is still Cloud's to drive, and the anonymous path now admits it: the Project that
	// stopped naming a credential must be able to clone again rather than be terminated by the past.
	public := "https://git.example.invalid/group/public.git"
	if _, err := store.Pool.Exec(`UPDATE projects SET repository_url=$2 WHERE id=$1`, scene.project, public); err != nil {
		t.Fatalf("repoint the Project at a public repository: %v", err)
	}
	if _, err := store.Control(context.Background(), &ControlRequest{
		Action: "clone_dispatch",
		Body: Object{
			"epoch": 1, "operationId": scene.operation, "executionId": "exec-after-unbind",
			"nodeId": scene.node,
			"input":  Object{"kind": "clone", "repositoryUrl": public, "branch": scene.ref},
		},
		Service: controllerClaims(),
	}); err != nil {
		t.Fatalf("the anonymous clone must still dispatch: %v", err)
	}
	if state := readTerminalFact(t, store, scene.operation).state; state != "running" {
		t.Fatalf("an Operation the settlement declined to write must stay where the Controller left it, got %q", state)
	}
}

// TestSettlementRefusesToWriteOnceTheStepAdvanced pins the other conditional fence against the case
// the epoch alone cannot answer: an Operation re-claimed and carried past its clone admission under
// the *same* epoch.
//
// This is the interleaving that a fence keyed only on ownership would get wrong. `controller_epoch`
// moves on lease handover, not on every claim, so this Operation is still owned by the epoch whose
// dispatch was refused — and it has since advanced. The step is what makes the difference: `advance`
// walks `sandbox → node → clone → …` strictly forward and no transition returns to `clone`, so an
// Operation that got past its clone admission can never be at it again, and the refusal that names
// the clone step no longer describes any work this Operation has left to do.
func TestSettlementRefusesToWriteOnceTheStepAdvanced(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")
	// The clone succeeded on a deployment that had an executor, the Operation advanced to `cleanup`,
	// and the Controller re-claimed it — all still under epoch 1, which a claim does not change.
	if _, err := store.Pool.Exec(`UPDATE operations SET step='cleanup' WHERE id=$1`, scene.operation); err != nil {
		t.Fatalf("advance the Operation past its clone step: %v", err)
	}
	claimed, ok := claimOperation(t, store, 1)
	if !ok || claimed.S("id") != scene.operation {
		t.Fatalf("the advanced Operation must be the claimable one, got %v (claimed=%v)", claimed, ok)
	}
	if epoch := claimed.N("controllerEpoch"); epoch != 1 {
		t.Fatalf("a claim must not move the epoch, want 1, got %d", epoch)
	}
	before := readTerminalFact(t, store, scene.operation)
	if before.state != "running" {
		t.Fatalf("the re-claimed Operation must be running, got %q", before.state)
	}

	if err := store.settleRefusedCredentialedClone(context.Background(), settlementRequest(scene, 1), refusal()); err != nil {
		t.Fatalf("a settlement that no longer applies must be a silent no-op: %v", err)
	}
	if after := readTerminalFact(t, store, scene.operation); after != before {
		t.Fatalf("a refusal that names the clone step terminated an Operation past it: %+v then %+v", before, after)
	}
}

// TestSettledOperationLeavesTheClaimQueueAndFreesItsProject pins the two durable consequences the
// settlement exists for: Cloud stops handing the Operation to a Controller, and the Project's single
// live-Operation slot — which `one_project_operation` holds for `running` — is released, so the user
// can act on the Project again.
func TestSettledOperationLeavesTheClaimQueueAndFreesItsProject(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedRefusedCredentialedWedge(t, store)

	if claimed, ok := claimOperation(t, store, 1); ok {
		t.Fatalf("a settled Operation must not be handed to a Controller, got %s", claimed.S("id"))
	}
	if fact := readTerminalFact(t, store, scene.operation); fact.version != 2 {
		t.Fatalf("the claim must not have touched the settled Operation, want version 2, got %d", fact.version)
	}
	// The slot is free: a Project may carry one live Operation, and a `failed` one no longer counts.
	if _, err := store.Pool.Exec(`INSERT INTO operations(id,tenant_id,actor_user_id,project_id,kind,state,step,request,idempotency_key,request_hash)
		VALUES($1,$2,$3,$4,'delete_project','queued','cleanup','{}','after-settlement','after-settlement')`,
		newID(), scene.tenant, scene.user, scene.project); err != nil {
		t.Fatalf("the settled Operation still occupies the Project's operation slot: %v", err)
	}
}

// TestASettledOperationNeverTakesAClaimFromAnotherProject is the end-to-end consequence, and the test
// the previous round's fix did not have.
//
// A real Operation is claimed once per step of its chain, so "the waiting work is preferred over the
// running work" only decides the first round: from the second round on, a wedge that is still `running`
// is the older of two running Operations and wins every claim, and the work behind it never advances
// past its first step. The same scene is run twice — with the wedge settled and with the wedge left
// running as the previous fix left it — so the difference the settlement makes is asserted rather than
// assumed, and the victim is driven through every step of its chain instead of one.
func TestASettledOperationNeverTakesAClaimFromAnotherProject(t *testing.T) {
	// The victim's own chain, in the order the store advances it. The last entry is terminal and is
	// where a completed provisioning run ends.
	chain := []string{"sandbox", "node", "clone", "cleanup", "done"}

	t.Run("settled, the victim is claimed at every step", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := seedRefusedCredentialedWedge(t, store)
		_, victim := seedSettledQueueVictim(t, store, scene, "queued", chain[0])
		before := readTerminalFact(t, store, scene.operation).version

		reached := []string{}
		for step := range chain[:len(chain)-1] {
			claimed, ok := claimOperation(t, store, 1)
			if !ok {
				t.Fatalf("step %d (%s): the victim was still waiting and the claim offered nothing", step, chain[step])
			}
			reached = append(reached, claimed.S("id"))
			if _, err := store.Pool.Exec(`UPDATE operations SET step=$2 WHERE id=$1`, victim, chain[step+1]); err != nil {
				t.Fatalf("advance the victim to %s: %v", chain[step+1], err)
			}
		}
		for step, id := range reached {
			if id != victim {
				t.Fatalf("step %d (%s): the claim must go to the victim, got %s", step, chain[step], id)
			}
		}
		if after := readTerminalFact(t, store, scene.operation); after.version != before {
			t.Fatalf("a settled Operation must never take a claim, version moved %d to %d", before, after.version)
		}
	})

	t.Run("still running, the victim advances one step and stops", func(t *testing.T) {
		store := dispatcherDB(t)
		scene := seedRefusedCredentialedWedge(t, store)
		// Exactly the state the previous scheduling fix left a refused Operation in: running, at the
		// clone step, and a claim candidate for as long as it stays there.
		if _, err := store.Pool.Exec(`UPDATE operations SET state='running',error_code=NULL,version=1 WHERE id=$1`, scene.operation); err != nil {
			t.Fatalf("restore the unsettled wedge: %v", err)
		}
		_, victim := seedSettledQueueVictim(t, store, scene, "queued", chain[0])

		first, ok := claimOperation(t, store, 1)
		if !ok || first.S("id") != victim {
			t.Fatalf("the first claim must go to the waiting victim, got %v", first)
		}
		for step := 1; step < len(chain)-1; step++ {
			claimed, ok := claimOperation(t, store, 1)
			if !ok {
				t.Fatalf("step %d (%s): the claim offered nothing", step, chain[step])
			}
			if claimed.S("id") != scene.operation {
				t.Fatalf("step %d (%s): an unsettled wedge is the older running Operation and wins the claim, got %s", step, chain[step], claimed.S("id"))
			}
		}
		// The defect the settlement removes, recorded here so the pair above is a difference and not two
		// green tests: without it the victim is claimed once and never again, so it never finishes.
		if v := operationVersion(t, store, victim); v != 2 {
			t.Fatalf("the starved victim must have been claimed exactly once, got version %d", v)
		}
	})
}

// TestAnonymousCloneIsNotSettled is the negative control for the whole change. A Project that names no
// credential never reaches the guard, so nothing about it may become terminal: the dispatch registers
// an execution for a Node to run, and the Operation stays where the Controller put it.
func TestAnonymousCloneIsNotSettled(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/public.git", "")
	if _, err := store.Pool.Exec(`UPDATE projects SET credential_ref_id=NULL WHERE id=$1`, scene.project); err != nil {
		t.Fatalf("unbind the Project: %v", err)
	}

	out, err := scene.dispatch(store, "exec-anonymous")
	if err != nil {
		t.Fatalf("an anonymous clone must still dispatch: %v", err)
	}
	if out.S("executionId") != "exec-anonymous" {
		t.Fatalf("the ordinary dispatch must register the execution: %v", out)
	}
	if result := out.O("result"); len(result) != 0 {
		t.Fatalf("an anonymous clone must wait for the Node's result, got %v", result)
	}
	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "running" || fact.errorCode != "" {
		t.Fatalf("an anonymous clone must leave its Operation untouched, got %s/%q", fact.state, fact.errorCode)
	}
}

// TestSettlementLandsUnderAClaimThatMovedTheVersion pins, deterministically, the interleaving a
// version-fenced settlement would have got wrong.
//
// The claim runs between the refusal and the settlement. It picks the very Operation that was refused
// — still `running`, still in its clone step — and advances the row's `version` on the way through,
// exactly as a Controller's next round would. The settlement must still land.
//
// That is why its WHERE clause fences on `controller_epoch` rather than on the version. The refusal
// came from a dispatch this epoch owns, and a claim by the *same* epoch does not make the missing
// executor any less permanent. A settlement keyed on the version would quietly match nothing here, and
// the Operation would go on occupying the Project's only live-Operation slot — the defect it exists to
// remove, restored by a race that a passing sequential test would never show.
func TestSettlementLandsUnderAClaimThatMovedTheVersion(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")

	// The scene is seeded `running`/`clone` under epoch 1, which is the state a refused dispatch leaves
	// behind: the refusal unwound its own transaction and recorded nothing.
	if before := readTerminalFact(t, store, scene.operation); before.state != "running" {
		t.Fatalf("the refused dispatch must leave the Operation where it was, got %q", before.state)
	}
	claimed, ok := claimOperation(t, store, 1)
	if !ok || claimed.S("id") != scene.operation {
		t.Fatalf("the scene's Operation must be the claimable one, got %v (claimed=%v)", claimed, ok)
	}
	if moved := readTerminalFact(t, store, scene.operation); moved.version != 2 {
		t.Fatalf("the claim must have advanced the version the settlement is about to write under, got %d", moved.version)
	}

	if err := store.settleRefusedCredentialedClone(context.Background(), settlementRequest(scene, 1), refusal()); err != nil {
		t.Fatalf("the settlement must be writable after a claim moved the version: %v", err)
	}

	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "failed" || fact.errorCode != repositoryCredentialExecutorUnavailable {
		t.Fatalf("a claim that moved the version must not stop the settlement: %+v", fact)
	}
	if fact.version != 3 {
		t.Fatalf("the settlement is one versioned write after the claim, want version 3, got %d", fact.version)
	}
}

// TestConcurrentClaimsAndSettlementAgreeOnTheOutcome asks the same question under contention, which is
// where the refusals are actually produced: a Controller claims on a timer while another one's dispatch
// is being refused and settled, so the two writers of one row really do overlap.
//
// The assertions are the complete permitted set rather than one happy path, because the interesting
// failure is not a wrong answer to a specific interleaving but a state neither writer intended:
//
//   - A claim either hands back an Operation that is `running` — the only state a claim can hand back,
//     since a claim's own UPDATE is the only thing that makes an Operation claimable — or it hands back
//     nothing. It can never hand back the settled Operation: that one is `failed`.
//   - The settlement lands whatever the claims did to the version on the way, and it clears the retry
//     deadline, so no claim can read the Operation back as still waiting.
//   - Neither writer fabricated a clone execution for a refusal.
//
// The run is meaningful together with `-race`: the invariants say what must hold, and the detector says
// whether the two writers touched one row's state without synchronizing.
func TestConcurrentClaimsAndSettlementAgreeOnTheOutcome(t *testing.T) {
	store := dispatcherDB(t)
	scene := seedCredentialedCloneScene(t, store, "https://git.example.invalid/group/private.git", "https://git.example.invalid/group/private")

	// `t.Fatalf` may only be called from the test goroutine, so a claimer's failure travels as a value.
	type claimOutcome struct {
		operation Object
		claimed   bool
		err       error
	}

	const claimers, rounds = 4, 25
	start := make(chan struct{})
	outcomes := make(chan claimOutcome, claimers*rounds)
	var wg sync.WaitGroup
	for range claimers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range rounds {
				out, err := store.Control(context.Background(), &ControlRequest{
					Action: "claim", Body: Object{"epoch": 1}, Service: controllerClaims(),
				})
				if err != nil {
					outcomes <- claimOutcome{err: err}
					return
				}
				value, present := out["operation"]
				outcomes <- claimOutcome{operation: out.O("operation"), claimed: present && value != nil}
			}
		}()
	}

	// The claimers are released first, so their rounds overlap the dispatch rather than following it.
	close(start)
	if _, err := scene.dispatch(store, "exec-concurrent"); err == nil {
		t.Fatal("this deployment installs no executor, so the dispatch must be refused")
	}
	wg.Wait()
	close(outcomes)

	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatalf("a claim conflicted while a settlement ran: %v", outcome.err)
		}
		if !outcome.claimed {
			continue
		}
		if state := outcome.operation.S("state"); state != "running" {
			t.Fatalf("a claim handed back an Operation in state %q", state)
		}
	}

	fact := readTerminalFact(t, store, scene.operation)
	if fact.state != "failed" || fact.errorCode != repositoryCredentialExecutorUnavailable {
		t.Fatalf("a concurrent claim must not prevent or undo the settlement: %+v", fact)
	}
	if fact.retryAt != nil {
		t.Fatal("the settled Operation kept a retry deadline a concurrent claim could read back")
	}
	if n := scalarOf(t, store, "SELECT count(*) FROM clone_executions WHERE operation_id=$1", scene.operation); n != 0 {
		t.Fatalf("claims racing a settlement registered %d clone executions", n)
	}
	// The settled Operation left the queue, so the claim that outlives the settlement has nothing left
	// to hand back. Any other Operation in this schema is a different Project's, and allowed.
	if claimed, ok := claimOperation(t, store, 1); ok && claimed.S("id") == scene.operation {
		t.Fatal("a settled Operation was still claimable after the settlement landed")
	}
}
