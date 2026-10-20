package core

// Claim-time settlement of deterministic repository refusals and last_claimed_at rotation
// (specs decisions/cloud/operation/20261014-settle-deterministic-dispatch-refusals.md D2/D3).
//
// Every scene runs on a real isolated PostgreSQL schema through Store.Control's shipped `claim`
// action, mirroring the production Controller path: no operation state is written directly by the
// assertions themselves. Deterministic refusal settlement must free the queue head for other
// projects, settle the run Workspace through the same-transaction hook, and never touch candidates
// that already carry registered executions; rotation must hand the next opportunity to the
// candidate that consumed the fewest claims.

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// claimOnce drives the shipped claim action with the shared controller principal and lease.
func claimOnce(t *testing.T, store *Store) Object {
	t.Helper()
	out, err := store.Control(context.Background(), &ControlRequest{
		Action: "claim", Body: Object{"epoch": 1}, Service: controllerClaims(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	return out
}

// claimCredentialState names the seeded repository credential of one scene; "" seeds an anonymous
// project (the existing public Git execution path).
type claimCredentialState = string

const (
	claimAnonymous     = ""
	claimFrozen        = "frozen"
	claimDeleted       = "deleted"
	claimScopeMismatch = "scope"
	claimTeamAvailable = "team"
)

// claimScene is one seeded project with its main Workspace plus a fresh isolated Workspace whose
// queued create_workspace operation sits at the clone step.
type claimScene struct {
	tenant, project, workspace, operation, actor string
}

// seedClaimScene loads one schema-valid scene in a single transaction. credential selects the
// repository credential state; run additionally binds the isolated Workspace to a provisioning
// agent run so the settlement hook path is exercised; createdAt pins the operation's queue
// position for deterministic rotation.
func seedClaimScene(t *testing.T, store *Store, credential claimCredentialState, run bool, createdAt string) claimScene {
	t.Helper()
	scene := claimScene{tenant: uuid.NewString(), project: uuid.NewString(), workspace: uuid.NewString()}
	user := uuid.NewString()
	scene.actor = user
	space := uuid.NewString()
	main := uuid.NewString()
	tx, err := store.Pool.Begin()
	if err != nil {
		t.Fatalf("seed begin: %v", err)
	}
	exec := func(name, q string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(q, args...); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	exec("user", `INSERT INTO users(id,display_name,status) VALUES($1,'claim','active')`, user)
	exec("tenant", `INSERT INTO tenants(id,name,status) VALUES($1,'claim','active')`, scene.tenant)
	exec("membership", `INSERT INTO tenant_memberships(tenant_id,user_id,role,status) VALUES($1,$2,'admin','active')`, scene.tenant, user)
	exec("space", `INSERT INTO collab_workspaces(id,tenant_id,name,slug,created_by) VALUES($1,$2,'claim','claim-'||left($4::text,8),$3)`, space, scene.tenant, user, space)
	var cred any
	if credential != claimAnonymous {
		cid := uuid.NewString()
		// Since 0043 every reference points into the Credential Store: a stored version scoped to
		// one repository. Freezing, revocation and soft deletion are audit states on that same
		// reference, so the scene never needs a deployment-preset secret reference.
		availability, scope := "available", any("https://example.invalid/claim.git")
		secretState, storeVersion := "stored", any(1)
		var deletedAt, revokedAt any
		switch credential {
		case claimFrozen:
			availability = "frozen"
		case claimDeleted:
			availability, secretState, storeVersion, scope = "frozen", "revoked", 0, nil
			revokedAt, deletedAt = time.Now(), time.Now()
		case claimScopeMismatch:
			scope = "https://example.invalid/other.git"
		}
		exec("credential", `INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,name,attribution,attribution_basis,availability,repository_scope,secret_state,store_version,revoked_at,deleted_at) VALUES($1,$2,$3,'git','claim credential','team','test fixture',$4,$5,$6,$7,$8,$9)`, cid, scene.tenant, user, availability, scope, secretState, storeVersion, revokedAt, deletedAt)
		cred = cid
	}
	exec("project", `INSERT INTO projects(id,tenant_id,owner_user_id,space_id,name,lifecycle) VALUES($1,$2,$3,$4,'claim','active')`, scene.project, scene.tenant, user, space)
	exec("binding", `INSERT INTO repository_bindings(id,tenant_id,project_id,repository_url,branch,credential_ref_id) VALUES($1,$2,$3,'https://example.invalid/claim.git','main',$4)`, uuid.NewString(), scene.tenant, scene.project, cred)
	exec("main-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,runtime_generation,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'main','running','ready',1,'main')`, main, scene.tenant, user, scene.project)
	var issueRun any
	if run {
		issue, agent := uuid.NewString(), uuid.NewString()
		issueRun = uuid.NewString()
		exec("issue", `INSERT INTO issues(id,tenant_id,creator_user_id,project_ref,title,number) VALUES($1,$2,$3,$4,'claim issue',1)`, issue, scene.tenant, user, scene.project)
		exec("run", `INSERT INTO issue_runs(id,tenant_id,issue_id,executor_type,executor_id,status,phase) VALUES($1,$2,$3,'agent',$4,'dispatched','provisioning')`, issueRun, scene.tenant, issue, agent)
	}
	exec("isolated-workspace", `INSERT INTO workspaces(id,tenant_id,owner_user_id,creator_user_id,creator_evidence,project_id,kind,desired_state,observed_state,requested_ref) VALUES($1,$2,$3,$3,'verified_request',$4,'isolated','running','provisioning','main')`, scene.workspace, scene.tenant, user, scene.project)
	exec("task", `INSERT INTO tasks(id,workspace_id,title) VALUES($1,$2,'claim')`, uuid.NewString(), scene.workspace)
	if run {
		// createRunWorkspace breaks the workspace↔/run reference cycle exactly this way.
		exec("bind-run", `UPDATE workspaces SET issue_run_id=$2 WHERE id=$1`, scene.workspace, issueRun)
	}
	scene.operation = uuid.NewString()
	exec("operation", `INSERT INTO operations(id,tenant_id,actor_user_id,project_id,workspace_id,kind,state,step,request,idempotency_key,request_hash,created_at) VALUES($1,$2,$3,$4,$5,'create_workspace','queued','clone','{}',$6,$7,$8::timestamptz)`, scene.operation, scene.tenant, user, scene.project, scene.workspace, "claim-"+scene.operation, "claim-hash-"+scene.operation, createdAt)
	if run {
		// reserveRuntimeMaintenance's durable row: the create kind holds runtime maintenance until
		// its terminal transaction hands it back.
		exec("runtime-control", `INSERT INTO runtime_controls(workspace_id,state,maintenance_operation_id) VALUES($1,'maintenance',$2)`, scene.workspace, scene.operation)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return scene
}

// seedCloneExecution registers one clone execution of the scene's operation with the given result
// (nil result leaves the execution unsettled).
func seedCloneExecution(t *testing.T, store *Store, scene claimScene, execution string, result any) {
	t.Helper()
	var res any
	if result != nil {
		res = result
	}
	if _, err := store.Pool.Exec(`INSERT INTO clone_executions(execution_id,operation_id,workspace_id,node_id,input,dispatched_epoch,node_operation_id,result) VALUES($1,$2,$3,'claim-node','{}'::jsonb,1,$1,$4)`, execution, scene.operation, scene.workspace, res); err != nil {
		t.Fatalf("seed clone execution: %v", err)
	}
}

// claimOperationState reads the persisted operation row the assertions talk about.
func claimOperationState(t *testing.T, store *Store, id string) (state, code string, retryAt sql.NullString, version int64) {
	t.Helper()
	err := store.Pool.QueryRow(`SELECT state,COALESCE(error_code,''),retry_at,version FROM operations WHERE id=$1`, id).Scan(&state, &code, &retryAt, &version)
	if err != nil {
		t.Fatalf("read operation %s: %v", id, err)
	}
	return state, code, retryAt, version
}

func TestClaimSettlesEachDeterministicRefusalCode(t *testing.T) {
	cases := []struct {
		name       string
		credential claimCredentialState
		code       string
	}{
		{"frozen credential", claimFrozen, "repository_credential_frozen"},
		{"deleted credential", claimDeleted, "repository_credential_unavailable"},
		{"scope mismatch", claimScopeMismatch, "repository_credential_scope_mismatch"},
		// Main has no production executor for project references yet: an otherwise valid team
		// credential is still the closed-executor-boundary deterministic refusal.
		{"available team credential without executor", claimTeamAvailable, "repository_credential_executor_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := dispatcherDB(t)
			seedControllerLease(t, store)
			scene := seedClaimScene(t, store, tc.credential, false, "2026-01-01 00:00:01+00")
			claimOnce(t, store)
			state, code, retryAt, version := claimOperationState(t, store, scene.operation)
			if state != "failed" || code != tc.code {
				t.Fatalf("operation settled as (%s,%s), want (failed,%s)", state, code, tc.code)
			}
			if retryAt.Valid {
				t.Fatalf("settled operation keeps retry_at=%s, want NULL", retryAt.String)
			}
			// The settlement is one write, so a Controller still holding the pre-claim snapshot is
			// fenced by the version it no longer matches.
			if version != 2 {
				t.Fatalf("settled operation version %d, want the settlement's single increment to 2", version)
			}
			var observed, admission bool
			if err := store.Pool.QueryRow(`SELECT observed_state='unavailable',admission_open FROM workspaces WHERE id=$1`, scene.workspace).Scan(&observed, &admission); err != nil {
				t.Fatalf("read workspace: %v", err)
			}
			if !observed || admission {
				t.Fatalf("workspace after settlement: unavailable=%v admission_open=%v, want true/false", observed, admission)
			}
		})
	}
}

func TestClaimRefusalFreesQueueHeadForOtherProjects(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	// The refused candidate is older: under the previous created_at order it owned the queue head.
	refused := seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:01+00")
	healthy := seedClaimScene(t, store, claimAnonymous, false, "2026-01-01 00:00:02+00")

	out := claimOnce(t, store)
	if got := out.O("operation").S("id"); got != healthy.operation {
		t.Fatalf("claim returned operation %s, want the healthy project's %s", got, healthy.operation)
	}
	if state, code, _, _ := claimOperationState(t, store, refused.operation); state != "failed" || code != "repository_credential_frozen" {
		t.Fatalf("refused operation settled as (%s,%s), want (failed,repository_credential_frozen)", state, code)
	}
	if state, _, _, _ := claimOperationState(t, store, healthy.operation); state != "running" {
		t.Fatalf("healthy operation state %s, want running", state)
	}
	// The settled operation is terminal: the next claim keeps serving the healthy candidate.
	out = claimOnce(t, store)
	if got := out.O("operation").S("id"); got != healthy.operation {
		t.Fatalf("second claim returned operation %s, want the healthy project's %s again", got, healthy.operation)
	}
}

func TestClaimRotatesByLastClaimedAt(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	first := seedClaimScene(t, store, claimAnonymous, false, "2026-01-01 00:00:01+00")
	second := seedClaimScene(t, store, claimAnonymous, false, "2026-01-01 00:00:02+00")

	want := []string{first.operation, second.operation, first.operation, second.operation}
	for i, id := range want {
		out := claimOnce(t, store)
		if got := out.O("operation").S("id"); got != id {
			t.Fatalf("claim %d returned operation %s, want %s", i+1, got, id)
		}
	}
}

func TestClaimRefusalPreservesRegisteredAndNonCloneWork(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)

	succeeded := seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:01+00")
	seedCloneExecution(t, store, succeeded, "claim-ready", `{"outcome":"clone_ready","commit":"0000000000000000000000000000000000000001"}`)

	unsettled := seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:02+00")
	seedCloneExecution(t, store, unsettled, "claim-open", nil)

	// A sandbox-step candidate does no new remote Git work: the guard never applies to it.
	sandbox := seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:03+00")
	if _, err := store.Pool.Exec(`UPDATE operations SET step='sandbox' WHERE id=$1`, sandbox.operation); err != nil {
		t.Fatalf("stage sandbox candidate: %v", err)
	}

	for _, scene := range []claimScene{succeeded, unsettled, sandbox} {
		out := claimOnce(t, store)
		if got := out.O("operation").S("id"); got != scene.operation {
			t.Fatalf("claim returned operation %s, want candidate %s handed back untouched", got, scene.operation)
		}
		if state, code, _, _ := claimOperationState(t, store, scene.operation); state != "running" || code != "" {
			t.Fatalf("candidate %s settled as (%s,%s), want (running,) — registered work and non-clone steps are never refused in claim", scene.operation, state, code)
		}
	}
}

func TestClaimSettlesRetryWaitCandidateAfterSettledCloneFailure(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	scene := seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:01+00")
	// One settled failure already parked the operation for retry; the retry's new clone is the
	// remote Git work the frozen credential now deterministically refuses.
	seedCloneExecution(t, store, scene, "claim-failed", `{"outcome":"clone_failed"}`)
	if _, err := store.Pool.Exec(`UPDATE operations SET state='retry_wait',error_code='clone_failed',retry_at=clock_timestamp()-interval '1 minute' WHERE id=$1`, scene.operation); err != nil {
		t.Fatalf("park candidate: %v", err)
	}

	claimOnce(t, store)
	state, code, retryAt, _ := claimOperationState(t, store, scene.operation)
	if state != "failed" || code != "repository_credential_frozen" || retryAt.Valid {
		t.Fatalf("retry_wait candidate settled as (%s,%s,%s), want (failed,repository_credential_frozen,NULL)", state, code, retryAt.String)
	}
}

func TestClaimRefusalSettlesRunWorkspaceInSameTransaction(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	scene := seedClaimScene(t, store, claimFrozen, true, "2026-01-01 00:00:01+00")

	out := claimOnce(t, store)
	if got := out.O("operation").S("id"); got == scene.operation {
		t.Fatal("claim handed back the settled operation itself")
	}
	state, code, _, _ := claimOperationState(t, store, scene.operation)
	if state != "failed" || code != "repository_credential_frozen" {
		t.Fatalf("run operation settled as (%s,%s), want (failed,repository_credential_frozen)", state, code)
	}
	var phase, status, reason string
	if err := store.Pool.QueryRow(`SELECT r.phase,r.status,COALESCE(r.failure_reason,'') FROM issue_runs r WHERE r.id=(SELECT issue_run_id FROM workspaces WHERE id=$1)`, scene.workspace).Scan(&phase, &status, &reason); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if phase != "releasing" || status != "failed" || reason != "workspace_unavailable" {
		t.Fatalf("run settled as (%s,%s,%s), want (releasing,failed,workspace_unavailable)", phase, status, reason)
	}
	var control, holder string
	if err := store.Pool.QueryRow(`SELECT state,COALESCE(maintenance_operation_id::text,'') FROM runtime_controls WHERE workspace_id=$1`, scene.workspace).Scan(&control, &holder); err != nil {
		t.Fatalf("read runtime control: %v", err)
	}
	// The follow-up delete operation may legitimately re-reserve maintenance; the settled create
	// operation itself must no longer hold it.
	if holder == scene.operation {
		t.Fatalf("runtime control (%s) still holds maintenance for the settled operation", control)
	}
}

func TestClaimRefusalBudgetReturnsEmptyAndNextPeriodContinues(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	// claimCandidates refused candidates consume the whole budget; the healthy candidate behind
	// them waits for the next claim period instead of growing this transaction.
	for i := 0; i < claimCandidates; i++ {
		seedClaimScene(t, store, claimFrozen, false, "2026-01-01 00:00:01+00")
	}
	healthy := seedClaimScene(t, store, claimAnonymous, false, "2026-01-02 00:00:00+00")

	out := claimOnce(t, store)
	if out["operation"] != nil {
		t.Fatalf("budgeted claim returned an operation, want empty")
	}
	var settled int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM operations WHERE state='failed' AND error_code='repository_credential_frozen'`).Scan(&settled); err != nil {
		t.Fatalf("count settled: %v", err)
	}
	if settled != claimCandidates {
		t.Fatalf("first claim settled %d operations, want %d", settled, claimCandidates)
	}

	out = claimOnce(t, store)
	if got := out.O("operation").S("id"); got != healthy.operation {
		t.Fatalf("second claim returned operation %s, want the healthy project's %s", got, healthy.operation)
	}
}

func TestClaimRefusalRollsBackWhenTheRunSettlementHookFails(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	scene := seedClaimScene(t, store, claimFrozen, true, "2026-01-01 00:00:01+00")
	store.OnRunWorkspaceSettled = func(context.Context, *sql.Tx, string, string) error {
		return errors.New("run settlement hook failed")
	}

	// D2 settlement is one transaction: a failing run settlement hook must not leave the operation
	// failed, the Workspace unavailable, or the runtime maintenance reservation handed back.
	if _, err := store.Control(context.Background(), &ControlRequest{Action: "claim", Body: Object{"epoch": 1}, Service: controllerClaims()}); err == nil {
		t.Fatal("claim committed although the run settlement hook failed")
	}
	state, code, retryAt, version := claimOperationState(t, store, scene.operation)
	if state != "queued" || code != "" || retryAt.Valid || version != 1 {
		t.Fatalf("failed hook left operation (%s,%s,%s,v%d), want the original queued row", state, code, retryAt.String, version)
	}
	var observed string
	var admission bool
	if err := store.Pool.QueryRow(`SELECT observed_state,admission_open FROM workspaces WHERE id=$1`, scene.workspace).Scan(&observed, &admission); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if observed != "provisioning" || admission {
		t.Fatalf("failed hook left workspace (%s,%v), want provisioning/false", observed, admission)
	}
	var holder string
	if err := store.Pool.QueryRow(`SELECT COALESCE(maintenance_operation_id::text,'') FROM runtime_controls WHERE workspace_id=$1`, scene.workspace).Scan(&holder); err != nil {
		t.Fatalf("read runtime control: %v", err)
	}
	if holder != scene.operation {
		t.Fatalf("failed hook released runtime maintenance to %q, want the unsettled operation", holder)
	}
}

func TestClaimSettlesCredentialFreezeBetweenClaimAndDispatch(t *testing.T) {
	store := dispatcherDB(t)
	seedControllerLease(t, store)
	scene := seedClaimScene(t, store, claimAnonymous, false, "2026-01-01 00:00:01+00")
	if out := claimOnce(t, store); out.O("operation").S("id") != scene.operation {
		t.Fatalf("claim returned %s, want the only candidate %s", out.O("operation").S("id"), scene.operation)
	}

	// The Project gains a frozen scoped credential after the claim returned and before any clone is
	// dispatched. D1 still classifies it, and the next authoritative claim settles the running
	// candidate instead of handing back an operation whose dispatch could only be refused again.
	credential := uuid.NewString()
	if _, err := store.Pool.Exec(`INSERT INTO credential_refs(id,tenant_id,owner_user_id,purpose,name,attribution,attribution_basis,availability,repository_scope,secret_state,store_version) VALUES($1,$2,$3,'git','late credential','team','test fixture','frozen','https://example.invalid/claim.git','stored',1)`, credential, scene.tenant, scene.actor); err != nil {
		t.Fatalf("seed late credential: %v", err)
	}
	if _, err := store.Pool.Exec(`UPDATE repository_bindings SET credential_ref_id=$2,version=version+1 WHERE project_id=$1`, scene.project, credential); err != nil {
		t.Fatalf("bind late credential: %v", err)
	}

	out := claimOnce(t, store)
	if out["operation"] != nil {
		t.Fatalf("claim returned an operation after the credential froze, want empty")
	}
	state, code, retryAt, _ := claimOperationState(t, store, scene.operation)
	if state != "failed" || code != "repository_credential_frozen" || retryAt.Valid {
		t.Fatalf("late refusal settled as (%s,%s,%s), want (failed,repository_credential_frozen,NULL)", state, code, retryAt.String)
	}
	var executions int
	if err := store.Pool.QueryRow(`SELECT count(*) FROM clone_executions WHERE operation_id=$1`, scene.operation).Scan(&executions); err != nil {
		t.Fatalf("count clone executions: %v", err)
	}
	if executions != 0 {
		t.Fatalf("settled operation registered %d clone executions, want none", executions)
	}
}
